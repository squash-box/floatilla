// floatilla-launcher consumes run requests for one process type and starts a
// sandbox container per run.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/squash-box/floatilla/src/internal/bus"
	"github.com/squash-box/floatilla/src/internal/env"
	"github.com/squash-box/floatilla/src/internal/launcher"
)

// passthrough env vars are handed to every sandbox when set.
var passthrough = []string{
	"ANTHROPIC_API_KEY",
	"ANTHROPIC_BASE_URL",
	"FLOATILLA_MODEL_PROVIDER",
	"FLOATILLA_MODEL",
	"FLOATILLA_EFFORT",
	"FLOATILLA_MAX_TOKENS",
}

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

	processType := env.String("FLOATILLA_PROCESS_TYPE", "")
	if processType == "" {
		log.Error("FLOATILLA_PROCESS_TYPE is required")
		os.Exit(2)
	}
	natsURL := env.String("NATS_URL", "nats://localhost:4222")

	startCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	b, err := bus.Connect(startCtx, natsURL, "floatilla-launcher-"+processType)
	if err != nil {
		return err
	}
	defer b.Close()

	sandboxEnv := map[string]string{}
	for _, k := range passthrough {
		if v := os.Getenv(k); v != "" {
			sandboxEnv[k] = v
		}
	}
	extra := strings.FieldsFunc(env.String("FLOATILLA_SANDBOX_ENV", ""), func(r rune) bool { return r == ',' })
	for _, k := range extra {
		if v := os.Getenv(k); v != "" {
			sandboxEnv[k] = v
		}
	}

	l := launcher.New(launcher.Config{
		ProcessType:   processType,
		Concurrency:   env.Int("FLOATILLA_CONCURRENCY", 4),
		Network:       env.String("FLOATILLA_DOCKER_NETWORK", "floatilla"),
		NATSURL:       env.String("FLOATILLA_SANDBOX_NATS_URL", natsURL),
		ImageOverride: env.String("FLOATILLA_SANDBOX_IMAGE", ""),
		Env:           sandboxEnv,
		KeepSandboxes: env.Bool("FLOATILLA_KEEP_SANDBOXES", false),
	}, b, launcher.NewDocker(env.String("DOCKER_SOCKET", "/var/run/docker.sock")), log)
	return l.Run(ctx)
}
