// Package control is the control plane: it accepts tasks, drives the task
// state machine from run events, relays the outbox to NATS and reaps runs
// whose lease lapsed.
package control

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"

	v1 "github.com/squash-box/floatilla/src/gen/go/floatilla/v1"
	"github.com/squash-box/floatilla/src/internal/bus"
	"github.com/squash-box/floatilla/src/internal/fsm"
	"github.com/squash-box/floatilla/src/internal/packs"
	"github.com/squash-box/floatilla/src/internal/store"
)

// ErrInvalid marks a request the caller must fix.
var ErrInvalid = errors.New("invalid request")

type Control struct {
	store *store.Store
	bus   *bus.Bus
	packs *packs.Registry
	cfg   fsm.Config
	log   *slog.Logger
	now   func() time.Time
}

func New(s *store.Store, b *bus.Bus, reg *packs.Registry, cfg fsm.Config, log *slog.Logger) *Control {
	return &Control{store: s, bus: b, packs: reg, cfg: cfg, log: log, now: time.Now}
}

var slugRE = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// Submit validates and creates a task, queueing its first run.
func (c *Control) Submit(ctx context.Context, req *v1.SubmitTaskRequest) (*v1.Task, error) {
	spec, ok := c.packs.Get(req.ProcessType)
	if !ok {
		return nil, fmt.Errorf("%w: unknown process type %q", ErrInvalid, req.ProcessType)
	}
	workflow := req.Workflow
	if workflow == "" {
		workflow = spec.DefaultWorkflow
	}
	if _, ok := spec.Workflows[workflow]; !ok {
		return nil, fmt.Errorf("%w: process type %s has no workflow %q", ErrInvalid, spec.Name, workflow)
	}
	if !slugRE.MatchString(req.Name) {
		return nil, fmt.Errorf("%w: name must be a kebab-case slug, got %q", ErrInvalid, req.Name)
	}
	if strings.TrimSpace(req.Title) == "" {
		return nil, fmt.Errorf("%w: title is required", ErrInvalid)
	}

	task, effects := fsm.New(&v1.Task{
		Id:           newID(),
		Name:         req.Name,
		ProcessType:  spec.Name,
		Workflow:     workflow,
		Title:        req.Title,
		Instructions: req.Instructions,
		Inputs:       req.Inputs,
		ParentId:     req.ParentId,
		Branch:       "agent/" + req.Name,
	}, c.now())
	out, err := c.outbox(task, effects)
	if err != nil {
		return nil, err
	}
	if err := c.store.Create(ctx, task, out); err != nil {
		return nil, err
	}
	c.log.Info("task submitted", "task", task.Id, "name", task.Name, "type", task.ProcessType, "workflow", workflow)
	return task, nil
}

// Apply runs one state machine input against a stored task.
func (c *Control) Apply(ctx context.Context, id string, in fsm.Input) (*v1.Task, error) {
	return c.store.Update(ctx, id, func(cur *v1.Task) (*v1.Task, []store.OutboxMsg, error) {
		next, effects, err := fsm.Apply(cur, in, c.now(), c.cfg)
		if err != nil {
			return nil, nil, err
		}
		if len(effects) == 0 && proto.Equal(next, cur) {
			return cur, nil, nil
		}
		if next.Status != cur.Status {
			c.log.Info("task status", "task", id, "from", cur.Status, "to", next.Status, "run", next.RunSeq)
		}
		out, err := c.outbox(next, effects)
		return next, out, err
	})
}

// outbox turns effects into bus messages, plus a fleet snapshot of the task.
func (c *Control) outbox(t *v1.Task, effects []fsm.Effect) ([]store.OutboxMsg, error) {
	var out []store.OutboxMsg
	for _, e := range effects {
		switch e := e.(type) {
		case fsm.EnqueueRun:
			req := proto.Clone(e.Request).(*v1.RunRequest)
			spec, ok := c.packs.Get(t.ProcessType)
			if !ok {
				return nil, fmt.Errorf("process type %q is no longer registered", t.ProcessType)
			}
			req.ProcessType = spec
			data, err := proto.Marshal(req)
			if err != nil {
				return nil, err
			}
			out = append(out, store.OutboxMsg{
				Subject: bus.TaskQueueSubject(t.ProcessType),
				Payload: data,
				MsgID:   fmt.Sprintf("run-%s-%d-%d", t.Id, req.RunSeq, req.Task.Attempt),
			})
		case fsm.SendCommand:
			data, err := proto.Marshal(e.Command)
			if err != nil {
				return nil, err
			}
			out = append(out, store.OutboxMsg{Subject: bus.CommandSubject(t.Id), Payload: data})
		default:
			return nil, fmt.Errorf("unhandled effect %T", e)
		}
	}
	snap, err := proto.Marshal(t)
	if err != nil {
		return nil, err
	}
	return append(out, store.OutboxMsg{Subject: bus.FleetSubject(t.Id), Payload: snap, Core: true}), nil
}

// Run starts the background loops and blocks until ctx ends.
func (c *Control) Run(ctx context.Context) error {
	errc := make(chan error, 3)
	go func() { errc <- c.consumeEvents(ctx) }()
	go func() { errc <- c.relayOutbox(ctx) }()
	go func() { errc <- c.reapLeases(ctx) }()
	select {
	case <-ctx.Done():
		return nil
	case err := <-errc:
		return err
	}
}

func (c *Control) consumeEvents(ctx context.Context) error {
	cons, err := c.bus.JS.CreateOrUpdateConsumer(ctx, bus.StreamEvents, jetstream.ConsumerConfig{
		Durable:       "control",
		FilterSubject: bus.AllEventCtl,
		AckPolicy:     jetstream.AckExplicitPolicy,
		DeliverPolicy: jetstream.DeliverNewPolicy,
		MaxAckPending: 256,
	})
	if err != nil {
		return fmt.Errorf("events consumer: %w", err)
	}
	cc, err := cons.Consume(func(msg jetstream.Msg) {
		ev := &v1.AgentEvent{}
		if err := proto.Unmarshal(msg.Data(), ev); err != nil {
			c.log.Error("bad event", "subject", msg.Subject(), "err", err)
			_ = msg.Term()
			return
		}
		_, err := c.Apply(ctx, ev.TaskId, fsm.RunEvent{Event: ev})
		switch {
		case err == nil, errors.Is(err, store.ErrNotFound):
			_ = msg.Ack()
		default:
			c.log.Error("apply event", "task", ev.TaskId, "err", err)
			_ = msg.NakWithDelay(time.Second)
		}
	})
	if err != nil {
		return err
	}
	<-ctx.Done()
	cc.Stop()
	return nil
}

func (c *Control) relayOutbox(ctx context.Context) error {
	publish := func(ctx context.Context, m store.OutboxMsg) error {
		if m.Core {
			return c.bus.NC.Publish(m.Subject, m.Payload)
		}
		var opts []jetstream.PublishOpt
		if m.MsgID != "" {
			opts = append(opts, jetstream.WithMsgID(m.MsgID))
		}
		_, err := c.bus.JS.Publish(ctx, m.Subject, m.Payload, opts...)
		return err
	}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		for {
			n, err := c.store.RelayOutbox(ctx, 100, publish)
			if err != nil {
				c.log.Warn("outbox relay", "err", err)
				break
			}
			if n == 0 {
				break
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		case <-c.store.Kicked():
		}
	}
}

func (c *Control) reapLeases(ctx context.Context) error {
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
		ids, err := c.store.ExpiredLeases(ctx, c.now())
		if err != nil {
			c.log.Warn("lease scan", "err", err)
			continue
		}
		for _, id := range ids {
			if _, err := c.Apply(ctx, id, fsm.LeaseCheck{}); err != nil {
				c.log.Warn("lease check", "task", id, "err", err)
			}
		}
	}
}

// newID returns a time-ordered id: 12 hex digits of unix ms + 8 random.
func newID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%012x%s", time.Now().UnixMilli(), hex.EncodeToString(b[:]))
}
