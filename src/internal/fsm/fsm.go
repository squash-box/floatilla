// Package fsm is the task state machine. It is pure: Apply takes a task and an
// input and returns the next task plus the side effects to perform. Persisting
// the task and delivering effects atomically is the store's job.
package fsm

import (
	"errors"
	"fmt"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/squash-box/floatilla/src/gen/go/floatilla/v1"
)

// ErrInvalidTransition is returned when an input doesn't apply to the task's
// current state (e.g. answering a task that isn't waiting).
var ErrInvalidTransition = errors.New("invalid transition")

type Config struct {
	// LeaseTTL is how long a run may go without a heartbeat.
	LeaseTTL time.Duration
	// MaxAttempts bounds lease-expiry retries of one stage.
	MaxAttempts uint32
	// TailLines is how many log lines a task keeps for tiles.
	TailLines int
}

func DefaultConfig() Config {
	return Config{LeaseTTL: 45 * time.Second, MaxAttempts: 3, TailLines: 2}
}

// Inputs.

type Input interface{ isInput() }

// RunEvent is an event from a run (supervisor or launcher).
type RunEvent struct{ Event *v1.AgentEvent }

type AnswerPrompt struct{ Answer *v1.PromptAnswer }

type ExternalEvent struct{ Event *v1.ExternalEvent }

type Stop struct{ Reason string }

type Pause struct{}

type Resume struct{}

// LeaseCheck is applied periodically; it expires runs whose lease lapsed.
type LeaseCheck struct{}

func (RunEvent) isInput()      {}
func (AnswerPrompt) isInput()  {}
func (ExternalEvent) isInput() {}
func (Stop) isInput()          {}
func (Pause) isInput()         {}
func (Resume) isInput()        {}
func (LeaseCheck) isInput()    {}

// Effects.

type Effect interface{ isEffect() }

// EnqueueRun puts a run request on the process type's queue.
type EnqueueRun struct{ Request *v1.RunRequest }

// SendCommand delivers a command to the task's live run.
type SendCommand struct{ Command *v1.Command }

func (EnqueueRun) isEffect()  {}
func (SendCommand) isEffect() {}

// New creates a queued task and its first run request.
func New(t *v1.Task, now time.Time) (*v1.Task, []Effect) {
	t = proto.Clone(t).(*v1.Task)
	t.Status = v1.TaskStatus_TASK_STATUS_QUEUED
	t.RunSeq = 1
	t.Attempt = 1
	t.Usage = &v1.Usage{}
	t.CreatedAt = timestamppb.New(now)
	t.UpdatedAt = t.CreatedAt
	return t, []Effect{enqueue(t, nil)}
}

// Apply returns the task after applying in, and the effects to perform. The
// input task is not modified. A nil error with no change is a valid no-op
// (e.g. an event from a superseded run).
func Apply(cur *v1.Task, in Input, now time.Time, cfg Config) (*v1.Task, []Effect, error) {
	t := proto.Clone(cur).(*v1.Task)
	var effects []Effect
	var err error

	switch in := in.(type) {
	case RunEvent:
		effects, err = applyRunEvent(t, in.Event, now, cfg)
	case AnswerPrompt:
		effects, err = applyAnswer(t, in.Answer)
	case ExternalEvent:
		effects, err = applyExternal(t, in.Event, now)
	case Stop:
		effects, err = applyStop(t, in.Reason, now)
	case Pause:
		effects, err = applyPause(t)
	case Resume:
		effects, err = applyResume(t)
	case LeaseCheck:
		effects = applyLeaseCheck(t, now, cfg)
	default:
		err = fmt.Errorf("fsm: unknown input %T", in)
	}
	if err != nil {
		return cur, nil, err
	}
	if !proto.Equal(t, cur) {
		t.UpdatedAt = timestamppb.New(now)
	}
	return t, effects, nil
}

func IsTerminal(s v1.TaskStatus) bool {
	return s == v1.TaskStatus_TASK_STATUS_SUCCEEDED || s == v1.TaskStatus_TASK_STATUS_FAILED
}

// hasLiveRun reports whether the current run_seq is expected to be executing.
func hasLiveRun(s v1.TaskStatus) bool {
	return s == v1.TaskStatus_TASK_STATUS_RUNNING || s == v1.TaskStatus_TASK_STATUS_WAITING
}

func applyRunEvent(t *v1.Task, ev *v1.AgentEvent, now time.Time, cfg Config) ([]Effect, error) {
	// Events from superseded runs, or arriving after the task stopped
	// executing, are dropped. Queued is allowed so run_started can land.
	if ev.GetRunSeq() != t.RunSeq || !(hasLiveRun(t.Status) || t.Status == v1.TaskStatus_TASK_STATUS_QUEUED) {
		return nil, nil
	}
	if t.Status == v1.TaskStatus_TASK_STATUS_QUEUED && ev.GetRunStarted() == nil && ev.GetRunFinished() == nil {
		// Out-of-order delivery before run_started; treat it as started.
		markStarted(t, now, cfg)
	}

	switch b := ev.Body.(type) {
	case *v1.AgentEvent_RunStarted:
		if t.Status == v1.TaskStatus_TASK_STATUS_QUEUED {
			markStarted(t, now, cfg)
		}
	case *v1.AgentEvent_Heartbeat:
		t.LeaseExpiresAt = timestamppb.New(now.Add(cfg.LeaseTTL))
	case *v1.AgentEvent_StageStarted:
		t.StageIndex = b.StageStarted.Index
		t.StageProgress = nil
		t.Activity = "starting " + b.StageStarted.Name
		rec := stageRecord(t, b.StageStarted.Index, b.StageStarted.Name)
		if rec.StartedAt == nil {
			rec.StartedAt = timestamppb.New(now)
		}
	case *v1.AgentEvent_StageFinished:
		rec := stageRecord(t, b.StageFinished.Index, "")
		rec.FinishedAt = timestamppb.New(now)
		rec.Summary = b.StageFinished.Summary
	case *v1.AgentEvent_Progress:
		p := b.Progress
		if p.Activity != "" {
			t.Activity = p.Activity
		}
		if p.StageProgress != nil {
			t.StageProgress = proto.Float32(*p.StageProgress)
		}
		if p.ContextUsed > 0 {
			t.ContextUsed = p.ContextUsed
		}
		if len(p.Tail) > 0 {
			tail := p.Tail
			if len(tail) > cfg.TailLines {
				tail = tail[len(tail)-cfg.TailLines:]
			}
			t.Tail = append([]string(nil), tail...)
		}
	case *v1.AgentEvent_Artefact:
		upsertArtefact(t, b.Artefact, now)
	case *v1.AgentEvent_PromptRaised:
		t.Status = v1.TaskStatus_TASK_STATUS_WAITING
		t.Prompt = b.PromptRaised.Prompt
		t.Activity = b.PromptRaised.Prompt.GetText()
	case *v1.AgentEvent_PromptResolved:
		if t.Prompt.GetId() == b.PromptResolved.PromptId {
			t.Prompt = nil
		}
		if t.Status == v1.TaskStatus_TASK_STATUS_WAITING {
			t.Status = v1.TaskStatus_TASK_STATUS_RUNNING
		}
	case *v1.AgentEvent_Usage:
		if t.Usage == nil {
			t.Usage = &v1.Usage{}
		}
		addUsage(t.Usage, b.Usage.GetUsage())
	case *v1.AgentEvent_StoriesPublished:
		t.Stories = b.StoriesPublished.Stories
		t.Spec = b.StoriesPublished.Spec
	case *v1.AgentEvent_RunFinished:
		finishRun(t, b.RunFinished, now)
	case *v1.AgentEvent_Log:
		// Log lines are not task state; the supervisor folds them into
		// Progress.tail.
	}
	return nil, nil
}

func markStarted(t *v1.Task, now time.Time, cfg Config) {
	t.Status = v1.TaskStatus_TASK_STATUS_RUNNING
	if t.StartedAt == nil {
		t.StartedAt = timestamppb.New(now)
	}
	t.LeaseExpiresAt = timestamppb.New(now.Add(cfg.LeaseTTL))
}

func finishRun(t *v1.Task, f *v1.RunFinished, now time.Time) {
	t.LeaseExpiresAt = nil
	t.Prompt = nil
	switch f.Outcome {
	case v1.Outcome_OUTCOME_SUCCEEDED:
		t.Status = v1.TaskStatus_TASK_STATUS_SUCCEEDED
		t.FinishedAt = timestamppb.New(now)
		t.StageProgress = nil
		t.Activity = "done"
	case v1.Outcome_OUTCOME_PARKED:
		t.Status = v1.TaskStatus_TASK_STATUS_PARKED
		t.ParkOn = f.ParkOn
		t.Activity = "waiting for " + f.ParkOn
	default: // failed, stopped, unspecified
		t.Status = v1.TaskStatus_TASK_STATUS_FAILED
		t.FinishedAt = timestamppb.New(now)
		t.Error = f.Error
		if t.Error == "" {
			t.Error = "run ended: " + f.Outcome.String()
		}
		t.Activity = t.Error
	}
}

func applyAnswer(t *v1.Task, a *v1.PromptAnswer) ([]Effect, error) {
	if t.Status != v1.TaskStatus_TASK_STATUS_WAITING || t.Prompt == nil {
		return nil, fmt.Errorf("%w: task is %s, not waiting", ErrInvalidTransition, t.Status)
	}
	if a.GetPromptId() == "" {
		a = proto.Clone(a).(*v1.PromptAnswer)
		a.PromptId = t.Prompt.Id
	} else if a.PromptId != t.Prompt.Id {
		return nil, fmt.Errorf("%w: prompt %q is not pending (pending %q)", ErrInvalidTransition, a.PromptId, t.Prompt.Id)
	}
	// Optimistically clear the prompt so every client sees the answer land;
	// the run confirms with prompt_resolved.
	t.Activity = answerActivity(t.Prompt, a)
	t.Prompt = nil
	t.Status = v1.TaskStatus_TASK_STATUS_RUNNING
	return []Effect{SendCommand{Command: &v1.Command{
		TaskId: t.Id,
		RunSeq: t.RunSeq,
		Body:   &v1.Command_Answer{Answer: a},
	}}}, nil
}

func answerActivity(p *v1.Prompt, a *v1.PromptAnswer) string {
	if p.Kind == v1.PromptKind_PROMPT_KIND_QUESTION {
		return "answered: " + a.Text
	}
	if a.Approved {
		return "approved: " + p.Text
	}
	return "denied: " + p.Text
}

func applyExternal(t *v1.Task, ev *v1.ExternalEvent, now time.Time) ([]Effect, error) {
	if t.Status != v1.TaskStatus_TASK_STATUS_PARKED {
		return nil, fmt.Errorf("%w: task is %s, not parked", ErrInvalidTransition, t.Status)
	}
	if ev.GetKey() != t.ParkOn {
		return nil, fmt.Errorf("%w: task is parked on %q, got %q", ErrInvalidTransition, t.ParkOn, ev.GetKey())
	}
	// The park stage is complete; the next run starts at the following stage.
	rec := stageRecord(t, t.StageIndex, "")
	rec.FinishedAt = timestamppb.New(now)
	rec.Summary = "resumed by " + ev.Key
	t.ParkOn = ""
	return requeue(t, &v1.Resume{Kind: &v1.Resume_Event{Event: ev}}), nil
}

func applyStop(t *v1.Task, reason string, now time.Time) ([]Effect, error) {
	if IsTerminal(t.Status) {
		return nil, fmt.Errorf("%w: task already %s", ErrInvalidTransition, t.Status)
	}
	if reason == "" {
		reason = "stopped by user"
	}
	effects := stopEffects(t, reason)
	t.Status = v1.TaskStatus_TASK_STATUS_FAILED
	t.Error = reason
	t.Activity = reason
	t.Prompt = nil
	t.LeaseExpiresAt = nil
	t.FinishedAt = timestamppb.New(now)
	return effects, nil
}

func applyPause(t *v1.Task) ([]Effect, error) {
	switch t.Status {
	case v1.TaskStatus_TASK_STATUS_QUEUED, v1.TaskStatus_TASK_STATUS_RUNNING, v1.TaskStatus_TASK_STATUS_WAITING:
	default:
		return nil, fmt.Errorf("%w: cannot pause a %s task", ErrInvalidTransition, t.Status)
	}
	effects := stopEffects(t, "paused by user")
	t.Status = v1.TaskStatus_TASK_STATUS_PAUSED
	t.Prompt = nil
	t.LeaseExpiresAt = nil
	t.Activity = "paused"
	return effects, nil
}

func applyResume(t *v1.Task) ([]Effect, error) {
	if t.Status != v1.TaskStatus_TASK_STATUS_PAUSED {
		return nil, fmt.Errorf("%w: task is %s, not paused", ErrInvalidTransition, t.Status)
	}
	return requeue(t, nil), nil
}

func applyLeaseCheck(t *v1.Task, now time.Time, cfg Config) []Effect {
	if !hasLiveRun(t.Status) || t.LeaseExpiresAt == nil || now.Before(t.LeaseExpiresAt.AsTime()) {
		return nil
	}
	if t.Attempt >= cfg.MaxAttempts {
		finishRun(t, &v1.RunFinished{
			Outcome: v1.Outcome_OUTCOME_FAILED,
			Error:   fmt.Sprintf("run lost: no heartbeat after %d attempts", t.Attempt),
		}, now)
		return nil
	}
	// The lost run may only be partitioned or frozen: tell it to stop so it
	// can't linger once it reconnects.
	effects := stopEffects(t, "superseded: run lease expired")
	attempt := t.Attempt + 1
	retry := requeue(t, nil)[0].(EnqueueRun)
	t.Attempt = attempt
	retry.Request.Task.Attempt = attempt
	return append(effects, retry)
}

// requeue starts a new run for the task from its current stage.
func requeue(t *v1.Task, resume *v1.Resume) []Effect {
	t.Status = v1.TaskStatus_TASK_STATUS_QUEUED
	t.RunSeq++
	t.Attempt = 1
	t.Prompt = nil
	t.LeaseExpiresAt = nil
	t.Activity = "queued"
	return []Effect{enqueue(t, resume)}
}

func enqueue(t *v1.Task, resume *v1.Resume) EnqueueRun {
	return EnqueueRun{Request: &v1.RunRequest{
		TaskId: t.Id,
		RunSeq: t.RunSeq,
		Task:   proto.Clone(t).(*v1.Task),
		Resume: resume,
	}}
}

func stopEffects(t *v1.Task, reason string) []Effect {
	if !hasLiveRun(t.Status) && t.Status != v1.TaskStatus_TASK_STATUS_QUEUED {
		return nil
	}
	// Queued runs get the stop too: the sandbox may start before it's seen.
	return []Effect{SendCommand{Command: &v1.Command{
		TaskId: t.Id,
		RunSeq: t.RunSeq,
		Body:   &v1.Command_Stop{Stop: &v1.StopRun{Reason: reason}},
	}}}
}

func stageRecord(t *v1.Task, index int32, name string) *v1.StageRecord {
	for _, r := range t.Stages {
		if r.Index == index {
			if name != "" {
				r.Name = name
			}
			return r
		}
	}
	r := &v1.StageRecord{Index: index, Name: name}
	t.Stages = append(t.Stages, r)
	return r
}

func upsertArtefact(t *v1.Task, a *v1.ArtefactRef, now time.Time) {
	a = proto.Clone(a).(*v1.ArtefactRef)
	a.UpdatedAt = timestamppb.New(now)
	for i, cur := range t.Artefacts {
		if cur.Kind == a.Kind {
			t.Artefacts[i] = a
			return
		}
	}
	t.Artefacts = append(t.Artefacts, a)
}

func addUsage(dst, d *v1.Usage) {
	dst.InputTokens += d.GetInputTokens()
	dst.OutputTokens += d.GetOutputTokens()
	dst.CacheReadTokens += d.GetCacheReadTokens()
	dst.CacheWriteTokens += d.GetCacheWriteTokens()
	dst.CostUsd += d.GetCostUsd()
	dst.IndicativeCostUsd += d.GetIndicativeCostUsd()
}
