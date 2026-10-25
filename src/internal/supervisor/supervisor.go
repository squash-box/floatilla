// Package supervisor runs inside a sandbox as the run's entrypoint. It starts
// the agent runtime as a child process, speaks the stdio protocol with it,
// enforces the permission policy, and bridges events and commands to NATS.
package supervisor

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/squash-box/floatilla/src/gen/go/floatilla/v1"
	"github.com/squash-box/floatilla/src/internal/bus"
)

type Config struct {
	// AgentCmd starts the agent runtime, e.g. ["node", "/opt/floatilla/agent/dist/main.js"].
	AgentCmd  []string
	Workspace string
	Model     *v1.ModelConfig
	// Heartbeat interval; must be well under the control plane's lease TTL.
	Heartbeat time.Duration
	// StopGrace is how long the agent gets to exit after a stop.
	StopGrace time.Duration
	TailLines int
}

type Supervisor struct {
	cfg Config
	bus *bus.Bus
	req *v1.RunRequest
	log *slog.Logger
	seq atomic.Uint64

	stdinMu sync.Mutex
	stdin   io.WriteCloser

	tail      []string
	lastProg  *v1.Progress
	tailDirty bool
	// pending prompts by id: the tool request id they gate, or "" for asks.
	pending  map[string]string
	promptN  int
	finished *v1.RunFinished
}

func New(cfg Config, b *bus.Bus, req *v1.RunRequest, log *slog.Logger) *Supervisor {
	if cfg.Heartbeat == 0 {
		cfg.Heartbeat = 10 * time.Second
	}
	if cfg.StopGrace == 0 {
		cfg.StopGrace = 15 * time.Second
	}
	if cfg.TailLines == 0 {
		cfg.TailLines = 2
	}
	return &Supervisor{
		cfg: cfg, bus: b, req: req,
		log:     log.With("task", req.TaskId, "run", req.RunSeq),
		pending: map[string]string{},
	}
}

// Run executes the run to completion. It returns an error only when the
// supervisor itself failed (the launcher then reports the run as failed);
// agent failures are reported as a run_finished event and return nil.
func (s *Supervisor) Run(ctx context.Context) error {
	task := s.req.GetTask()
	spec := s.req.GetProcessType()
	wf, ok := spec.GetWorkflows()[task.GetWorkflow()]
	if !ok {
		return s.finish(ctx, &v1.RunFinished{Outcome: v1.Outcome_OUTCOME_FAILED,
			Error: fmt.Sprintf("workflow %q not defined for %s", task.GetWorkflow(), spec.GetName())})
	}

	// Subscribe to commands before announcing the run so none are missed.
	cmds := make(chan *v1.Command, 16)
	cons, err := s.bus.JS.OrderedConsumer(ctx, bus.StreamCmds, jetstream.OrderedConsumerConfig{
		FilterSubjects: []string{bus.CommandSubject(s.req.TaskId)},
		DeliverPolicy:  jetstream.DeliverAllPolicy,
	})
	if err != nil {
		return fmt.Errorf("command consumer: %w", err)
	}
	cc, err := cons.Consume(func(m jetstream.Msg) {
		c := &v1.Command{}
		if proto.Unmarshal(m.Data(), c) == nil && c.RunSeq == s.req.RunSeq {
			cmds <- c
		}
	})
	if err != nil {
		return err
	}
	defer cc.Stop()

	host, _ := os.Hostname()
	if err := s.publish(ctx, &v1.AgentEvent{Body: &v1.AgentEvent_RunStarted{RunStarted: &v1.RunStarted{SandboxId: host}}}); err != nil {
		return err
	}

	child := exec.CommandContext(ctx, s.cfg.AgentCmd[0], s.cfg.AgentCmd[1:]...)
	child.Dir = s.cfg.Workspace
	child.Stderr = os.Stderr
	stdout, err := child.StdoutPipe()
	if err != nil {
		return err
	}
	if s.stdin, err = child.StdinPipe(); err != nil {
		return err
	}
	if err := child.Start(); err != nil {
		return s.finish(ctx, &v1.RunFinished{Outcome: v1.Outcome_OUTCOME_FAILED, Error: "start agent runtime: " + err.Error()})
	}

	msgs := make(chan *v1.AgentMessage, 64)
	readErr := make(chan error, 1)
	go func() { readErr <- readMessages(stdout, msgs) }()
	exited := make(chan error, 1)
	go func() {
		err := <-readErr
		if werr := child.Wait(); err == nil {
			err = werr
		}
		exited <- err
	}()

	if err := s.send(&v1.SupervisorMessage{Body: &v1.SupervisorMessage_Init{Init: &v1.RunInit{
		Task:        task,
		Resume:      s.req.GetResume(),
		ProcessType: spec,
		Workflow:    wf,
		Workspace:   s.cfg.Workspace,
		Model:       s.cfg.Model,
	}}}); err != nil {
		_ = child.Process.Kill()
	}

	heartbeat := time.NewTicker(s.cfg.Heartbeat)
	defer heartbeat.Stop()
	flush := time.NewTicker(time.Second)
	defer flush.Stop()
	var killTimer <-chan time.Time
	stopping := false

	for {
		select {
		case m := <-msgs:
			if err := s.handleAgent(ctx, m); err != nil {
				return err
			}
			if s.finished != nil {
				s.closeStdin()
			}
		case c := <-cmds:
			switch b := c.Body.(type) {
			case *v1.Command_Answer:
				if err := s.handleAnswer(ctx, b.Answer); err != nil {
					return err
				}
			case *v1.Command_Stop:
				if !stopping {
					stopping = true
					s.log.Info("stopping", "reason", b.Stop.Reason)
					_ = s.send(&v1.SupervisorMessage{Body: &v1.SupervisorMessage_Stop{Stop: b.Stop}})
					s.closeStdin()
					killTimer = time.After(s.cfg.StopGrace)
				}
			}
		case <-killTimer:
			_ = child.Process.Kill()
		case <-heartbeat.C:
			if err := s.publish(ctx, &v1.AgentEvent{Body: &v1.AgentEvent_Heartbeat{Heartbeat: &v1.Heartbeat{}}}); err != nil {
				return err
			}
		case <-flush.C:
			if err := s.flushTail(ctx); err != nil {
				return err
			}
		case err := <-exited:
			// Drain anything the agent wrote before exiting.
			for len(msgs) > 0 {
				if herr := s.handleAgent(ctx, <-msgs); herr != nil {
					return herr
				}
			}
			if ferr := s.flushTail(ctx); ferr != nil {
				return ferr
			}
			switch {
			case stopping:
				return s.finish(ctx, &v1.RunFinished{Outcome: v1.Outcome_OUTCOME_STOPPED, Error: "stopped"})
			case s.finished != nil:
				return s.finish(ctx, s.finished)
			default:
				msg := "agent runtime exited without finishing"
				if err != nil {
					msg += ": " + err.Error()
				}
				return s.finish(ctx, &v1.RunFinished{Outcome: v1.Outcome_OUTCOME_FAILED, Error: msg})
			}
		case <-ctx.Done():
			_ = child.Process.Kill()
			return ctx.Err()
		}
	}
}

func readMessages(r io.Reader, out chan<- *v1.AgentMessage) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		m := &v1.AgentMessage{}
		if err := protojson.Unmarshal(line, m); err != nil {
			// Not protocol output (stray console.log); keep it visible.
			fmt.Fprintf(os.Stderr, "agent stdout: %s\n", line)
			continue
		}
		out <- m
	}
	return sc.Err()
}

func (s *Supervisor) handleAgent(ctx context.Context, m *v1.AgentMessage) error {
	switch b := m.Body.(type) {
	case *v1.AgentMessage_StageStarted:
		s.tail = nil
		return s.publish(ctx, &v1.AgentEvent{Body: &v1.AgentEvent_StageStarted{StageStarted: b.StageStarted}})
	case *v1.AgentMessage_StageFinished:
		return s.publish(ctx, &v1.AgentEvent{Body: &v1.AgentEvent_StageFinished{StageFinished: b.StageFinished}})
	case *v1.AgentMessage_Progress:
		s.lastProg = b.Progress
		s.tailDirty = true
		return s.flushTail(ctx)
	case *v1.AgentMessage_Log:
		if b.Log.Kind != "" || len(s.tail) == 0 {
			s.tail = appendTail(s.tail, formatTail(b.Log), s.cfg.TailLines)
			s.tailDirty = true
		}
		return s.publish(ctx, &v1.AgentEvent{Body: &v1.AgentEvent_Log{Log: b.Log}})
	case *v1.AgentMessage_Artefact:
		return s.publish(ctx, &v1.AgentEvent{Body: &v1.AgentEvent_Artefact{Artefact: b.Artefact}})
	case *v1.AgentMessage_Usage:
		u := proto.Clone(b.Usage.GetUsage()).(*v1.Usage)
		u.CostUsd = Cost(b.Usage.Model, u)
		return s.publish(ctx, &v1.AgentEvent{Body: &v1.AgentEvent_Usage{Usage: &v1.UsageDelta{Model: b.Usage.Model, Usage: u}}})
	case *v1.AgentMessage_ToolRequest:
		return s.handleToolRequest(ctx, b.ToolRequest)
	case *v1.AgentMessage_Ask:
		p := proto.Clone(b.Ask.GetPrompt()).(*v1.Prompt)
		if p.Id == "" {
			p.Id = s.nextPromptID()
		}
		s.pending[p.Id] = ""
		return s.publish(ctx, &v1.AgentEvent{Body: &v1.AgentEvent_PromptRaised{PromptRaised: &v1.PromptRaised{Prompt: p}}})
	case *v1.AgentMessage_StoriesPublished:
		return s.publish(ctx, &v1.AgentEvent{Body: &v1.AgentEvent_StoriesPublished{StoriesPublished: b.StoriesPublished}})
	case *v1.AgentMessage_Finished:
		s.finished = b.Finished
	}
	return nil
}

func (s *Supervisor) handleToolRequest(ctx context.Context, r *v1.ToolRequest) error {
	decision := func(allowed bool, reason string) error {
		return s.send(&v1.SupervisorMessage{Body: &v1.SupervisorMessage_ToolDecision{ToolDecision: &v1.ToolDecision{
			RequestId: r.RequestId, Allowed: allowed, Reason: reason,
		}}})
	}
	switch Decide(s.req.GetProcessType().GetPermissions(), r.Tool) {
	case Allow:
		return decision(true, "")
	case Deny:
		return decision(false, fmt.Sprintf("tool %s is not permitted for %s agents", r.Tool, s.req.GetProcessType().GetName()))
	}
	id := s.nextPromptID()
	s.pending[id] = r.RequestId
	detail := r.InputJson
	if len(detail) > 2000 {
		detail = detail[:2000] + "…"
	}
	return s.publish(ctx, &v1.AgentEvent{Body: &v1.AgentEvent_PromptRaised{PromptRaised: &v1.PromptRaised{Prompt: &v1.Prompt{
		Id:     id,
		Kind:   v1.PromptKind_PROMPT_KIND_PERMISSION,
		Text:   "approve " + r.Summary + "?",
		Detail: detail,
	}}}})
}

func (s *Supervisor) handleAnswer(ctx context.Context, a *v1.PromptAnswer) error {
	toolReq, ok := s.pending[a.PromptId]
	if !ok {
		return nil // already answered, or for another prompt
	}
	delete(s.pending, a.PromptId)
	var err error
	if toolReq != "" {
		reason := a.Text
		if !a.Approved && reason == "" {
			reason = "denied by a human"
		}
		err = s.send(&v1.SupervisorMessage{Body: &v1.SupervisorMessage_ToolDecision{ToolDecision: &v1.ToolDecision{
			RequestId: toolReq, Allowed: a.Approved, Reason: reason,
		}}})
	} else {
		err = s.send(&v1.SupervisorMessage{Body: &v1.SupervisorMessage_Answer{Answer: a}})
	}
	if err != nil {
		s.log.Warn("deliver answer", "err", err)
	}
	return s.publish(ctx, &v1.AgentEvent{Body: &v1.AgentEvent_PromptResolved{PromptResolved: &v1.PromptResolved{PromptId: a.PromptId}}})
}

func (s *Supervisor) flushTail(ctx context.Context) error {
	if !s.tailDirty {
		return nil
	}
	s.tailDirty = false
	p := &v1.Progress{}
	if s.lastProg != nil {
		p = proto.Clone(s.lastProg).(*v1.Progress)
	}
	p.Tail = append([]string(nil), s.tail...)
	return s.publish(ctx, &v1.AgentEvent{Body: &v1.AgentEvent_Progress{Progress: p}})
}

func (s *Supervisor) finish(ctx context.Context, f *v1.RunFinished) error {
	s.log.Info("run finished", "outcome", f.Outcome, "error", f.Error)
	return s.publish(ctx, &v1.AgentEvent{Body: &v1.AgentEvent_RunFinished{RunFinished: f}})
}

// publish stamps and sends an event, retrying briefly. Log lines go to the
// log subject; everything else to the control subject.
func (s *Supervisor) publish(ctx context.Context, ev *v1.AgentEvent) error {
	ev.TaskId = s.req.TaskId
	ev.RunSeq = s.req.RunSeq
	ev.Seq = s.seq.Add(1)
	ev.At = timestamppb.Now()
	subject := bus.EventCtlSubject(s.req.TaskId)
	if ev.GetLog() != nil {
		subject = bus.EventLogSubject(s.req.TaskId)
	}
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		if err = s.bus.Publish(ctx, subject, ev, ""); err == nil {
			return nil
		}
		if errors.Is(err, context.Canceled) {
			return err
		}
		time.Sleep(time.Duration(attempt+1) * 500 * time.Millisecond)
	}
	return fmt.Errorf("publish %s: %w", subject, err)
}

func (s *Supervisor) send(m *v1.SupervisorMessage) error {
	line, err := protojson.Marshal(m)
	if err != nil {
		return err
	}
	s.stdinMu.Lock()
	defer s.stdinMu.Unlock()
	if s.stdin == nil {
		return errors.New("agent stdin closed")
	}
	_, err = s.stdin.Write(append(line, '\n'))
	return err
}

func (s *Supervisor) closeStdin() {
	s.stdinMu.Lock()
	defer s.stdinMu.Unlock()
	if s.stdin != nil {
		_ = s.stdin.Close()
		s.stdin = nil
	}
}

func (s *Supervisor) nextPromptID() string {
	s.promptN++
	return fmt.Sprintf("r%d-p%d", s.req.RunSeq, s.promptN)
}

func formatTail(l *v1.LogLine) string {
	if l.Kind == "" {
		return l.Text
	}
	return l.Kind + " " + l.Text
}

func appendTail(tail []string, line string, n int) []string {
	tail = append(tail, line)
	if len(tail) > n {
		tail = tail[len(tail)-n:]
	}
	return tail
}
