package tui

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/squash-box/floatilla/src/gen/go/floatilla/v1"
)

// DemoTypes mirror packs/dev so the demo runs without a control plane.
func DemoTypes() []*v1.ProcessTypeSpec {
	return []*v1.ProcessTypeSpec{
		{Name: "plan", Title: "PLANNING", Tag: "PLAN", LegendShort: "explore › spec › validate", LaneOrder: 10,
			Stages: []string{"exploration", "specification", "validation"}, Artefacts: []string{"log", "doc"}},
		{Name: "impl", Title: "IMPLEMENTATION", Tag: "IMPL", LegendShort: "setup › impl › test › pr › review › fdbk › closed", LaneOrder: 20,
			Stages:    []string{"setup", "implementation", "testing", "pr", "awaiting review", "feedback implementation", "closed"},
			Artefacts: []string{"log", "diff", "tests", "doc", "pr"}},
		{Name: "deploy", Title: "DEPLOY", Tag: "DEPLOY", LegendShort: "setup › deploy › verify", LaneOrder: 30,
			Stages: []string{"setup", "deploy", "verify"}, Artefacts: []string{"log", "deploy"}},
	}
}

type demoAgent struct {
	typ, name, status string
	stage             int32
	frac              float32
	elapsed           time.Duration
	tokens            int64
	cost              float64
	sub               bool // on a Claude subscription: cost is indicative
	title, activity   string
	tail              []string
	artefacts         [][2]string
	prompt            *v1.Prompt
}

// The sample fleet from fleet-handoff/design/mockups/Main.dc.html.
var demoFleet = []demoAgent{
	{"plan", "fx-retry-spec", "waiting", 2, 0.5, 12 * time.Minute, 14800, 0.74, true, "Spec retry policy for FX rate fetch", "approve spec? 2 open questions",
		[]string{"wrote docs/specs/fx-retry.md · 4 sections"}, [][2]string{{"doc", "spec 4 sections  open q 2"}},
		&v1.Prompt{Id: "p-spec", Kind: v1.PromptKind_PROMPT_KIND_APPROVAL, Text: "approve spec? 2 open questions"}},
	{"plan", "otel-tracing", "running", 0, 0.6, 4 * time.Minute, 6100, 0.29, true, "Plan OpenTelemetry spans for Service Bus", "reading src/bus/handlers/",
		[]string{"grep ServiceBusReceiver → 11 hits", "read src/bus/handlers/orders.ts"}, [][2]string{{"doc", "notes 14 findings  files 23 read"}}, nil},
	{"impl", "flaky-tests", "waiting", 2, 0.7, 22 * time.Minute, 31000, 1.74, false, "Stabilise flaky checkout e2e suite", "approve Bash(pnpm test --runInBand)?",
		[]string{"ran checkout.spec.ts 20× · 0 failures"}, [][2]string{{"diff", "+42 −17"}, {"tests", "✓118 ✗0"}},
		&v1.Prompt{Id: "p-bash", Kind: v1.PromptKind_PROMPT_KIND_PERMISSION, Text: "approve Bash(pnpm test --runInBand)?", Detail: `{"command":"pnpm test --runInBand"}`}},
	{"impl", "ledger-migration", "failed", 2, 0.8, 41 * time.Minute, 58300, 3.20, false, "Migrate ledger entries to v3 schema", "exit 1 in 4m12s · 2 tests failing",
		[]string{"✗ rolls back partial batch", "✗ preserves entry ordering"}, [][2]string{{"diff", "+312 −140"}, {"tests", "✓64 ✗2"}}, nil},
	{"impl", "auth-refactor", "running", 1, 0.6, 14 * time.Minute, 38200, 2.14, true, "Move sessions to rotating refresh tokens", "editing src/auth/middleware.ts",
		[]string{"edit src/auth/session.ts +31 −9", "bash pnpm tsc --noEmit · ✓ no errors"}, [][2]string{{"diff", "+184 −36"}, {"tests", "✓41 ✗2"}, {"doc", "docs/plans/auth-rotation.md"}}, nil},
	{"impl", "orders-pagination", "running", 5, 0.4, 80 * time.Minute, 61900, 3.41, false, "Paginate /v2/orders with cursors", "addressing review on #142 · 2/3",
		[]string{"● resolved: clamp limit to 100", "edit src/api/orders.ts +6 −2"}, [][2]string{{"diff", "+274 −53"}, {"pr", "#142 ●1 open"}}, nil},
	{"impl", "deps-bump", "running", 3, 0.5, 27 * time.Minute, 44000, 2.38, false, "Bump typescript 5.6 → 5.9 and fix types", "writing PR description",
		[]string{"git push -u origin agent/deps-bump", "gh pr create --draft"}, [][2]string{{"diff", "+221 −198"}, {"tests", "✓203 ✗0"}}, nil},
	{"impl", "docs-sweep", "paused", 1, 0.4, 18 * time.Minute, 12200, 0.57, false, "Refresh API reference for v2 endpoints", "paused 11m ago · r to resume",
		[]string{"write docs/api/orders.md +48", "edit docs/api/index.md +6 −2"}, [][2]string{{"diff", "+77 −20"}}, nil},
	{"deploy", "fx-hotfix-prod", "running", 2, 0.7, 6 * time.Minute, 9400, 0.38, false, "Ship FX rounding hotfix to production", "verifying · smoke 14/20 passed",
		[]string{"rollout 100% · 6/6 pods ready", "p95 182ms · errors 0.1%"}, [][2]string{{"deploy", "env prod  checks 14/20  err 0.1%"}}, nil},
	{"deploy", "pricing-staging", "success", 2, 1, 31 * time.Minute, 7700, 0.31, false, "Deploy pricing cache to staging", "verified · 0 errors in 10m",
		[]string{"smoke 20/20 passed", "rollback: helm rollback pricing 41"}, [][2]string{{"deploy", "env staging  checks 20/20  err 0.0%"}}, nil},
}

var demoStatus = map[string]v1.TaskStatus{
	"running": v1.TaskStatus_TASK_STATUS_RUNNING, "waiting": v1.TaskStatus_TASK_STATUS_WAITING,
	"failed": v1.TaskStatus_TASK_STATUS_FAILED, "success": v1.TaskStatus_TASK_STATUS_SUCCEEDED,
	"paused": v1.TaskStatus_TASK_STATUS_PAUSED,
}

// DemoTasks builds the sample fleet as of now.
func DemoTasks(now time.Time) []*v1.Task {
	out := make([]*v1.Task, 0, len(demoFleet))
	for i, a := range demoFleet {
		created := now.Add(-a.elapsed)
		t := &v1.Task{
			Id:          fmt.Sprintf("demo%02d", i),
			Name:        a.name,
			ProcessType: a.typ,
			Workflow:    "demo",
			Title:       a.title,
			Status:      demoStatus[a.status],
			StageIndex:  a.stage,
			Activity:    a.activity,
			Tail:        a.tail,
			Prompt:      a.prompt,
			Branch:      "agent/" + a.name,
			RunSeq:      1,
			ContextUsed: float64(a.tokens) / 120_000,
			Usage:       &v1.Usage{InputTokens: a.tokens * 9 / 10, OutputTokens: a.tokens / 10},
			CreatedAt:   timestamppb.New(created),
			StartedAt:   timestamppb.New(created),
			UpdatedAt:   timestamppb.New(now),
		}
		if a.sub {
			t.Usage.IndicativeCostUsd = a.cost
		} else {
			t.Usage.CostUsd = a.cost
		}
		if a.frac > 0 && a.status != "success" {
			t.StageProgress = proto.Float32(a.frac)
		}
		stageDur := a.elapsed / time.Duration(a.stage+1)
		for s := int32(0); s <= a.stage; s++ {
			rec := &v1.StageRecord{Index: s, Name: DemoTypes()[typeIndex(a.typ)].Stages[s],
				StartedAt: timestamppb.New(created.Add(time.Duration(s) * stageDur))}
			if s < a.stage || a.status == "success" {
				rec.FinishedAt = timestamppb.New(created.Add(time.Duration(s+1) * stageDur))
				rec.Summary = "completed " + rec.Name
			}
			t.Stages = append(t.Stages, rec)
		}
		if a.status == "success" || a.status == "failed" {
			t.FinishedAt = timestamppb.New(now.Add(-2 * time.Minute))
			if a.status == "failed" {
				t.Error = a.activity
			}
		}
		for _, kv := range a.artefacts {
			t.Artefacts = append(t.Artefacts, &v1.ArtefactRef{Kind: kv[0], Title: kv[0], Summary: kv[1], UpdatedAt: t.UpdatedAt})
		}
		out = append(out, t)
	}
	return out
}

func typeIndex(name string) int {
	for i, s := range DemoTypes() {
		if s.Name == name {
			return i
		}
	}
	return 0
}

var demoActivities = []string{"reading %s", "editing %s", "running tests for %s", "thinking about %s"}
var demoFiles = []string{"src/auth/session.ts", "src/api/orders.ts", "src/bus/handlers/orders.ts", "test/checkout.spec.ts"}

// DemoSource serves the sample fleet locally and animates it, so every
// state in the design is visible without a control plane.
type DemoSource struct {
	mu      sync.Mutex
	tasks   map[string]*v1.Task
	order   []string
	logs    map[string][]*v1.AgentEvent
	subs    map[int]func(*v1.Task)
	logSubs map[int]demoLogSub
	nextSub int
	n       int
}

type demoLogSub struct {
	id     string
	onLog  func(*v1.AgentEvent)
	onTask func(*v1.Task)
}

func NewDemoSource() *DemoSource {
	d := &DemoSource{tasks: map[string]*v1.Task{}, logs: map[string][]*v1.AgentEvent{}, subs: map[int]func(*v1.Task){}, logSubs: map[int]demoLogSub{}}
	now := time.Now()
	for _, t := range DemoTasks(now) {
		d.tasks[t.Id] = t
		d.order = append(d.order, t.Id)
		d.logs[t.Id] = demoLog(t)
	}
	go d.animate()
	return d
}

func logEvent(id string, at time.Time, kind, text string, tone v1.LogTone) *v1.AgentEvent {
	return &v1.AgentEvent{TaskId: id, At: timestamppb.New(at),
		Body: &v1.AgentEvent_Log{Log: &v1.LogLine{Kind: kind, Text: text, Tone: tone}}}
}

func demoLog(t *v1.Task) []*v1.AgentEvent {
	start := t.CreatedAt.AsTime()
	at := func(m int) time.Time { return start.Add(time.Duration(m) * 20 * time.Second) }
	evs := []*v1.AgentEvent{
		logEvent(t.Id, at(0), "start", "task: "+strings.ToLower(t.Title), 0),
		logEvent(t.Id, at(1), "read", "README.md (88 lines)", v1.LogTone_LOG_TONE_MUTED),
		logEvent(t.Id, at(2), "grep", "TODO|FIXME (14 matches)", v1.LogTone_LOG_TONE_MUTED),
		logEvent(t.Id, at(3), "think", "plan: change the smallest surface first, keep tests green", v1.LogTone_LOG_TONE_MUTED),
	}
	for i, l := range t.Tail {
		kind, text, _ := strings.Cut(l, " ")
		if !contains([]string{"read", "edit", "write", "bash", "grep"}, kind) {
			kind, text = "think", l
		}
		evs = append(evs, logEvent(t.Id, at(4+2*i), kind, text, 0))
		if kind == "bash" {
			evs = append(evs,
				logEvent(t.Id, time.Time{}, "", "FAIL  test/auth/rotation.spec.ts", v1.LogTone_LOG_TONE_FAILED),
				logEvent(t.Id, time.Time{}, "", "✓ 41 passed  ✗ 2 failed", v1.LogTone_LOG_TONE_MUTED))
		}
	}
	return evs
}

func (d *DemoSource) Snapshot(context.Context) ([]*v1.ProcessTypeSpec, []*v1.Task, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]*v1.Task, 0, len(d.order))
	for _, id := range d.order {
		out = append(out, proto.Clone(d.tasks[id]).(*v1.Task))
	}
	return DemoTypes(), out, nil
}

func (d *DemoSource) WatchFleet(ctx context.Context, onTask func(*v1.Task)) error {
	d.mu.Lock()
	id := d.nextSub
	d.nextSub++
	d.subs[id] = onTask
	d.mu.Unlock()
	<-ctx.Done()
	d.mu.Lock()
	delete(d.subs, id)
	d.mu.Unlock()
	return ctx.Err()
}

func (d *DemoSource) WatchTask(ctx context.Context, taskID string, onTask func(*v1.Task), onLog func(*v1.AgentEvent)) error {
	d.mu.Lock()
	t, ok := d.tasks[taskID]
	if !ok {
		d.mu.Unlock()
		return fmt.Errorf("no task %s", taskID)
	}
	snap := proto.Clone(t).(*v1.Task)
	logs := append([]*v1.AgentEvent(nil), d.logs[taskID]...)
	id := d.nextSub
	d.nextSub++
	d.logSubs[id] = demoLogSub{id: taskID, onLog: onLog, onTask: onTask}
	d.mu.Unlock()

	onTask(snap)
	for _, ev := range logs {
		onLog(ev)
	}
	<-ctx.Done()
	d.mu.Lock()
	delete(d.logSubs, id)
	d.mu.Unlock()
	return ctx.Err()
}

// publish must be called with d.mu held.
func (d *DemoSource) publish(t *v1.Task) {
	t.UpdatedAt = timestamppb.Now()
	for _, fn := range d.subs {
		fn(proto.Clone(t).(*v1.Task))
	}
	for _, s := range d.logSubs {
		if s.id == t.Id {
			s.onTask(proto.Clone(t).(*v1.Task))
		}
	}
}

func (d *DemoSource) appendLog(id, kind, text string, tone v1.LogTone) {
	ev := logEvent(id, time.Now(), kind, text, tone)
	d.logs[id] = append(d.logs[id], ev)
	for _, s := range d.logSubs {
		if s.id == id {
			s.onLog(ev)
		}
	}
}

func (d *DemoSource) animate() {
	tick := time.NewTicker(1500 * time.Millisecond)
	defer tick.Stop()
	for range tick.C {
		d.mu.Lock()
		d.n++
		for _, id := range d.order {
			t := d.tasks[id]
			if t.Status != v1.TaskStatus_TASK_STATUS_RUNNING || (d.n+len(id))%3 != 0 {
				continue
			}
			file := demoFiles[(d.n+len(t.Name))%len(demoFiles)]
			t.Activity = fmt.Sprintf(demoActivities[d.n%len(demoActivities)], file)
			t.Tail = append(t.Tail, "edit "+file)[max(0, len(t.Tail)-1):]
			p := float32(0.1)
			if t.StageProgress != nil {
				p = *t.StageProgress + 0.05
			}
			if p >= 1 {
				d.advance(t)
			} else {
				t.StageProgress = proto.Float32(p)
			}
			t.Usage.InputTokens += 800
			if t.Usage.IndicativeCostUsd > 0 { // a subscription task
				t.Usage.IndicativeCostUsd += 0.02
			} else {
				t.Usage.CostUsd += 0.02
			}
			d.appendLog(id, "edit", file+fmt.Sprintf(" +%d −%d", d.n%17+1, d.n%5), 0)
			d.publish(t)
		}
		d.mu.Unlock()
	}
}

// advance moves a running task to its next stage (or finishes it).
func (d *DemoSource) advance(t *v1.Task) {
	spec := DemoTypes()[typeIndex(t.ProcessType)]
	now := timestamppb.Now()
	if n := len(t.Stages); n > 0 {
		t.Stages[n-1].FinishedAt = now
	}
	if int(t.StageIndex)+1 >= len(spec.Stages) {
		t.Status = v1.TaskStatus_TASK_STATUS_SUCCEEDED
		t.FinishedAt = now
		t.StageProgress = nil
		t.Activity = "done"
		return
	}
	t.StageIndex++
	t.StageProgress = proto.Float32(0.05)
	t.Stages = append(t.Stages, &v1.StageRecord{Index: t.StageIndex, Name: spec.Stages[t.StageIndex], StartedAt: now})
	d.appendLog(t.Id, "start", "stage "+spec.Stages[t.StageIndex], v1.LogTone_LOG_TONE_MUTED)
}

func (d *DemoSource) with(id string, fn func(t *v1.Task) error) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	t, ok := d.tasks[id]
	if !ok {
		return fmt.Errorf("no task %s", id)
	}
	if err := fn(t); err != nil {
		return err
	}
	d.publish(t)
	return nil
}

func (d *DemoSource) Answer(_ context.Context, id string, a *v1.PromptAnswer) error {
	return d.with(id, func(t *v1.Task) error {
		if t.Status != v1.TaskStatus_TASK_STATUS_WAITING {
			return fmt.Errorf("%s is not waiting", t.Name)
		}
		verdict := "denied"
		if a.Approved {
			verdict = "approved"
		}
		if a.Text != "" {
			verdict = "answered: " + a.Text
		}
		d.appendLog(id, "", verdict, v1.LogTone_LOG_TONE_SUCCESS)
		t.Activity = verdict + ": " + t.Prompt.GetText()
		t.Prompt = nil
		t.Status = v1.TaskStatus_TASK_STATUS_RUNNING
		return nil
	})
}

func (d *DemoSource) Stop(_ context.Context, id, reason string) error {
	return d.with(id, func(t *v1.Task) error {
		if reason == "" {
			reason = "stopped by user"
		}
		t.Status = v1.TaskStatus_TASK_STATUS_FAILED
		t.Error, t.Activity, t.Prompt = reason, reason, nil
		t.FinishedAt = timestamppb.Now()
		return nil
	})
}

func (d *DemoSource) Pause(_ context.Context, id string) error {
	return d.with(id, func(t *v1.Task) error {
		t.Status, t.Activity, t.Prompt = v1.TaskStatus_TASK_STATUS_PAUSED, "paused · r to resume", nil
		return nil
	})
}

func (d *DemoSource) Resume(_ context.Context, id string) error {
	return d.with(id, func(t *v1.Task) error {
		if t.Status != v1.TaskStatus_TASK_STATUS_PAUSED {
			return fmt.Errorf("%s is not paused", t.Name)
		}
		t.Status, t.Activity = v1.TaskStatus_TASK_STATUS_RUNNING, "resuming"
		return nil
	})
}

func (d *DemoSource) Submit(_ context.Context, req *v1.SubmitTaskRequest) (*v1.Task, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := timestamppb.Now()
	spec := DemoTypes()[typeIndex(req.ProcessType)]
	t := &v1.Task{
		Id: fmt.Sprintf("demo%02d", len(d.order)), Name: req.Name, ProcessType: req.ProcessType, Workflow: "demo",
		Title: req.Title, Status: v1.TaskStatus_TASK_STATUS_RUNNING, Activity: "starting " + spec.Stages[0],
		Branch: "agent/" + req.Name, RunSeq: 1, Usage: &v1.Usage{}, CreatedAt: now, StartedAt: now,
		StageProgress: proto.Float32(0.05),
		Stages:        []*v1.StageRecord{{Index: 0, Name: spec.Stages[0], StartedAt: now}},
	}
	d.tasks[t.Id] = t
	d.order = append(d.order, t.Id)
	d.logs[t.Id] = []*v1.AgentEvent{logEvent(t.Id, now.AsTime(), "start", "task: "+strings.ToLower(t.Title), 0)}
	d.publish(t)
	return proto.Clone(t).(*v1.Task), nil
}
