package fsm

import (
	"errors"
	"testing"
	"time"

	v1 "github.com/squash-box/floatilla/src/gen/go/floatilla/v1"
)

var t0 = time.Date(2026, 10, 24, 14, 0, 0, 0, time.UTC)

type harness struct {
	t    *testing.T
	task *v1.Task
	now  time.Time
	cfg  Config
}

func newHarness(t *testing.T) (*harness, []Effect) {
	task, effects := New(&v1.Task{Id: "t1", Name: "auth-refactor", ProcessType: "impl", Workflow: "feature"}, t0)
	return &harness{t: t, task: task, now: t0, cfg: DefaultConfig()}, effects
}

func (h *harness) apply(in Input) []Effect {
	h.t.Helper()
	next, effects, err := Apply(h.task, in, h.now, h.cfg)
	if err != nil {
		h.t.Fatalf("apply %T: %v", in, err)
	}
	h.task = next
	return effects
}

func (h *harness) ev(e *v1.AgentEvent) []Effect {
	h.t.Helper()
	if e.RunSeq == 0 {
		e.RunSeq = h.task.RunSeq
	}
	e.TaskId = h.task.Id
	return h.apply(RunEvent{Event: e})
}

func (h *harness) wantStatus(s v1.TaskStatus) {
	h.t.Helper()
	if h.task.Status != s {
		h.t.Fatalf("status = %s, want %s", h.task.Status, s)
	}
}

func started() *v1.AgentEvent {
	return &v1.AgentEvent{Body: &v1.AgentEvent_RunStarted{RunStarted: &v1.RunStarted{}}}
}

func finished(o v1.Outcome, parkOn string) *v1.AgentEvent {
	return &v1.AgentEvent{Body: &v1.AgentEvent_RunFinished{RunFinished: &v1.RunFinished{Outcome: o, ParkOn: parkOn}}}
}

func singleEnqueue(t *testing.T, effects []Effect) *v1.RunRequest {
	t.Helper()
	if len(effects) != 1 {
		t.Fatalf("effects = %v, want one EnqueueRun", effects)
	}
	e, ok := effects[0].(EnqueueRun)
	if !ok {
		t.Fatalf("effect = %T, want EnqueueRun", effects[0])
	}
	return e.Request
}

func TestFullLifecycle(t *testing.T) {
	h, effects := newHarness(t)
	h.wantStatus(v1.TaskStatus_TASK_STATUS_QUEUED)
	if req := singleEnqueue(t, effects); req.RunSeq != 1 {
		t.Fatalf("first run_seq = %d", req.RunSeq)
	}

	h.ev(started())
	h.wantStatus(v1.TaskStatus_TASK_STATUS_RUNNING)
	if h.task.LeaseExpiresAt == nil {
		t.Fatal("no lease after run_started")
	}

	h.ev(&v1.AgentEvent{Body: &v1.AgentEvent_StageStarted{StageStarted: &v1.StageStarted{Index: 1, Name: "implementation"}}})
	if h.task.StageIndex != 1 || len(h.task.Stages) != 1 {
		t.Fatalf("stage not recorded: %v", h.task.Stages)
	}

	// Permission prompt round trip.
	h.ev(&v1.AgentEvent{Body: &v1.AgentEvent_PromptRaised{PromptRaised: &v1.PromptRaised{
		Prompt: &v1.Prompt{Id: "p1", Kind: v1.PromptKind_PROMPT_KIND_PERMISSION, Text: "approve bash(make test)?"},
	}}})
	h.wantStatus(v1.TaskStatus_TASK_STATUS_WAITING)

	effects = h.apply(AnswerPrompt{Answer: &v1.PromptAnswer{Approved: true}})
	h.wantStatus(v1.TaskStatus_TASK_STATUS_RUNNING)
	if h.task.Activity != "approved: approve bash(make test)?" {
		t.Fatalf("activity after answer = %q", h.task.Activity)
	}
	cmd := effects[0].(SendCommand).Command
	if cmd.GetAnswer().GetPromptId() != "p1" || !cmd.GetAnswer().Approved || cmd.RunSeq != 1 {
		t.Fatalf("answer command = %v", cmd)
	}
	h.ev(&v1.AgentEvent{Body: &v1.AgentEvent_PromptResolved{PromptResolved: &v1.PromptResolved{PromptId: "p1"}}})
	h.wantStatus(v1.TaskStatus_TASK_STATUS_RUNNING)

	h.ev(&v1.AgentEvent{Body: &v1.AgentEvent_Usage{Usage: &v1.UsageDelta{Usage: &v1.Usage{InputTokens: 100, CostUsd: 0.5}}}})
	h.ev(&v1.AgentEvent{Body: &v1.AgentEvent_Usage{Usage: &v1.UsageDelta{Usage: &v1.Usage{InputTokens: 50, CostUsd: 0.25}}}})
	if h.task.Usage.InputTokens != 150 || h.task.Usage.CostUsd != 0.75 {
		t.Fatalf("usage = %v", h.task.Usage)
	}

	// Park on review, then resume via external event into a new run.
	h.ev(&v1.AgentEvent{Body: &v1.AgentEvent_StageStarted{StageStarted: &v1.StageStarted{Index: 4, Name: "awaiting review"}}})
	h.ev(finished(v1.Outcome_OUTCOME_PARKED, "pr.review"))
	h.wantStatus(v1.TaskStatus_TASK_STATUS_PARKED)
	if h.task.LeaseExpiresAt != nil {
		t.Fatal("parked task kept its lease")
	}

	if _, _, err := Apply(h.task, ExternalEvent{Event: &v1.ExternalEvent{Key: "other"}}, h.now, h.cfg); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("wrong event key: err = %v", err)
	}
	effects = h.apply(ExternalEvent{Event: &v1.ExternalEvent{Key: "pr.review", PayloadJson: `{"state":"changes_requested"}`}})
	h.wantStatus(v1.TaskStatus_TASK_STATUS_QUEUED)
	req := singleEnqueue(t, effects)
	if req.RunSeq != 2 || req.Resume.GetEvent().GetKey() != "pr.review" {
		t.Fatalf("resume request = %v", req)
	}
	if rec := h.task.Stages[len(h.task.Stages)-1]; rec.Index != 4 || rec.FinishedAt == nil {
		t.Fatalf("park stage not finished: %v", rec)
	}

	// A late event from run 1 is ignored.
	h.ev(&v1.AgentEvent{RunSeq: 1, Body: &v1.AgentEvent_RunFinished{RunFinished: &v1.RunFinished{Outcome: v1.Outcome_OUTCOME_FAILED}}})
	h.wantStatus(v1.TaskStatus_TASK_STATUS_QUEUED)

	h.ev(started())
	h.ev(finished(v1.Outcome_OUTCOME_SUCCEEDED, ""))
	h.wantStatus(v1.TaskStatus_TASK_STATUS_SUCCEEDED)
	if h.task.FinishedAt == nil {
		t.Fatal("no finished_at")
	}
	if h.task.Usage.InputTokens != 150 {
		t.Fatal("usage lost across runs")
	}
}

func TestLeaseExpiryRetriesThenFails(t *testing.T) {
	h, _ := newHarness(t)
	h.ev(started())

	h.now = h.now.Add(10 * time.Second)
	if effects := h.apply(LeaseCheck{}); len(effects) != 0 {
		t.Fatal("lease expired early")
	}

	for attempt := uint32(2); attempt <= h.cfg.MaxAttempts; attempt++ {
		h.now = h.now.Add(h.cfg.LeaseTTL)
		lostRun := h.task.RunSeq
		effects := h.apply(LeaseCheck{})
		if len(effects) != 2 {
			t.Fatalf("effects = %v, want stop + enqueue", effects)
		}
		if stop := effects[0].(SendCommand).Command; stop.GetStop() == nil || stop.RunSeq != lostRun {
			t.Fatalf("lost run not stopped: %v", stop)
		}
		req := singleEnqueue(t, effects[1:])
		h.wantStatus(v1.TaskStatus_TASK_STATUS_QUEUED)
		if h.task.Attempt != attempt || req.Task.Attempt != attempt {
			t.Fatalf("attempt = %d (req %d), want %d", h.task.Attempt, req.Task.Attempt, attempt)
		}
		h.ev(started())
	}

	h.now = h.now.Add(h.cfg.LeaseTTL)
	if effects := h.apply(LeaseCheck{}); len(effects) != 0 {
		t.Fatalf("retried past MaxAttempts: %v", effects)
	}
	h.wantStatus(v1.TaskStatus_TASK_STATUS_FAILED)
}

func TestHeartbeatExtendsLease(t *testing.T) {
	h, _ := newHarness(t)
	h.ev(started())
	h.now = h.now.Add(40 * time.Second)
	h.ev(&v1.AgentEvent{Body: &v1.AgentEvent_Heartbeat{Heartbeat: &v1.Heartbeat{}}})
	h.now = h.now.Add(40 * time.Second)
	if effects := h.apply(LeaseCheck{}); len(effects) != 0 {
		t.Fatal("heartbeat did not extend the lease")
	}
	h.wantStatus(v1.TaskStatus_TASK_STATUS_RUNNING)
}

func TestStopSendsStopToLiveRun(t *testing.T) {
	h, _ := newHarness(t)
	h.ev(started())
	effects := h.apply(Stop{})
	h.wantStatus(v1.TaskStatus_TASK_STATUS_FAILED)
	if effects[0].(SendCommand).Command.GetStop() == nil {
		t.Fatalf("effects = %v", effects)
	}
	// The run's own stopped report is a no-op on a terminal task.
	h.ev(finished(v1.Outcome_OUTCOME_STOPPED, ""))
	if h.task.Error != "stopped by user" {
		t.Fatalf("error overwritten: %q", h.task.Error)
	}
	if _, _, err := Apply(h.task, Stop{}, h.now, h.cfg); !errors.Is(err, ErrInvalidTransition) {
		t.Fatal("stopping a finished task should fail")
	}
}

func TestPauseResume(t *testing.T) {
	h, _ := newHarness(t)
	h.ev(started())
	h.apply(Pause{})
	h.wantStatus(v1.TaskStatus_TASK_STATUS_PAUSED)
	h.ev(finished(v1.Outcome_OUTCOME_STOPPED, ""))
	h.wantStatus(v1.TaskStatus_TASK_STATUS_PAUSED)

	req := singleEnqueue(t, h.apply(Resume{}))
	h.wantStatus(v1.TaskStatus_TASK_STATUS_QUEUED)
	if req.RunSeq != 2 {
		t.Fatalf("resume run_seq = %d", req.RunSeq)
	}
}

func TestAnswerRequiresMatchingPrompt(t *testing.T) {
	h, _ := newHarness(t)
	if _, _, err := Apply(h.task, AnswerPrompt{Answer: &v1.PromptAnswer{}}, h.now, h.cfg); !errors.Is(err, ErrInvalidTransition) {
		t.Fatal("answering a queued task should fail")
	}
	h.ev(started())
	h.ev(&v1.AgentEvent{Body: &v1.AgentEvent_PromptRaised{PromptRaised: &v1.PromptRaised{Prompt: &v1.Prompt{Id: "p1"}}}})
	if _, _, err := Apply(h.task, AnswerPrompt{Answer: &v1.PromptAnswer{PromptId: "p0"}}, h.now, h.cfg); !errors.Is(err, ErrInvalidTransition) {
		t.Fatal("stale prompt id should fail")
	}
}

func TestEventsBeforeRunStartedImplyStart(t *testing.T) {
	h, _ := newHarness(t)
	h.ev(&v1.AgentEvent{Body: &v1.AgentEvent_StageStarted{StageStarted: &v1.StageStarted{Index: 0, Name: "setup"}}})
	h.wantStatus(v1.TaskStatus_TASK_STATUS_RUNNING)
	h.ev(started())
	h.wantStatus(v1.TaskStatus_TASK_STATUS_RUNNING)
}

func TestApplyDoesNotMutateInput(t *testing.T) {
	h, _ := newHarness(t)
	before := h.task
	_, _, _ = Apply(before, RunEvent{Event: &v1.AgentEvent{RunSeq: 1, Body: &v1.AgentEvent_RunStarted{RunStarted: &v1.RunStarted{}}}}, h.now, h.cfg)
	if before.Status != v1.TaskStatus_TASK_STATUS_QUEUED {
		t.Fatal("Apply mutated its input")
	}
}

func TestStoriesPublishedReplacesStories(t *testing.T) {
	h, _ := newHarness(t)
	h.ev(started())
	pub := func(keys ...string) {
		var stories []*v1.Story
		for _, k := range keys {
			stories = append(stories, &v1.Story{Key: k, Title: "story " + k})
		}
		h.ev(&v1.AgentEvent{Body: &v1.AgentEvent_StoriesPublished{StoriesPublished: &v1.StoriesPublished{Stories: stories, Spec: "# spec " + keys[0]}}})
	}
	pub("s1", "s2")
	pub("s1", "s2", "s3") // a revised plan replaces the earlier stories
	if len(h.task.Stories) != 3 || h.task.Spec != "# spec s1" {
		t.Fatalf("stories = %v, spec = %q", h.task.Stories, h.task.Spec)
	}
}
