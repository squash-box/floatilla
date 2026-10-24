// floatilla-supervisor is the sandbox entrypoint. It reads its run request
// from FLOATILLA_RUN_REQUEST (base64 protobuf) and drives the agent runtime.
package main

import (
	"context"
	"encoding/base64"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"google.golang.org/protobuf/proto"

	v1 "github.com/squash-box/floatilla/src/gen/go/floatilla/v1"
	"github.com/squash-box/floatilla/src/internal/bus"
	"github.com/squash-box/floatilla/src/internal/env"
	"github.com/squash-box/floatilla/src/internal/supervisor"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	raw, err := base64.StdEncoding.DecodeString(os.Getenv("FLOATILLA_RUN_REQUEST"))
	if err != nil {
		return err
	}
	req := &v1.RunRequest{}
	if err := proto.Unmarshal(raw, req); err != nil {
		return err
	}

	connectCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	b, err := bus.Connect(connectCtx, env.String("NATS_URL", "nats://localhost:4222"), "floatilla-supervisor-"+req.TaskId)
	if err != nil {
		return err
	}
	defer b.Close()

	provider := env.String("FLOATILLA_MODEL_PROVIDER", "")
	if provider == "" {
		provider = "scripted"
		if os.Getenv("ANTHROPIC_API_KEY") != "" {
			provider = "anthropic"
		}
	}

	return supervisor.New(supervisor.Config{
		AgentCmd:  strings.Fields(env.String("FLOATILLA_AGENT_CMD", "node /opt/floatilla/agent/dist/src/main.js")),
		Workspace: env.String("FLOATILLA_WORKSPACE", "/workspace"),
		Model: &v1.ModelConfig{
			Provider:  provider,
			Model:     env.String("FLOATILLA_MODEL", "claude-opus-5-5"),
			Effort:    env.String("FLOATILLA_EFFORT", "high"),
			MaxTokens: int32(env.Int("FLOATILLA_MAX_TOKENS", 64000)),
		},
		Heartbeat: env.Duration("FLOATILLA_HEARTBEAT", 10*time.Second),
	}, b, req, log).Run(ctx)
}
