package tracker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	v1 "github.com/squash-box/floatilla/src/gen/go/floatilla/v1"
)

type fakeFleet struct {
	tasks   []*v1.Task
	events  []string
	answers []string
	stops   []string
}

func (f *fakeFleet) Submit(_ context.Context, req *v1.SubmitTaskRequest) (*v1.Task, error) {
	t := &v1.Task{
		Id: fmt.Sprintf("t%d", len(f.tasks)+1), Name: req.Name, ProcessType: req.ProcessType, Title: req.Title,
		Instructions: req.Instructions, Inputs: req.Inputs, ParentId: req.ParentId, Status: v1.TaskStatus_TASK_STATUS_QUEUED,
	}
	f.tasks = append(f.tasks, t)
	return t, nil
}

func (f *fakeFleet) Tasks(context.Context) ([]*v1.Task, error) { return f.tasks, nil }

func (f *fakeFleet) PostEvent(_ context.Context, id string, ev *v1.ExternalEvent) error {
	f.events = append(f.events, id+" "+ev.Key+" "+ev.PayloadJson)
	return nil
}

func (f *fakeFleet) Answer(_ context.Context, id string, a *v1.PromptAnswer) error {
	f.answers = append(f.answers, fmt.Sprintf("%s %s approved=%v %q", id, a.PromptId, a.Approved, a.Text))
	if t := f.get(id); t != nil {
		t.Status, t.Prompt = v1.TaskStatus_TASK_STATUS_RUNNING, nil
	}
	return nil
}

func (f *fakeFleet) Stop(_ context.Context, id, reason string) error {
	f.stops = append(f.stops, id+" "+reason)
	return nil
}

func (f *fakeFleet) get(id string) *v1.Task {
	for _, t := range f.tasks {
		if t.Id == id {
			return t
		}
	}
	return nil
}

type memLinks struct {
	links map[string]Link
	plans map[string]bool
}

func (m *memLinks) TrackerLinks(context.Context) ([]Link, error) {
	var out []Link
	for _, l := range m.links {
		out = append(out, l)
	}
	return out, nil
}

func (m *memLinks) SaveTrackerLink(_ context.Context, l Link) error {
	m.links[l.ItemID] = l
	return nil
}

func (m *memLinks) PublishedPlans(context.Context) (map[string]bool, error) { return m.plans, nil }

func (m *memLinks) MarkPlanPublished(_ context.Context, id string) error {
	m.plans[id] = true
	return nil
}

type harness struct {
	t     *testing.T
	plane *fakePlane
	fleet *fakeFleet
	links *memLinks
	sync  *Sync
	clock time.Time
}

func newHarness(t *testing.T) *harness {
	f, p := newFakePlane(t)
	if err := p.EnsureSetup(context.Background()); err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, plane: f, fleet: &fakeFleet{}, links: &memLinks{links: map[string]Link{}, plans: map[string]bool{}}, clock: time.Now()}
	h.sync = &Sync{
		Tracker: p, Fleet: h.fleet, Links: h.links, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		ImplType: "impl", PlanType: "plan", PublishState: StateBacklog, Interval: time.Second,
		now: func() time.Time { return h.clock },
	}
	return h
}

func (h *harness) tick() {
	h.t.Helper()
	h.clock = h.clock.Add(time.Second)
	if err := h.sync.Tick(context.Background()); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) comments(issueID string) []string {
	var out []string
	for _, c := range h.plane.comments[issueID] {
		out = append(out, c["comment_stripped"].(string))
	}
	return out
}

func approvedPlan() *v1.Task {
	return &v1.Task{
		Id: "plan1", Name: "fx-retries", ProcessType: "plan", Title: "Make FX fetches resilient",
		Status: v1.TaskStatus_TASK_STATUS_SUCCEEDED, Inputs: map[string]string{"repo": "https://git.example/fx.git"},
		Spec: "# Spec\nRetry everything.",
		Stories: []*v1.Story{
			{Key: "s1", Title: "Add retry with jitter", Description: "Wrap fetchRate.", AcceptanceCriteria: []string{"3 retries"}, Priority: "high"},
			{Key: "s2", Title: "Alert on stale rates", Description: "Page when stale.", Priority: "medium"},
		},
	}
}

func TestSyncPublishesApprovedPlanOnce(t *testing.T) {
	h := newHarness(t)
	running := approvedPlan()
	running.Id, running.Status = "plan0", v1.TaskStatus_TASK_STATUS_RUNNING
	h.fleet.tasks = []*v1.Task{approvedPlan(), running}

	h.tick()
	h.tick()

	if len(h.plane.issues) != 2 {
		t.Fatalf("got %d issues, want 2 (unapproved plans stay unpublished, approved ones publish once)", len(h.plane.issues))
	}
	for i, it := range h.plane.issues {
		if h.plane.stateName(it["id"].(string)) != "Backlog" || it["external_id"] != fmt.Sprintf("plan/plan1/s%d", i+1) {
			t.Fatalf("issue %d: %v", i, it)
		}
	}
	if len(h.plane.modules) != 1 {
		t.Fatalf("modules: %v", h.plane.modules)
	}
	for _, ids := range h.plane.modules {
		if len(ids) != 2 {
			t.Fatalf("module issues: %v", ids)
		}
	}
	if len(h.fleet.tasks) != 2 {
		t.Fatal("backlog items must not dispatch")
	}
}

func TestSyncRetriedPublishReusesItems(t *testing.T) {
	h := newHarness(t)
	h.fleet.tasks = []*v1.Task{approvedPlan()}
	h.tick()
	// Lost the record of publishing (e.g. crashed before marking it).
	delete(h.links.plans, "plan1")
	h.links.links = map[string]Link{}
	h.tick()
	if len(h.plane.issues) != 2 || len(h.plane.modules) != 1 {
		t.Fatalf("issues %d modules %d: a retry must not duplicate", len(h.plane.issues), len(h.plane.modules))
	}
	if len(h.links.links) != 2 {
		t.Fatalf("links: %v", h.links.links)
	}
}

func TestSyncStoryLifecycle(t *testing.T) {
	h := newHarness(t)
	h.fleet.tasks = []*v1.Task{approvedPlan()}
	h.tick()
	item := h.plane.issues[0]["id"].(string)

	// A person hands the story to an agent.
	h.plane.move(item, "Ready for agent")
	h.tick()
	task := h.fleet.tasks[1]
	if task.ProcessType != "impl" || task.Name != "float-1-add-retry-with-jitter" || task.ParentId != "plan1" {
		t.Fatalf("task: %+v", task)
	}
	if task.Inputs["repo"] != "https://git.example/fx.git" || task.Inputs["tracker.key"] != "FLOAT-1" || task.Inputs["tracker.url"] == "" {
		t.Fatalf("inputs: %v", task.Inputs)
	}
	for _, want := range []string{"FLOAT-1: Add retry with jitter", "Wrap fetchRate.", "3 retries", "# Spec\nRetry everything."} {
		if !strings.Contains(task.Instructions, want) {
			t.Fatalf("instructions missing %q:\n%s", want, task.Instructions)
		}
	}
	if got := h.plane.stateName(item); got != "Agent working" {
		t.Fatalf("state %q", got)
	}
	h.tick()
	if len(h.fleet.tasks) != 2 {
		t.Fatal("dispatched twice")
	}

	// The agent asks something.
	task.Status = v1.TaskStatus_TASK_STATUS_WAITING
	task.Prompt = &v1.Prompt{Text: "approve bash(rm -rf build)?"}
	h.tick()
	if got := h.plane.stateName(item); got != "Needs you" {
		t.Fatalf("state %q", got)
	}
	if c := h.comments(item); !strings.Contains(c[len(c)-1], "approve bash(rm -rf build)?") {
		t.Fatalf("comments: %q", c)
	}

	// Answered; it opens a PR and parks for review.
	task.Status, task.Prompt = v1.TaskStatus_TASK_STATUS_PARKED, nil
	task.ParkOn = ReviewEvent
	task.Artefacts = []*v1.ArtefactRef{{Kind: "pr", Uri: "https://git.example/fx/pull/7"}}
	h.tick()
	if got := h.plane.stateName(item); got != "In review" {
		t.Fatalf("state %q", got)
	}
	if links := h.plane.links[item]; len(links) != 1 || links[0] != "https://git.example/fx/pull/7" {
		t.Fatalf("links: %v", links)
	}

	// The reviewer asks for changes.
	h.plane.addComment(item, "<p>use exponential backoff</p>", h.clock.Add(time.Millisecond))
	h.plane.move(item, "Ready for agent")
	h.tick()
	if len(h.fleet.events) != 1 {
		t.Fatalf("events: %v", h.fleet.events)
	}
	ev := strings.SplitN(h.fleet.events[0], " ", 3)
	var payload struct {
		State    string   `json:"state"`
		Comments []string `json:"comments"`
	}
	if err := json.Unmarshal([]byte(ev[2]), &payload); err != nil {
		t.Fatal(err)
	}
	if ev[0] != task.Id || ev[1] != ReviewEvent || payload.State != "changes_requested" || len(payload.Comments) != 1 || payload.Comments[0] != "use exponential backoff" {
		t.Fatalf("event: %v", h.fleet.events[0])
	}
	if len(h.fleet.tasks) != 2 {
		t.Fatal("review feedback must resume the task, not dispatch a new one")
	}
	if got := h.plane.stateName(item); got != "Agent working" {
		t.Fatalf("state %q", got)
	}

	// Feedback addressed, back in review; the reviewer accepts.
	task.Status = v1.TaskStatus_TASK_STATUS_RUNNING
	h.tick()
	task.Status = v1.TaskStatus_TASK_STATUS_PARKED
	h.tick()
	h.plane.move(item, "Done")
	h.tick()
	if len(h.fleet.events) != 2 || !strings.Contains(h.fleet.events[1], `"state":"approved"`) {
		t.Fatalf("events: %v", h.fleet.events)
	}
	// The agent wraps up; the item stays Done throughout.
	task.Status = v1.TaskStatus_TASK_STATUS_RUNNING
	h.tick()
	if got := h.plane.stateName(item); got != "Done" {
		t.Fatalf("accepted item moved to %q", got)
	}
	task.Status = v1.TaskStatus_TASK_STATUS_SUCCEEDED
	h.tick()
	if l := h.links.links[item]; !l.Settled || l.SyncedState != StateDone {
		t.Fatalf("link: %+v", l)
	}
	if len(h.fleet.events) != 2 {
		t.Fatal("done item re-approved")
	}
}

func TestSyncFailedTaskRetriesWhenMovedToReady(t *testing.T) {
	h := newHarness(t)
	h.fleet.tasks = []*v1.Task{approvedPlan()}
	h.tick()
	item := h.plane.issues[1]["id"].(string)
	h.plane.move(item, "Ready for agent")
	h.tick()
	first := h.fleet.tasks[1]
	first.Status, first.Error = v1.TaskStatus_TASK_STATUS_FAILED, "tests failed"
	h.tick()
	if got := h.plane.stateName(item); got != "Needs you" {
		t.Fatalf("state %q", got)
	}
	if c := h.comments(item); !strings.Contains(c[len(c)-1], "agent failed: tests failed") {
		t.Fatalf("comments: %q", c)
	}

	h.plane.move(item, "Ready for agent")
	h.tick()
	if len(h.fleet.tasks) != 3 || h.fleet.tasks[2].Name != "float-2-alert-on-stale-rates-r2" {
		t.Fatalf("retry: %v", h.fleet.tasks)
	}
	if l := h.links.links[item]; l.TaskID != "t3" || l.Dispatches != 2 {
		t.Fatalf("link: %+v", l)
	}
}

func TestSyncDispatchesManualItemsAndStopsCancelled(t *testing.T) {
	h := newHarness(t)
	it, err := h.sync.Tracker.Create(context.Background(), NewItem{Title: "Fix typo in README", Description: "teh → the", State: StateReady})
	if err != nil {
		t.Fatal(err)
	}
	h.tick()
	if len(h.fleet.tasks) != 1 || h.fleet.tasks[0].ParentId != "" || h.fleet.tasks[0].Inputs["tracker.item"] != it.ID {
		t.Fatalf("tasks: %v", h.fleet.tasks)
	}
	h.plane.move(it.ID, "Cancelled")
	h.tick()
	if len(h.fleet.stops) != 1 || !strings.HasPrefix(h.fleet.stops[0], "t1 cancelled in plane") {
		t.Fatalf("stops: %v", h.fleet.stops)
	}
	h.fleet.tasks[0].Status = v1.TaskStatus_TASK_STATUS_FAILED
	h.tick()
	if got := h.plane.stateName(it.ID); got != "Cancelled" {
		t.Fatalf("cancelled item moved to %q", got)
	}
}

func TestSyncPutsDraggedItemBack(t *testing.T) {
	h := newHarness(t)
	it, _ := h.sync.Tracker.Create(context.Background(), NewItem{Title: "x", State: StateReady})
	h.tick()
	h.fleet.tasks[0].Status = v1.TaskStatus_TASK_STATUS_RUNNING
	h.plane.move(it.ID, "Ready for agent")
	h.tick()
	if len(h.fleet.tasks) != 1 {
		t.Fatal("an item whose agent is still working must not dispatch again")
	}
	if got := h.plane.stateName(it.ID); got != "Agent working" {
		t.Fatalf("state %q", got)
	}
}

func (h *harness) lastComment(issueID string) string {
	c := h.comments(issueID)
	if len(c) == 0 {
		return ""
	}
	return c[len(c)-1]
}

func TestSyncPlanItemLifecycle(t *testing.T) {
	h := newHarness(t)
	h.sync.Repo = "https://git.example/default.git"
	ctx := context.Background()
	it, err := h.sync.Tracker.Create(ctx, NewItem{Title: "Make FX fetches resilient", Description: "Rates go stale.\nrepo: https://git.example/fx.git", State: StateBacklog})
	if err != nil {
		t.Fatal(err)
	}
	h.plane.label(it.ID, PlanLabel)
	h.plane.move(it.ID, "Ready for agent")
	h.tick()

	plan := h.fleet.tasks[0]
	if plan.ProcessType != "plan" || plan.Inputs["repo"] != "https://git.example/fx.git" || plan.Inputs["tracker.item"] != it.ID {
		t.Fatalf("plan task: %+v", plan)
	}
	if !strings.Contains(h.lastComment(it.ID), "plan agent float-1-make-fx-fetches-resilient picked this up") {
		t.Fatalf("comment: %q", h.lastComment(it.ID))
	}

	// The agent publishes stories and asks for approval.
	plan.Status = v1.TaskStatus_TASK_STATUS_WAITING
	plan.Prompt = &v1.Prompt{Id: "p1", Kind: v1.PromptKind_PROMPT_KIND_APPROVAL, Text: "approve the spec and 2 stories?"}
	plan.Spec = "# Spec\nRetry everything."
	plan.Stories = approvedPlan().Stories
	h.tick()
	if got := h.plane.stateName(it.ID); got != "In review" {
		t.Fatalf("state %q", got)
	}
	c := h.lastComment(it.ID)
	for _, want := range []string{"approval needed: approve the spec and 2 stories?", "Add retry with jitter", "Retry everything."} {
		if !strings.Contains(c, want) {
			t.Fatalf("comment missing %q: %q", want, c)
		}
	}

	// Changes requested from the board.
	h.plane.addComment(it.ID, "<p>split s1 in two</p>", h.clock.Add(time.Millisecond))
	h.plane.move(it.ID, "Ready for agent")
	h.tick()
	if len(h.fleet.answers) != 1 || h.fleet.answers[0] != `t1 p1 approved=false "split s1 in two"` {
		t.Fatalf("answers: %v", h.fleet.answers)
	}
	if got := h.plane.stateName(it.ID); got != "Agent working" || len(h.fleet.tasks) != 1 {
		t.Fatalf("state %q, tasks %d", got, len(h.fleet.tasks))
	}

	// Revised; approved on the board.
	plan.Status = v1.TaskStatus_TASK_STATUS_WAITING
	plan.Prompt = &v1.Prompt{Id: "p2", Kind: v1.PromptKind_PROMPT_KIND_APPROVAL, Text: "approve the revised spec?"}
	h.tick()
	h.plane.move(it.ID, "Done")
	h.tick()
	if len(h.fleet.answers) != 2 || h.fleet.answers[1] != `t1 p2 approved=true ""` {
		t.Fatalf("answers: %v", h.fleet.answers)
	}
	h.tick() // running again: the item stays Done
	if got := h.plane.stateName(it.ID); got != "Done" {
		t.Fatalf("approved plan moved to %q", got)
	}

	// The plan finishes: its stories become children of the plan item.
	plan.Status = v1.TaskStatus_TASK_STATUS_SUCCEEDED
	h.tick()
	if len(h.plane.issues) != 3 {
		t.Fatalf("issues: %d", len(h.plane.issues))
	}
	for _, child := range h.plane.issues[1:] {
		if child["parent"] != it.ID || h.plane.stateName(child["id"].(string)) != "Backlog" {
			t.Fatalf("story: %v", child)
		}
	}
	if len(h.plane.modules) != 0 {
		t.Fatalf("stories with a parent need no module: %v", h.plane.modules)
	}
	if l := h.links.links[it.ID]; !l.Settled {
		t.Fatalf("plan link: %+v", l)
	}

	// A story handed to an agent gets the plan's repo and spec.
	story := h.plane.issues[1]["id"].(string)
	h.plane.move(story, "Ready for agent")
	h.tick()
	impl := h.fleet.tasks[1]
	if impl.ProcessType != "impl" || impl.ParentId != "t1" || impl.Inputs["repo"] != "https://git.example/fx.git" || !strings.Contains(impl.Instructions, "Retry everything.") {
		t.Fatalf("impl task: %+v", impl)
	}
}

func TestSyncAnswersQuestionsAndPermissionsFromTheBoard(t *testing.T) {
	h := newHarness(t)
	h.sync.Repo = "https://git.example/default.git"
	it, _ := h.sync.Tracker.Create(context.Background(), NewItem{Title: "Fix typo", State: StateReady})
	h.tick()
	task := h.fleet.tasks[0]
	if task.Inputs["repo"] != "https://git.example/default.git" {
		t.Fatalf("inputs: %v", task.Inputs)
	}

	task.Status = v1.TaskStatus_TASK_STATUS_WAITING
	task.Prompt = &v1.Prompt{Id: "q1", Kind: v1.PromptKind_PROMPT_KIND_QUESTION, Text: "which branch?"}
	h.tick()
	if got := h.plane.stateName(it.ID); got != "Needs you" || !strings.Contains(h.lastComment(it.ID), "question: which branch?") {
		t.Fatalf("state %q comment %q", got, h.lastComment(it.ID))
	}
	h.plane.addComment(it.ID, "<p>main</p>", h.clock.Add(time.Millisecond))
	h.plane.move(it.ID, "Ready for agent")
	h.tick()

	task.Status = v1.TaskStatus_TASK_STATUS_WAITING
	task.Prompt = &v1.Prompt{Id: "x1", Kind: v1.PromptKind_PROMPT_KIND_PERMISSION, Text: "bash(git push)", Detail: "git push origin HEAD"}
	h.tick()
	if c := h.lastComment(it.ID); !strings.Contains(c, "needs permission: bash(git push)") || !strings.Contains(c, "git push origin HEAD") {
		t.Fatalf("comment %q", c)
	}
	h.plane.move(it.ID, "Ready for agent")
	h.tick()

	want := []string{`t1 q1 approved=true "main"`, `t1 x1 approved=true ""`}
	if strings.Join(h.fleet.answers, "|") != strings.Join(want, "|") {
		t.Fatalf("answers: %v", h.fleet.answers)
	}
	if len(h.fleet.tasks) != 1 {
		t.Fatal("answering must not dispatch")
	}
}

func TestSyncHoldsBlockedItemsUntilBlockersAreDone(t *testing.T) {
	h := newHarness(t)
	plan := approvedPlan()
	plan.Stories = append(plan.Stories, &v1.Story{Key: "s3", Title: "Dashboard for retries", DependsOn: []string{"s1", "s2"}})
	plan.Stories[1].DependsOn = []string{"s1"}
	h.fleet.tasks = []*v1.Task{plan}
	h.tick()
	s1, s2, s3 := h.plane.issues[0]["id"].(string), h.plane.issues[1]["id"].(string), h.plane.issues[2]["id"].(string)
	if got := h.plane.blockedBy; len(got) != 2 || strings.Join(got[s2], ",") != s1 || strings.Join(got[s3], ",") != s1+","+s2 {
		t.Fatalf("relations: %v", got)
	}

	// Everything handed over at once: only s1 starts.
	for _, id := range []string{s1, s2, s3} {
		h.plane.move(id, "Ready for agent")
	}
	h.tick()
	if len(h.fleet.tasks) != 2 || h.fleet.tasks[1].Inputs["tracker.item"] != s1 {
		t.Fatalf("tasks: %v", h.fleet.tasks)
	}
	if got := h.plane.stateName(s2); got != "Ready for agent" {
		t.Fatalf("blocked item moved to %q", got)
	}
	if c := h.lastComment(s3); c != "floatilla · queued: blocked by FLOAT-1 (working), FLOAT-2 (ready). Starts when they are done." {
		t.Fatalf("comment: %q", c)
	}
	h.tick()
	if n := len(h.comments(s3)); n != 1 {
		t.Fatalf("blocked comment repeated: %d comments", n)
	}

	// s1 accepted: s2 starts on s1's branch; s3 still waits on s2.
	first := h.fleet.tasks[1]
	first.Branch = "agent/" + first.Name
	first.Status = v1.TaskStatus_TASK_STATUS_SUCCEEDED
	h.tick() // the item follows its task to Done
	h.tick() // s2 sees its blocker done
	if len(h.fleet.tasks) != 3 {
		t.Fatalf("tasks: %d", len(h.fleet.tasks))
	}
	second := h.fleet.tasks[2]
	if second.Inputs["tracker.item"] != s2 || second.Inputs["after_branches"] != "agent/float-1-add-retry-with-jitter" {
		t.Fatalf("second: %v", second.Inputs)
	}
	if !strings.Contains(second.Instructions, "FLOAT-1 (branch agent/float-1-add-retry-with-jitter)") {
		t.Fatalf("instructions: %s", second.Instructions)
	}
	if c := h.lastComment(s3); !strings.Contains(c, "blocked by FLOAT-2 (working).") {
		t.Fatalf("comment: %q", c)
	}
	if l := h.links.links[s2]; l.BlockedOn != "" {
		t.Fatalf("dispatched link still blocked: %+v", l)
	}
}

func TestSyncIgnoresCancelledAndOffBoardBlockers(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	gone, _ := h.sync.Tracker.Create(ctx, NewItem{Title: "dropped", State: StateCancelled})
	it, _ := h.sync.Tracker.Create(ctx, NewItem{Title: "x", State: StateReady})
	h.plane.blockedBy[it.ID] = []string{gone.ID, "item-in-another-project"}
	h.tick()
	if len(h.fleet.tasks) != 1 {
		t.Fatal("cancelled and unknown blockers must not hold the item")
	}
}

func TestTaskName(t *testing.T) {
	for _, c := range []struct {
		key, title string
		n          int
		want       string
	}{
		{"FLOAT-12", "Add retry (with jitter!) to FX", 1, "float-12-add-retry-with-jitter-to-fx"},
		{"FLOAT-3", "An extremely long title that goes on and on forever", 1, "float-3-an-extremely-long-title-that"},
		{"FLOAT-3", "Ünïcode — only", 3, "float-3-n-code-only-r3"},
	} {
		if got := taskName(c.key, c.title, c.n); got != c.want {
			t.Errorf("taskName(%q, %q) = %q, want %q", c.key, c.title, got, c.want)
		}
	}
}
