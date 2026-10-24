// Package bus owns the NATS JetStream layout: stream definitions, subject
// names and protobuf publish helpers.
//
//	tasks.<process_type>    RunRequest   work queue, one consumer per type
//	events.<task_id>.ctl    AgentEvent   control-relevant run events
//	events.<task_id>.log    AgentEvent   log lines (UI only)
//	cmd.<task_id>           Command      answers / stops for the live run
//	fleet.task.<task_id>    Task         snapshots after each change (core NATS)
package bus

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
)

const (
	StreamTasks  = "TASKS"
	StreamEvents = "EVENTS"
	StreamCmds   = "CMDS"
)

func TaskQueueSubject(processType string) string { return "tasks." + processType }
func EventCtlSubject(taskID string) string       { return "events." + taskID + ".ctl" }
func EventLogSubject(taskID string) string       { return "events." + taskID + ".log" }
func CommandSubject(taskID string) string        { return "cmd." + taskID }
func FleetSubject(taskID string) string          { return "fleet.task." + taskID }

const (
	AllEventCtl = "events.*.ctl"
	AllFleet    = "fleet.task.*"
)

type Bus struct {
	NC *nats.Conn
	JS jetstream.JetStream
}

// Connect dials NATS, retrying until ctx ends (compose starts services in
// parallel).
func Connect(ctx context.Context, url, name string) (*Bus, error) {
	var nc *nats.Conn
	var err error
	for {
		nc, err = nats.Connect(url, nats.Name(name), nats.MaxReconnects(-1))
		if err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("connect %s: %w", url, err)
		case <-time.After(time.Second):
		}
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, err
	}
	return &Bus{NC: nc, JS: js}, nil
}

func (b *Bus) Close() { b.NC.Drain() }

// EnsureStreams creates or updates the streams. Called by the control plane.
func (b *Bus) EnsureStreams(ctx context.Context) error {
	streams := []jetstream.StreamConfig{
		{
			Name:       StreamTasks,
			Subjects:   []string{"tasks.>"},
			Retention:  jetstream.WorkQueuePolicy,
			Duplicates: 10 * time.Minute,
		},
		{
			Name:      StreamEvents,
			Subjects:  []string{"events.>"},
			Retention: jetstream.LimitsPolicy,
			MaxAge:    7 * 24 * time.Hour,
		},
		{
			Name:      StreamCmds,
			Subjects:  []string{"cmd.>"},
			Retention: jetstream.LimitsPolicy,
			MaxAge:    24 * time.Hour,
		},
	}
	for _, cfg := range streams {
		if _, err := b.JS.CreateOrUpdateStream(ctx, cfg); err != nil {
			return fmt.Errorf("stream %s: %w", cfg.Name, err)
		}
	}
	return nil
}

// WaitForStream blocks until a stream exists (workers start before the
// control plane has created them).
func (b *Bus) WaitForStream(ctx context.Context, name string) (jetstream.Stream, error) {
	for {
		s, err := b.JS.Stream(ctx, name)
		if err == nil {
			return s, nil
		}
		if !errors.Is(err, jetstream.ErrStreamNotFound) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// Publish sends a protobuf message to a JetStream subject. A non-empty msgID
// enables server-side de-duplication.
func (b *Bus) Publish(ctx context.Context, subject string, m proto.Message, msgID string) error {
	data, err := proto.Marshal(m)
	if err != nil {
		return err
	}
	var opts []jetstream.PublishOpt
	if msgID != "" {
		opts = append(opts, jetstream.WithMsgID(msgID))
	}
	_, err = b.JS.Publish(ctx, subject, data, opts...)
	return err
}
