package tracker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"regexp"
	"strings"
	"time"

	v1 "github.com/squash-box/floatilla/src/gen/go/floatilla/v1"
)

// Fleet is the part of the control plane the sync drives.
type Fleet interface {
	Submit(ctx context.Context, req *v1.SubmitTaskRequest) (*v1.Task, error)
	// Tasks returns all tasks, finished ones included.
	Tasks(ctx context.Context) ([]*v1.Task, error)
	PostEvent(ctx context.Context, taskID string, ev *v1.ExternalEvent) error
	Answer(ctx context.Context, taskID string, a *v1.PromptAnswer) error
	Stop(ctx context.Context, taskID, reason string) error
}

// Link ties a tracker item to the task working on it.
type Link struct {
	ItemID  string
	ItemKey string
	URL     string
	// TaskID is the current impl task; empty until the item is dispatched.
	TaskID string
	// PlanTaskID and StoryKey are set for items published from a plan.
	PlanTaskID string
	StoryKey   string
	// SyncedState is the state floatilla last set on the item.
	SyncedState State
	// Since is when SyncedState last changed; review comments after it are
	// feedback.
	Since time.Time
	// Accepted: a person moved the item to Done while the task was in review
	// or awaiting approval.
	Accepted   bool
	PRLinked   bool
	Dispatches int
	// BlockedOn lists the open blockers (item keys) last reported while the
	// item waited in Ready; empty when it isn't blocked.
	BlockedOn string
	// Settled: the task finished and the item shows it; nothing left to sync.
	Settled bool
}

// Links persists links and which plans were published.
type Links interface {
	TrackerLinks(ctx context.Context) ([]Link, error)
	SaveTrackerLink(ctx context.Context, l Link) error
	PublishedPlans(ctx context.Context) (map[string]bool, error)
	MarkPlanPublished(ctx context.Context, planTaskID string) error
}

// ReviewEvent is the park key of the impl workflow's review stage.
const ReviewEvent = "pr.review"

// Sync reconciles the tracker and the fleet on a timer.
type Sync struct {
	Tracker Tracker
	Fleet   Fleet
	Links   Links
	Log     *slog.Logger
	// ImplType is the process type items are dispatched to; PlanType the one
	// for items labelled PlanLabel.
	ImplType string
	PlanType string
	// Repo is the default inputs.repo for dispatched tasks. An item overrides
	// it with a "repo: <url>" line in its description.
	Repo string
	// PublishState is where published stories land: StateBacklog leaves
	// dispatch to a person, StateReady hands them straight to agents.
	PublishState State
	Interval     time.Duration
	now          func() time.Time
}

func (s *Sync) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// Run sets the tracker up, then syncs every Interval until ctx ends. Tracker
// errors are logged and retried; they never stop the control plane.
func (s *Sync) Run(ctx context.Context) error {
	ready := false
	for {
		if !ready {
			if err := s.Tracker.EnsureSetup(ctx); err != nil {
				s.Log.Warn("tracker setup", "tracker", s.Tracker.Name(), "err", err)
			} else {
				ready = true
				s.Log.Info("tracker ready", "tracker", s.Tracker.Name())
			}
		}
		if ready {
			if err := s.Tick(ctx); err != nil && ctx.Err() == nil {
				s.Log.Warn("tracker sync", "tracker", s.Tracker.Name(), "err", err)
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(s.Interval):
		}
	}
}

// Tick runs one reconciliation pass.
func (s *Sync) Tick(ctx context.Context) error {
	items, err := s.Tracker.Items(ctx)
	if err != nil {
		return err
	}
	list, err := s.Links.TrackerLinks(ctx)
	if err != nil {
		return err
	}
	all, err := s.Fleet.Tasks(ctx)
	if err != nil {
		return err
	}
	published, err := s.Links.PublishedPlans(ctx)
	if err != nil {
		return err
	}
	links := map[string]*Link{}
	for i := range list {
		links[list[i].ItemID] = &list[i]
	}
	tasks := map[string]*v1.Task{}
	for _, t := range all {
		tasks[t.Id] = t
	}

	for _, t := range all {
		if len(t.Stories) > 0 && t.Status == v1.TaskStatus_TASK_STATUS_SUCCEEDED && !published[t.Id] {
			if err := s.publishPlan(ctx, t, items, links); err != nil {
				return fmt.Errorf("publish plan %s: %w", t.Name, err)
			}
		}
	}

	// People's moves on the board.
	byID := map[string]Item{}
	for _, it := range items {
		byID[it.ID] = it
	}
	touched := map[string]bool{}
	for _, it := range items {
		l := links[it.ID]
		var task *v1.Task
		if l != nil && l.TaskID != "" {
			task = tasks[l.TaskID]
		}
		var err error
		switch it.State {
		case StateReady:
			err = s.onReady(ctx, it, l, task, board{items: byID, links: links, tasks: tasks})
			touched[it.ID] = true
		case StateDone:
			switch {
			case l == nil || l.Accepted:
			case awaitingApproval(task):
				err = s.answer(ctx, it, l, task, true)
				touched[it.ID] = true
			case inReview(task):
				err = s.review(ctx, it, l, task, "approved")
				touched[it.ID] = true
			}
		case StateCancelled:
			if task != nil && !terminal(task) {
				err = s.Fleet.Stop(ctx, task.Id, "cancelled in "+s.Tracker.Name())
			}
			if l != nil && !l.Settled {
				l.Settled, l.SyncedState = true, StateCancelled
				err = joinErr(err, s.Links.SaveTrackerLink(ctx, *l))
			}
			touched[it.ID] = true
		}
		if err != nil {
			return fmt.Errorf("%s: %w", it.Key, err)
		}
	}

	// Task status onto the board.
	for _, it := range items {
		l := links[it.ID]
		if l == nil || l.TaskID == "" || l.Settled || touched[it.ID] {
			continue
		}
		if task := tasks[l.TaskID]; task != nil {
			if err := s.push(ctx, it, l, task); err != nil {
				return fmt.Errorf("%s: %w", it.Key, err)
			}
		}
	}
	return nil
}

func (s *Sync) publishPlan(ctx context.Context, plan *v1.Task, items []Item, links map[string]*Link) error {
	existing := map[string]Item{}
	for _, it := range items {
		if it.ExternalSource == Source {
			existing[it.ExternalID] = it
		}
	}
	parent := plan.Inputs["tracker.item"]
	var ids []string
	byKey := map[string]string{}
	created := map[string]bool{}
	fresh := true
	for _, st := range plan.Stories {
		ext := "plan/" + plan.Id + "/" + st.Key
		it, ok := existing[ext]
		if ok {
			fresh = false
		} else {
			var err error
			it, err = s.Tracker.Create(ctx, NewItem{
				Title: st.Title, Description: st.Description, AcceptanceCriteria: st.AcceptanceCriteria,
				Priority: st.Priority, State: s.PublishState, ExternalID: ext, ParentID: parent,
			})
			if err != nil {
				return err
			}
			created[st.Key] = true
		}
		ids = append(ids, it.ID)
		byKey[st.Key] = it.ID
		if links[it.ID] == nil {
			l := &Link{ItemID: it.ID, ItemKey: it.Key, URL: it.URL, PlanTaskID: plan.Id, StoryKey: st.Key, SyncedState: s.PublishState, Since: s.clock()}
			if err := s.Links.SaveTrackerLink(ctx, *l); err != nil {
				return err
			}
			links[it.ID] = l
		}
	}
	for _, st := range plan.Stories {
		if !created[st.Key] || len(st.DependsOn) == 0 {
			continue
		}
		var blockers []string
		for _, d := range st.DependsOn {
			if id := byKey[d]; id != "" {
				blockers = append(blockers, id)
			}
		}
		if len(blockers) > 0 {
			if err := s.Tracker.Block(ctx, byKey[st.Key], blockers); err != nil {
				return fmt.Errorf("story %s dependencies: %w", st.Key, err)
			}
		}
	}
	// Stories of a plan item become its children; others get a group. A
	// retried publish skips grouping rather than make a second group.
	if fresh && parent == "" {
		if err := s.Tracker.Group(ctx, plan.Name, plan.Title, ids); err != nil {
			s.Log.Warn("tracker group", "plan", plan.Name, "err", err)
		}
	}
	s.Log.Info("plan published", "plan", plan.Name, "stories", len(ids), "tracker", s.Tracker.Name())
	return s.Links.MarkPlanPublished(ctx, plan.Id)
}

// board is one pass's view of the tracker and the fleet.
type board struct {
	items map[string]Item
	links map[string]*Link
	tasks map[string]*v1.Task
}

func (s *Sync) onReady(ctx context.Context, it Item, l *Link, task *v1.Task, b board) error {
	switch {
	case task == nil || terminal(task):
		blockers, err := s.Tracker.BlockedBy(ctx, it.ID)
		if err != nil {
			return err
		}
		if open := openBlockers(blockers, b.items); len(open) > 0 {
			return s.wait(ctx, it, l, open)
		}
		return s.dispatch(ctx, it, l, blockers, b)
	case task.Status == v1.TaskStatus_TASK_STATUS_WAITING && task.Prompt != nil:
		// Handed back while blocked: comments since are the answer, and an
		// approval handed back is a rejection.
		return s.answer(ctx, it, l, task, task.Prompt.Kind != v1.PromptKind_PROMPT_KIND_APPROVAL)
	case inReview(task):
		return s.review(ctx, it, l, task, "changes_requested")
	default:
		// Moved back while the agent is still on it: show where it is.
		return s.setState(ctx, it, l, stateFor(task), "")
	}
}

// openBlockers returns the blockers not yet Done or Cancelled. Blockers
// outside the project (not on the board) don't hold anything up.
func openBlockers(ids []string, items map[string]Item) []Item {
	var open []Item
	for _, id := range ids {
		if b, ok := items[id]; ok && b.State != StateDone && b.State != StateCancelled {
			open = append(open, b)
		}
	}
	return open
}

// wait leaves a blocked item in Ready, saying once what it waits for; it
// starts on the pass after its blockers are done.
func (s *Sync) wait(ctx context.Context, it Item, l *Link, open []Item) error {
	var keys, desc []string
	for _, b := range open {
		keys = append(keys, b.Key)
		state := string(b.State)
		if state == "" {
			state = "not started"
		}
		desc = append(desc, fmt.Sprintf("%s (%s)", b.Key, state))
	}
	if l == nil {
		l = &Link{ItemID: it.ID, ItemKey: it.Key, URL: it.URL, Since: s.clock()}
	}
	if joined := strings.Join(keys, ","); l.BlockedOn != joined {
		l.BlockedOn = joined
		if err := s.Tracker.Comment(ctx, it.ID, fmt.Sprintf("queued: blocked by %s. Starts when %s done.", strings.Join(desc, ", "), plural(len(open), "it is", "they are"))); err != nil {
			return err
		}
		s.Log.Info("tracker item blocked", "item", it.Key, "on", joined)
		return s.Links.SaveTrackerLink(ctx, *l)
	}
	return nil
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func (s *Sync) dispatch(ctx context.Context, it Item, l *Link, blockers []string, b board) error {
	if l == nil {
		l = &Link{ItemID: it.ID}
	}
	l.ItemKey, l.URL, l.BlockedOn = it.Key, it.URL, ""
	var plan *v1.Task
	if l.PlanTaskID != "" {
		plan = b.tasks[l.PlanTaskID]
	}

	processType := s.ImplType
	if it.HasLabel(PlanLabel) {
		processType = s.PlanType
	}
	inputs := map[string]string{}
	if s.Repo != "" {
		inputs["repo"] = s.Repo
	}
	if plan != nil {
		maps.Copy(inputs, plan.Inputs)
	}
	if m := repoLine.FindStringSubmatch(it.Description); m != nil {
		inputs["repo"] = m[1]
	}
	inputs["tracker.item"], inputs["tracker.key"], inputs["tracker.url"] = it.ID, it.Key, it.URL
	// The work this item builds on may not be merged yet: hand over the
	// blockers' branches.
	var after, afterDesc []string
	for _, id := range blockers {
		bl := b.links[id]
		if bl == nil || b.tasks[bl.TaskID] == nil {
			continue
		}
		branch := b.tasks[bl.TaskID].Branch
		after = append(after, branch)
		afterDesc = append(afterDesc, fmt.Sprintf("%s (branch %s)", b.items[id].Key, branch))
	}
	if len(after) > 0 {
		inputs["after_branches"] = strings.Join(after, ",")
	}

	var b2 strings.Builder
	fmt.Fprintf(&b2, "Tracker item %s: %s\n%s\n\n%s\n", it.Key, it.Title, it.URL, strings.TrimSpace(it.Description))
	if len(afterDesc) > 0 {
		fmt.Fprintf(&b2, "\nThis builds on finished work that may not be merged yet: %s. Merge those branches into yours first if the base doesn't contain them.\n", strings.Join(afterDesc, ", "))
	}
	if plan != nil && plan.Spec != "" {
		fmt.Fprintf(&b2, "\nThis story is part of plan %q. The plan's spec, for context (implement only this story):\n\n%s\n", plan.Name, plan.Spec)
	}

	name := taskName(it.Key, it.Title, l.Dispatches+1)
	req := &v1.SubmitTaskRequest{
		ProcessType: processType, Name: name, Title: it.Title,
		Instructions: b2.String(), Inputs: inputs,
	}
	if plan != nil {
		req.ParentId = plan.Id
	}
	t, err := s.Fleet.Submit(ctx, req)
	if err != nil {
		return err
	}
	s.Log.Info("tracker item dispatched", "item", it.Key, "task", t.Id, "type", processType, "name", name)
	l.TaskID, l.Dispatches = t.Id, l.Dispatches+1
	l.Accepted, l.PRLinked, l.Settled = false, false, false
	// Later items in this pass see it started.
	b.links[it.ID], b.tasks[t.Id] = l, t
	started := it
	started.State = StateWorking
	b.items[it.ID] = started
	return s.setState(ctx, it, l, StateWorking, fmt.Sprintf("%s agent %s picked this up", processType, name))
}

// answer replies to the task's pending prompt from the board: the comments
// since the item went to Needs you / In review are the reply text.
func (s *Sync) answer(ctx context.Context, it Item, l *Link, task *v1.Task, approved bool) error {
	comments, err := s.Tracker.Comments(ctx, it.ID, l.Since)
	if err != nil {
		return err
	}
	p := task.Prompt
	if err := s.Fleet.Answer(ctx, task.Id, &v1.PromptAnswer{PromptId: p.Id, Approved: approved, Text: strings.Join(comments, "\n\n")}); err != nil {
		return err
	}
	s.Log.Info("tracker answer", "item", it.Key, "task", task.Id, "kind", p.Kind, "approved", approved, "comments", len(comments))
	if p.Kind == v1.PromptKind_PROMPT_KIND_APPROVAL && approved {
		l.Accepted = true
		l.SyncedState, l.Since = StateDone, s.clock()
		return s.Links.SaveTrackerLink(ctx, *l)
	}
	msg := "answered"
	switch {
	case p.Kind == v1.PromptKind_PROMPT_KIND_APPROVAL:
		msg = fmt.Sprintf("changes requested (%d comments); revising", len(comments))
	case p.Kind == v1.PromptKind_PROMPT_KIND_PERMISSION:
		msg = "allowed: " + p.Text
	}
	return s.setState(ctx, it, l, StateWorking, msg)
}

// review resumes a task parked on review with the person's verdict.
func (s *Sync) review(ctx context.Context, it Item, l *Link, task *v1.Task, verdict string) error {
	comments, err := s.Tracker.Comments(ctx, it.ID, l.Since)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(map[string]any{
		"state": verdict, "comments": comments, "source": s.Tracker.Name(), "item": it.Key,
	})
	if err != nil {
		return err
	}
	if err := s.Fleet.PostEvent(ctx, task.Id, &v1.ExternalEvent{Key: ReviewEvent, PayloadJson: string(payload)}); err != nil {
		return err
	}
	s.Log.Info("tracker review", "item", it.Key, "task", task.Id, "verdict", verdict, "comments", len(comments))
	if verdict == "approved" {
		l.Accepted = true
		l.SyncedState = StateDone
		return s.Links.SaveTrackerLink(ctx, *l)
	}
	return s.setState(ctx, it, l, StateWorking, fmt.Sprintf("addressing review feedback (%d comments)", len(comments)))
}

// push moves the item to match its task.
func (s *Sync) push(ctx context.Context, it Item, l *Link, task *v1.Task) error {
	if !l.PRLinked {
		if pr := prURL(task); pr != "" {
			if err := s.Tracker.Link(ctx, it.ID, pr, "Pull request"); err != nil {
				return err
			}
			l.PRLinked = true
			if err := s.Links.SaveTrackerLink(ctx, *l); err != nil {
				return err
			}
		}
	}
	want := stateFor(task)
	if l.Accepted && (want == StateWorking || want == StateInReview) && task.Status != v1.TaskStatus_TASK_STATUS_WAITING {
		// Accepted: the agent is wrapping up; leave the item in Done.
		want = StateDone
	}
	if want != "" && want != l.SyncedState {
		text, blocks := s.reason(task, want)
		if err := s.setState(ctx, it, l, want, text, blocks...); err != nil {
			return err
		}
	}
	if terminal(task) && l.SyncedState == want {
		l.Settled = true
		return s.Links.SaveTrackerLink(ctx, *l)
	}
	return nil
}

func (s *Sync) setState(ctx context.Context, it Item, l *Link, st State, comment string, blocks ...Block) error {
	if it.State != st {
		if err := s.Tracker.SetState(ctx, it.ID, st); err != nil {
			return err
		}
	}
	if comment != "" {
		if err := s.Tracker.Comment(ctx, it.ID, comment, blocks...); err != nil {
			return err
		}
	}
	if l.SyncedState != st {
		l.SyncedState, l.Since = st, s.clock()
	}
	return s.Links.SaveTrackerLink(ctx, *l)
}

func (s *Sync) reason(t *v1.Task, st State) (string, []Block) {
	switch st {
	case StateWorking:
		return "agent working", nil
	case StateNeedsYou:
		switch {
		case t.Status == v1.TaskStatus_TASK_STATUS_WAITING && t.GetPrompt().GetKind() == v1.PromptKind_PROMPT_KIND_PERMISSION:
			return fmt.Sprintf("needs permission: %s. Move to Ready for agent to allow, or answer in the floatilla TUI.", t.Prompt.Text), detail(t.Prompt)
		case t.Status == v1.TaskStatus_TASK_STATUS_WAITING:
			return fmt.Sprintf("question: %s. Reply in a comment, then move to Ready for agent.", t.GetPrompt().GetText()), detail(t.Prompt)
		case t.Status == v1.TaskStatus_TASK_STATUS_PAUSED:
			return "paused in floatilla", nil
		default:
			return fmt.Sprintf("agent failed: %s. Move to Ready for agent to retry.", t.Error), nil
		}
	case StateInReview:
		if awaitingApproval(t) {
			blocks := detail(t.Prompt)
			if len(t.Stories) > 0 {
				var list []string
				for _, st := range t.Stories {
					list = append(list, st.Title)
				}
				blocks = append(blocks, Block{Heading: fmt.Sprintf("Stories (%d)", len(t.Stories)), List: list})
			}
			if t.Spec != "" {
				blocks = append(blocks, Block{Heading: "Spec", Pre: t.Spec})
			}
			return fmt.Sprintf("approval needed: %s. Move to Done to approve, or comment and move back to Ready for agent to request changes.", t.Prompt.Text), blocks
		}
		msg := "ready for review"
		if pr := prURL(t); pr != "" {
			msg += ": " + pr
		}
		return msg + ". Move to Done to accept, or comment and move back to Ready for agent to request changes.", nil
	case StateDone:
		return "agent finished", nil
	}
	return "", nil
}

func detail(p *v1.Prompt) []Block {
	if p.GetDetail() == "" {
		return nil
	}
	return []Block{{Pre: p.Detail}}
}

func stateFor(t *v1.Task) State {
	switch t.Status {
	case v1.TaskStatus_TASK_STATUS_QUEUED, v1.TaskStatus_TASK_STATUS_RUNNING:
		return StateWorking
	case v1.TaskStatus_TASK_STATUS_WAITING:
		if awaitingApproval(t) {
			return StateInReview
		}
		return StateNeedsYou
	case v1.TaskStatus_TASK_STATUS_PAUSED, v1.TaskStatus_TASK_STATUS_FAILED:
		return StateNeedsYou
	case v1.TaskStatus_TASK_STATUS_PARKED:
		return StateInReview
	case v1.TaskStatus_TASK_STATUS_SUCCEEDED:
		return StateDone
	}
	return ""
}

func terminal(t *v1.Task) bool {
	return t.Status == v1.TaskStatus_TASK_STATUS_SUCCEEDED || t.Status == v1.TaskStatus_TASK_STATUS_FAILED
}

func inReview(t *v1.Task) bool {
	return t != nil && t.Status == v1.TaskStatus_TASK_STATUS_PARKED && t.ParkOn == ReviewEvent
}

func awaitingApproval(t *v1.Task) bool {
	return t != nil && t.Status == v1.TaskStatus_TASK_STATUS_WAITING && t.GetPrompt().GetKind() == v1.PromptKind_PROMPT_KIND_APPROVAL
}

func prURL(t *v1.Task) string {
	for _, a := range t.Artefacts {
		if a.Kind == "pr" && (strings.HasPrefix(a.Uri, "https://") || strings.HasPrefix(a.Uri, "http://")) {
			return a.Uri
		}
	}
	return ""
}

var (
	nonSlug  = regexp.MustCompile(`[^a-z0-9]+`)
	repoLine = regexp.MustCompile(`(?mi)^\s*repo:\s*(\S+)`)
)

// taskName makes a task slug from the item, e.g. "float-12-add-retry-to-fx".
func taskName(key, title string, n int) string {
	name := strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(key+" "+title), "-"), "-")
	if len(name) > 40 {
		name = name[:40]
		if i := strings.LastIndexByte(name, '-'); i > len(key) {
			name = name[:i] // whole words only
		}
	}
	if n > 1 {
		name += fmt.Sprintf("-r%d", n)
	}
	return name
}

func joinErr(a, b error) error {
	if a != nil {
		return a
	}
	return b
}
