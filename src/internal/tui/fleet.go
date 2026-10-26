package tui

import (
	"sort"
	"strings"
	"time"

	v1 "github.com/squash-box/floatilla/src/gen/go/floatilla/v1"
)

const (
	// Done tiles age out of their lane after this (SPEC §4.5).
	doneTTL = 30 * time.Minute
	// Failed tiles linger longer: they still need a human.
	failedTTL = 2 * time.Hour
	// A lane collapses after being empty this long.
	collapseAfter = 30 * time.Second
)

// statusRank is the "needs you first" sort order (SPEC §2.2), with parked
// (waiting on an external event) between running and paused.
var statusRank = map[v1.TaskStatus]int{
	v1.TaskStatus_TASK_STATUS_WAITING:   0,
	v1.TaskStatus_TASK_STATUS_FAILED:    1,
	v1.TaskStatus_TASK_STATUS_RUNNING:   2,
	v1.TaskStatus_TASK_STATUS_PARKED:    3,
	v1.TaskStatus_TASK_STATUS_PAUSED:    4,
	v1.TaskStatus_TASK_STATUS_QUEUED:    5,
	v1.TaskStatus_TASK_STATUS_SUCCEEDED: 6,
}

// countOrder is the order of per-status counts in headers.
var countOrder = []v1.TaskStatus{
	v1.TaskStatus_TASK_STATUS_RUNNING,
	v1.TaskStatus_TASK_STATUS_WAITING,
	v1.TaskStatus_TASK_STATUS_FAILED,
	v1.TaskStatus_TASK_STATUS_SUCCEEDED,
	v1.TaskStatus_TASK_STATUS_PARKED,
	v1.TaskStatus_TASK_STATUS_PAUSED,
	v1.TaskStatus_TASK_STATUS_QUEUED,
}

// Fleet is the client-side view of all tasks, grouped into lanes.
type Fleet struct {
	Types      []*v1.ProcessTypeSpec
	tasks      map[string]*v1.Task
	emptySince map[string]time.Time
	// manualOpen: lanes expanded by hand stay open until an agent arrives;
	// from then on they collapse normally when they next empty.
	manualOpen map[string]bool
}

func NewFleet() *Fleet {
	return &Fleet{
		tasks:      map[string]*v1.Task{},
		emptySince: map[string]time.Time{},
		manualOpen: map[string]bool{},
	}
}

// Load replaces the fleet with a snapshot. Lanes that start empty count as
// empty long enough, so the first frame doesn't wait 30 s to collapse.
func (f *Fleet) Load(types []*v1.ProcessTypeSpec, tasks []*v1.Task, now time.Time) {
	f.Types = types
	f.tasks = map[string]*v1.Task{}
	for _, t := range tasks {
		f.tasks[t.Id] = t
	}
	f.emptySince = map[string]time.Time{}
	f.Tick(now)
	for lane, since := range f.emptySince {
		if since.Equal(now) {
			f.emptySince[lane] = now.Add(-collapseAfter)
		}
	}
}

func (f *Fleet) Upsert(t *v1.Task) {
	if cur, ok := f.tasks[t.Id]; ok && t.UpdatedAt != nil && cur.UpdatedAt != nil && t.UpdatedAt.AsTime().Before(cur.UpdatedAt.AsTime()) {
		return // stale snapshot
	}
	f.tasks[t.Id] = t
}

func (f *Fleet) Task(id string) *v1.Task { return f.tasks[id] }

func (f *Fleet) Type(name string) *v1.ProcessTypeSpec {
	for _, s := range f.Types {
		if s.Name == name {
			return s
		}
	}
	return nil
}

// Visible reports whether a task holds a tile in its lane.
func Visible(t *v1.Task, now time.Time) bool {
	switch t.Status {
	case v1.TaskStatus_TASK_STATUS_SUCCEEDED:
		return t.FinishedAt != nil && now.Sub(t.FinishedAt.AsTime()) < doneTTL
	case v1.TaskStatus_TASK_STATUS_FAILED:
		return t.FinishedAt == nil || now.Sub(t.FinishedAt.AsTime()) < failedTTL
	}
	return true
}

func matches(t *v1.Task, filter string) bool {
	if filter == "" {
		return true
	}
	filter = strings.ToLower(filter)
	for _, s := range []string{t.Name, t.ProcessType, statusLabel(t.Status), strings.ToLower(t.Title)} {
		if strings.Contains(s, filter) {
			return true
		}
	}
	return false
}

func sortTasks(ts []*v1.Task) {
	sort.SliceStable(ts, func(i, j int) bool {
		a, b := ts[i], ts[j]
		if ra, rb := statusRank[a.Status], statusRank[b.Status]; ra != rb {
			return ra < rb
		}
		// Within a status keep start order stable (SPEC §2.2).
		if !a.CreatedAt.AsTime().Equal(b.CreatedAt.AsTime()) {
			return a.CreatedAt.AsTime().Before(b.CreatedAt.AsTime())
		}
		return a.Id < b.Id
	})
}

// Lanes returns the visible tasks per lane, sorted needs-you-first.
func (f *Fleet) Lanes(now time.Time, filter string) map[string][]*v1.Task {
	out := map[string][]*v1.Task{}
	for _, t := range f.tasks {
		if Visible(t, now) && matches(t, filter) {
			out[t.ProcessType] = append(out[t.ProcessType], t)
		}
	}
	for _, ts := range out {
		sortTasks(ts)
	}
	return out
}

// All returns every visible task sorted needs-you-first (ungrouped views).
func (f *Fleet) All(now time.Time, filter string) []*v1.Task {
	var out []*v1.Task
	for _, t := range f.tasks {
		if Visible(t, now) && matches(t, filter) {
			out = append(out, t)
		}
	}
	sortTasks(out)
	return out
}

// Tick updates the per-lane empty timers.
func (f *Fleet) Tick(now time.Time) {
	lanes := f.Lanes(now, "")
	for _, s := range f.Types {
		if len(lanes[s.Name]) > 0 {
			delete(f.emptySince, s.Name)
			delete(f.manualOpen, s.Name)
			continue
		}
		if _, ok := f.emptySince[s.Name]; !ok {
			f.emptySince[s.Name] = now
		}
	}
}

// Expand opens a collapsed lane by hand.
func (f *Fleet) Expand(lane string) { f.manualOpen[lane] = true }

// LaneInputs feeds ComputeLayout.
func (f *Fleet) LaneInputs(now time.Time, lanes map[string][]*v1.Task) []LaneIn {
	out := make([]LaneIn, 0, len(f.Types))
	for _, s := range f.Types {
		since, empty := f.emptySince[s.Name]
		out = append(out, LaneIn{
			Name:            s.Name,
			Count:           len(lanes[s.Name]),
			EmptyLongEnough: empty && !f.manualOpen[s.Name] && now.Sub(since) >= collapseAfter,
		})
	}
	return out
}

// Recent returns finished tasks, newest first: today's, or all of them.
func (f *Fleet) Recent(now time.Time, all bool) []*v1.Task {
	y, m, d := now.Date()
	midnight := time.Date(y, m, d, 0, 0, 0, 0, now.Location())
	var out []*v1.Task
	for _, t := range f.tasks {
		if t.FinishedAt == nil || !(t.Status == v1.TaskStatus_TASK_STATUS_SUCCEEDED || t.Status == v1.TaskStatus_TASK_STATUS_FAILED) {
			continue
		}
		if all || !t.FinishedAt.AsTime().Before(midnight) {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].FinishedAt.AsTime().After(out[j].FinishedAt.AsTime()) })
	return out
}

// RecentDone counts done tasks of a lane that aged out today ("✓n recent").
func (f *Fleet) RecentDone(lane string, now time.Time) int {
	n := 0
	for _, t := range f.Recent(now, false) {
		if t.ProcessType == lane && t.Status == v1.TaskStatus_TASK_STATUS_SUCCEEDED && !Visible(t, now) {
			n++
		}
	}
	return n
}

// LastRun is the most recently finished task of a lane, if any.
func (f *Fleet) LastRun(lane string, now time.Time) *v1.Task {
	for _, t := range f.Recent(now, true) {
		if t.ProcessType == lane {
			return t
		}
	}
	return nil
}

// Totals sums usage over the given tasks, keeping billed and indicative
// cost apart.
func Totals(ts []*v1.Task) (tokens int64, billed, indicative float64) {
	for _, t := range ts {
		u := t.GetUsage()
		tokens += u.GetInputTokens() + u.GetOutputTokens() + u.GetCacheReadTokens() + u.GetCacheWriteTokens()
		billed += u.GetCostUsd()
		indicative += u.GetIndicativeCostUsd()
	}
	return tokens, billed, indicative
}
