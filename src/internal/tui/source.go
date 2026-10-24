package tui

import (
	"context"
	"net/http"

	"connectrpc.com/connect"

	v1 "github.com/squash-box/floatilla/src/gen/go/floatilla/v1"
	"github.com/squash-box/floatilla/src/gen/go/floatilla/v1/floatillav1connect"
)

// Source is where the TUI gets the fleet from and sends actions to: the
// control plane, or the demo feed.
type Source interface {
	// Snapshot returns the process types and all tasks, finished included.
	Snapshot(ctx context.Context) ([]*v1.ProcessTypeSpec, []*v1.Task, error)
	// WatchFleet calls onTask for every task change until ctx ends or the
	// stream breaks.
	WatchFleet(ctx context.Context, onTask func(*v1.Task)) error
	// WatchTask streams one task and its log (replayed from the start).
	WatchTask(ctx context.Context, id string, onTask func(*v1.Task), onLog func(*v1.AgentEvent)) error

	Answer(ctx context.Context, id string, a *v1.PromptAnswer) error
	Stop(ctx context.Context, id, reason string) error
	Pause(ctx context.Context, id string) error
	Resume(ctx context.Context, id string) error
	Submit(ctx context.Context, req *v1.SubmitTaskRequest) (*v1.Task, error)
}

// ConnectSource talks to floatilla-control's FleetService.
type ConnectSource struct {
	c floatillav1connect.FleetServiceClient
}

func NewConnectSource(url string) *ConnectSource {
	return &ConnectSource{c: floatillav1connect.NewFleetServiceClient(http.DefaultClient, url)}
}

func (s *ConnectSource) Snapshot(ctx context.Context) ([]*v1.ProcessTypeSpec, []*v1.Task, error) {
	types, err := s.c.ListProcessTypes(ctx, connect.NewRequest(&v1.ListProcessTypesRequest{}))
	if err != nil {
		return nil, nil, err
	}
	tasks, err := s.c.ListTasks(ctx, connect.NewRequest(&v1.ListTasksRequest{IncludeFinished: true}))
	if err != nil {
		return nil, nil, err
	}
	return types.Msg.ProcessTypes, tasks.Msg.Tasks, nil
}

func (s *ConnectSource) WatchFleet(ctx context.Context, onTask func(*v1.Task)) error {
	return s.watch(ctx, "", onTask, nil)
}

func (s *ConnectSource) WatchTask(ctx context.Context, id string, onTask func(*v1.Task), onLog func(*v1.AgentEvent)) error {
	return s.watch(ctx, id, onTask, onLog)
}

func (s *ConnectSource) watch(ctx context.Context, id string, onTask func(*v1.Task), onLog func(*v1.AgentEvent)) error {
	stream, err := s.c.Watch(ctx, connect.NewRequest(&v1.WatchRequest{TaskId: id}))
	if err != nil {
		return err
	}
	defer stream.Close()
	for stream.Receive() {
		switch b := stream.Msg().Body.(type) {
		case *v1.WatchResponse_Task:
			onTask(b.Task)
		case *v1.WatchResponse_Log:
			if onLog != nil {
				onLog(b.Log)
			}
		}
	}
	if err := stream.Err(); err != nil {
		return err
	}
	return ctx.Err()
}

func (s *ConnectSource) Answer(ctx context.Context, id string, a *v1.PromptAnswer) error {
	_, err := s.c.AnswerPrompt(ctx, connect.NewRequest(&v1.AnswerPromptRequest{TaskId: id, Answer: a}))
	return err
}

func (s *ConnectSource) Stop(ctx context.Context, id, reason string) error {
	_, err := s.c.StopTask(ctx, connect.NewRequest(&v1.StopTaskRequest{TaskId: id, Reason: reason}))
	return err
}

func (s *ConnectSource) Pause(ctx context.Context, id string) error {
	_, err := s.c.PauseTask(ctx, connect.NewRequest(&v1.PauseTaskRequest{TaskId: id}))
	return err
}

func (s *ConnectSource) Resume(ctx context.Context, id string) error {
	_, err := s.c.ResumeTask(ctx, connect.NewRequest(&v1.ResumeTaskRequest{TaskId: id}))
	return err
}

func (s *ConnectSource) Submit(ctx context.Context, req *v1.SubmitTaskRequest) (*v1.Task, error) {
	res, err := s.c.SubmitTask(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return res.Msg.Task, nil
}
