// Package launcher consumes run requests for one process type and starts a
// fresh sandbox per run. It acks a request once the sandbox is started: from
// then on the run's liveness is the control plane's lease, not the queue.
package launcher

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/squash-box/floatilla/src/gen/go/floatilla/v1"
	"github.com/squash-box/floatilla/src/internal/bus"
)

const (
	labelManaged = "floatilla.managed"
	labelType    = "floatilla.type"
	labelTask    = "floatilla.task"
	labelRun     = "floatilla.run"
)

type Config struct {
	ProcessType string
	// Concurrency is the max number of sandboxes running at once.
	Concurrency int
	// Network the sandboxes join (must reach NATS).
	Network string
	NATSURL string
	// ImageOverride replaces the process type's image when set.
	ImageOverride string
	// Env is passed through to every sandbox (model config, API keys).
	Env map[string]string
	// KeepSandboxes leaves exited sandboxes for debugging.
	KeepSandboxes bool
	MaxDeliver    int
}

type Launcher struct {
	cfg Config
	bus *bus.Bus
	rt  Runtime
	log *slog.Logger
	sem chan struct{}
}

func New(cfg Config, b *bus.Bus, rt Runtime, log *slog.Logger) *Launcher {
	if cfg.Concurrency < 1 {
		cfg.Concurrency = 1
	}
	if cfg.MaxDeliver < 1 {
		cfg.MaxDeliver = 5
	}
	return &Launcher{cfg: cfg, bus: b, rt: rt, log: log.With("type", cfg.ProcessType), sem: make(chan struct{}, cfg.Concurrency)}
}

func (l *Launcher) Run(ctx context.Context) error {
	if _, err := l.bus.WaitForStream(ctx, bus.StreamTasks); err != nil {
		return err
	}
	l.adopt(ctx)

	cons, err := l.bus.JS.CreateOrUpdateConsumer(ctx, bus.StreamTasks, jetstream.ConsumerConfig{
		Durable:       "launcher-" + l.cfg.ProcessType,
		FilterSubject: bus.TaskQueueSubject(l.cfg.ProcessType),
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       2 * time.Minute,
		MaxDeliver:    l.cfg.MaxDeliver,
		MaxAckPending: l.cfg.Concurrency,
	})
	if err != nil {
		return fmt.Errorf("consumer: %w", err)
	}
	it, err := cons.Messages(jetstream.PullMaxMessages(1))
	if err != nil {
		return err
	}
	go func() { <-ctx.Done(); it.Stop() }()
	l.log.Info("launcher ready", "concurrency", l.cfg.Concurrency)

	for {
		select {
		case l.sem <- struct{}{}:
		case <-ctx.Done():
			return nil
		}
		msg, err := it.Next()
		if err != nil {
			<-l.sem
			if ctx.Err() != nil || errors.Is(err, jetstream.ErrMsgIteratorClosed) {
				return nil
			}
			l.log.Warn("next message", "err", err)
			continue
		}
		go l.handle(ctx, msg)
	}
}

// adopt resumes supervising sandboxes started before a launcher restart.
func (l *Launcher) adopt(ctx context.Context) {
	boxes, err := l.rt.List(ctx, map[string]string{labelManaged: "true", labelType: l.cfg.ProcessType})
	if err != nil {
		l.log.Warn("list sandboxes", "err", err)
		return
	}
	for _, b := range boxes {
		var seq uint32
		fmt.Sscan(b.Labels[labelRun], &seq)
		task := b.Labels[labelTask]
		l.log.Info("adopting sandbox", "task", task, "run", seq)
		holdsSlot := true
		select {
		case l.sem <- struct{}{}:
		default:
			// Over capacity after a config change; still supervise it.
			holdsSlot = false
		}
		go l.supervise(ctx, b.ID, task, seq, holdsSlot)
	}
}

func (l *Launcher) handle(ctx context.Context, msg jetstream.Msg) {
	req := &v1.RunRequest{}
	if err := proto.Unmarshal(msg.Data(), req); err != nil {
		l.log.Error("bad run request", "err", err)
		_ = msg.Term()
		<-l.sem
		return
	}
	log := l.log.With("task", req.TaskId, "run", req.RunSeq)

	id, err := l.start(ctx, req)
	switch {
	case errors.Is(err, ErrConflict):
		log.Info("sandbox already started (redelivery)")
		_ = msg.Ack()
		<-l.sem
		return
	case err != nil:
		meta, _ := msg.Metadata()
		if meta != nil && int(meta.NumDelivered) >= l.cfg.MaxDeliver {
			log.Error("giving up starting sandbox", "err", err)
			l.reportFailure(ctx, req.TaskId, req.RunSeq, "could not start sandbox: "+err.Error())
			_ = msg.Term()
		} else {
			log.Warn("start sandbox", "err", err)
			_ = msg.NakWithDelay(5 * time.Second)
		}
		<-l.sem
		return
	}
	_ = msg.Ack()
	log.Info("sandbox started", "sandbox", shortID(id))
	l.supervise(ctx, id, req.TaskId, req.RunSeq, true)
}

func (l *Launcher) start(ctx context.Context, req *v1.RunRequest) (string, error) {
	image := req.GetProcessType().GetImage()
	if l.cfg.ImageOverride != "" {
		image = l.cfg.ImageOverride
	}
	data, err := proto.Marshal(req)
	if err != nil {
		return "", err
	}
	env := map[string]string{
		"FLOATILLA_RUN_REQUEST": base64.StdEncoding.EncodeToString(data),
		"NATS_URL":              l.cfg.NATSURL,
	}
	for k, v := range l.cfg.Env {
		env[k] = v
	}
	return l.rt.Start(ctx, RunSpec{
		Name:  fmt.Sprintf("floatilla-%s-%d-%d", req.TaskId, req.RunSeq, req.GetTask().GetAttempt()),
		Image: image,
		Env:   env,
		Labels: map[string]string{
			labelManaged: "true",
			labelType:    l.cfg.ProcessType,
			labelTask:    req.TaskId,
			labelRun:     fmt.Sprint(req.RunSeq),
		},
		// The workspace outlives runs so a resumed task finds its files.
		Mounts:  []Mount{{Volume: "floatilla-ws-" + req.TaskId, Target: "/workspace"}},
		Network: l.cfg.Network,
	})
}

// supervise waits for a sandbox to exit, reports crashes, and cleans up.
// holdsSlot says whether the caller took a concurrency slot to release.
func (l *Launcher) supervise(ctx context.Context, id, taskID string, runSeq uint32, holdsSlot bool) {
	if holdsSlot {
		defer func() { <-l.sem }()
	}
	log := l.log.With("task", taskID, "run", runSeq, "sandbox", shortID(id))
	code, err := l.rt.Wait(ctx, id)
	if ctx.Err() != nil {
		return // shutting down; the sandbox keeps running and is adopted later
	}
	if err != nil {
		log.Warn("wait sandbox", "err", err)
		return
	}
	if code != 0 {
		logs, _ := l.rt.Logs(context.WithoutCancel(ctx), id, 20)
		log.Warn("sandbox failed", "exit", code, "logs", logs)
		l.reportFailure(ctx, taskID, runSeq, fmt.Sprintf("sandbox exited with code %d: %s", code, lastLine(logs)))
	} else {
		log.Info("sandbox exited")
	}
	if !l.cfg.KeepSandboxes {
		if err := l.rt.Remove(context.WithoutCancel(ctx), id); err != nil {
			log.Warn("remove sandbox", "err", err)
		}
	}
}

// reportFailure publishes a run_finished on the run's behalf. If the
// supervisor already reported an outcome, the state machine ignores it.
func (l *Launcher) reportFailure(ctx context.Context, taskID string, runSeq uint32, msg string) {
	ev := &v1.AgentEvent{
		TaskId: taskID,
		RunSeq: runSeq,
		At:     timestamppb.Now(),
		Body: &v1.AgentEvent_RunFinished{RunFinished: &v1.RunFinished{
			Outcome: v1.Outcome_OUTCOME_FAILED,
			Error:   msg,
		}},
	}
	if err := l.bus.Publish(context.WithoutCancel(ctx), bus.EventCtlSubject(taskID), ev, ""); err != nil {
		l.log.Error("report failure", "task", taskID, "err", err)
	}
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
