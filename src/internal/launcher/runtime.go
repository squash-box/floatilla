package launcher

import (
	"context"
	"errors"
)

// ErrConflict means a sandbox with the same name already exists, i.e. this
// run request was delivered before and the sandbox is already started.
var ErrConflict = errors.New("sandbox already exists")

type Mount struct {
	// Volume is a named volume, created on first use.
	Volume string
	Target string
}

type RunSpec struct {
	Name    string
	Image   string
	Env     map[string]string
	Labels  map[string]string
	Mounts  []Mount
	Network string
}

// Sandbox is a started sandbox found by List.
type Sandbox struct {
	ID     string
	Labels map[string]string
}

// Runtime starts sandboxes. Implementations: Docker (local); Kubernetes Jobs,
// ECS and Cloud Run Jobs are future adapters behind the same interface.
type Runtime interface {
	Start(ctx context.Context, spec RunSpec) (id string, err error)
	// Wait blocks until the sandbox exits and returns its exit code.
	Wait(ctx context.Context, id string) (int, error)
	// Logs returns up to the last n lines of output.
	Logs(ctx context.Context, id string, n int) (string, error)
	Remove(ctx context.Context, id string) error
	// List returns running sandboxes carrying all the given labels.
	List(ctx context.Context, labels map[string]string) ([]Sandbox, error)
}
