package control

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"

	v1 "github.com/squash-box/floatilla/src/gen/go/floatilla/v1"
	"github.com/squash-box/floatilla/src/gen/go/floatilla/v1/floatillav1connect"
	"github.com/squash-box/floatilla/src/internal/bus"
	"github.com/squash-box/floatilla/src/internal/fsm"
	"github.com/squash-box/floatilla/src/internal/store"
)

// API implements FleetService on top of Control.
type API struct {
	c *Control
}

var _ floatillav1connect.FleetServiceHandler = (*API)(nil)

func NewAPI(c *Control) *API { return &API{c: c} }

func toConnect(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, store.ErrNotFound):
		return connect.NewError(connect.CodeNotFound, err)
	case errors.Is(err, ErrInvalid):
		return connect.NewError(connect.CodeInvalidArgument, err)
	case errors.Is(err, fsm.ErrInvalidTransition):
		return connect.NewError(connect.CodeFailedPrecondition, err)
	default:
		return err
	}
}

func (a *API) SubmitTask(ctx context.Context, req *connect.Request[v1.SubmitTaskRequest]) (*connect.Response[v1.SubmitTaskResponse], error) {
	t, err := a.c.Submit(ctx, req.Msg)
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&v1.SubmitTaskResponse{Task: t}), nil
}

func (a *API) GetTask(ctx context.Context, req *connect.Request[v1.GetTaskRequest]) (*connect.Response[v1.GetTaskResponse], error) {
	t, err := a.c.store.Get(ctx, req.Msg.Id)
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&v1.GetTaskResponse{Task: t}), nil
}

func (a *API) ListTasks(ctx context.Context, req *connect.Request[v1.ListTasksRequest]) (*connect.Response[v1.ListTasksResponse], error) {
	ts, err := a.c.store.List(ctx, req.Msg.IncludeFinished)
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&v1.ListTasksResponse{Tasks: ts}), nil
}

func (a *API) AnswerPrompt(ctx context.Context, req *connect.Request[v1.AnswerPromptRequest]) (*connect.Response[v1.AnswerPromptResponse], error) {
	t, err := a.c.Apply(ctx, req.Msg.TaskId, fsm.AnswerPrompt{Answer: req.Msg.GetAnswer()})
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&v1.AnswerPromptResponse{Task: t}), nil
}

func (a *API) StopTask(ctx context.Context, req *connect.Request[v1.StopTaskRequest]) (*connect.Response[v1.StopTaskResponse], error) {
	t, err := a.c.Apply(ctx, req.Msg.TaskId, fsm.Stop{Reason: req.Msg.Reason})
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&v1.StopTaskResponse{Task: t}), nil
}

func (a *API) PauseTask(ctx context.Context, req *connect.Request[v1.PauseTaskRequest]) (*connect.Response[v1.PauseTaskResponse], error) {
	t, err := a.c.Apply(ctx, req.Msg.TaskId, fsm.Pause{})
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&v1.PauseTaskResponse{Task: t}), nil
}

func (a *API) ResumeTask(ctx context.Context, req *connect.Request[v1.ResumeTaskRequest]) (*connect.Response[v1.ResumeTaskResponse], error) {
	t, err := a.c.Apply(ctx, req.Msg.TaskId, fsm.Resume{})
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&v1.ResumeTaskResponse{Task: t}), nil
}

func (a *API) PostEvent(ctx context.Context, req *connect.Request[v1.PostEventRequest]) (*connect.Response[v1.PostEventResponse], error) {
	t, err := a.c.Apply(ctx, req.Msg.TaskId, fsm.ExternalEvent{Event: req.Msg.GetEvent()})
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&v1.PostEventResponse{Task: t}), nil
}

func (a *API) ListProcessTypes(context.Context, *connect.Request[v1.ListProcessTypesRequest]) (*connect.Response[v1.ListProcessTypesResponse], error) {
	return connect.NewResponse(&v1.ListProcessTypesResponse{ProcessTypes: a.c.packs.All()}), nil
}

func (a *API) Watch(ctx context.Context, req *connect.Request[v1.WatchRequest], stream *connect.ServerStream[v1.WatchResponse]) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	out := make(chan *v1.WatchResponse, 256)

	// Subscribe before taking the snapshot so no change falls in between.
	subject := bus.AllFleet
	if req.Msg.TaskId != "" {
		subject = bus.FleetSubject(req.Msg.TaskId)
	}
	sub, err := a.c.bus.NC.Subscribe(subject, func(m *nats.Msg) {
		t := &v1.Task{}
		if proto.Unmarshal(m.Data, t) != nil {
			return
		}
		select {
		case out <- &v1.WatchResponse{Body: &v1.WatchResponse_Task{Task: t}}:
		case <-ctx.Done():
		}
	})
	if err != nil {
		return err
	}
	defer sub.Unsubscribe()

	if req.Msg.TaskId != "" {
		t, err := a.c.store.Get(ctx, req.Msg.TaskId)
		if err != nil {
			return toConnect(err)
		}
		if err := stream.Send(&v1.WatchResponse{Body: &v1.WatchResponse_Task{Task: t}}); err != nil {
			return err
		}
		cons, err := a.c.bus.JS.OrderedConsumer(ctx, bus.StreamEvents, jetstream.OrderedConsumerConfig{
			FilterSubjects: []string{bus.EventLogSubject(req.Msg.TaskId)},
			DeliverPolicy:  jetstream.DeliverAllPolicy,
		})
		if err != nil {
			return err
		}
		cc, err := cons.Consume(func(m jetstream.Msg) {
			ev := &v1.AgentEvent{}
			if proto.Unmarshal(m.Data(), ev) != nil {
				return
			}
			select {
			case out <- &v1.WatchResponse{Body: &v1.WatchResponse_Log{Log: ev}}:
			case <-ctx.Done():
			}
		})
		if err != nil {
			return err
		}
		defer cc.Stop()
	} else {
		ts, err := a.c.store.List(ctx, false)
		if err != nil {
			return err
		}
		for _, t := range ts {
			if err := stream.Send(&v1.WatchResponse{Body: &v1.WatchResponse_Task{Task: t}}); err != nil {
				return err
			}
		}
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case m := <-out:
			if err := stream.Send(m); err != nil {
				return err
			}
		}
	}
}
