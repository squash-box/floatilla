// floatilla-control is the control plane: FleetService API, dispatcher,
// outbox relay and lease reaper.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/squash-box/floatilla/src/gen/go/floatilla/v1/floatillav1connect"
	"github.com/squash-box/floatilla/src/internal/bus"
	"github.com/squash-box/floatilla/src/internal/control"
	"github.com/squash-box/floatilla/src/internal/env"
	"github.com/squash-box/floatilla/src/internal/fsm"
	"github.com/squash-box/floatilla/src/internal/packs"
	"github.com/squash-box/floatilla/src/internal/store"
	"github.com/squash-box/floatilla/src/internal/tracker"
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

	reg, err := packs.LoadDir(env.String("FLOATILLA_PACKS", "packs"))
	if err != nil {
		return err
	}
	startCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()

	st, err := store.Open(startCtx, env.String("DATABASE_URL", "postgres://floatilla:floatilla@localhost:5432/floatilla"))
	if err != nil {
		return err
	}
	defer st.Close()

	b, err := bus.Connect(startCtx, env.String("NATS_URL", "nats://localhost:4222"), "floatilla-control")
	if err != nil {
		return err
	}
	defer b.Close()
	if err := b.EnsureStreams(startCtx); err != nil {
		return err
	}

	cfg := fsm.DefaultConfig()
	cfg.LeaseTTL = env.Duration("FLOATILLA_LEASE_TTL", cfg.LeaseTTL)
	cfg.MaxAttempts = uint32(env.Int("FLOATILLA_MAX_ATTEMPTS", int(cfg.MaxAttempts)))
	ctl := control.New(st, b, reg, cfg, log)

	mux := http.NewServeMux()
	mux.Handle(floatillav1connect.NewFleetServiceHandler(control.NewAPI(ctl)))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	srv := &http.Server{Addr: env.String("FLOATILLA_LISTEN", ":8080"), Handler: mux, Protocols: protocols}

	errc := make(chan error, 3)
	go func() { errc <- ctl.Run(ctx) }()
	if sync := trackerSync(ctl, st, log); sync != nil {
		go func() { errc <- sync.Run(ctx) }()
	}
	go func() {
		log.Info("api listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	select {
	case <-ctx.Done():
	case err = <-errc:
	}
	shutdown, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelShutdown()
	_ = srv.Shutdown(shutdown)
	return err
}

// trackerSync configures the story tracker sync from the environment; nil
// when no tracker is configured.
func trackerSync(ctl *control.Control, st *store.Store, log *slog.Logger) *tracker.Sync {
	key := env.String("PLANE_API_KEY", "")
	if key == "" {
		log.Info("no story tracker configured (set PLANE_API_KEY)")
		return nil
	}
	plane := tracker.NewPlane(tracker.PlaneConfig{
		BaseURL:   env.String("PLANE_BASE_URL", "https://api.plane.so"),
		AppURL:    env.String("PLANE_APP_URL", ""),
		APIKey:    key,
		Workspace: env.String("PLANE_WORKSPACE", ""),
		Project:   env.String("PLANE_PROJECT", ""),
	})
	publish := tracker.StateBacklog
	if env.String("FLOATILLA_TRACKER_PUBLISH_TO", "backlog") == "ready" {
		publish = tracker.StateReady
	}
	return &tracker.Sync{
		Tracker: plane, Fleet: ctl, Links: st, Log: log.With("component", "tracker"),
		ImplType:     env.String("FLOATILLA_TRACKER_PROCESS_TYPE", "impl"),
		PlanType:     env.String("FLOATILLA_TRACKER_PLAN_TYPE", "plan"),
		Repo:         env.String("FLOATILLA_TRACKER_REPO", ""),
		PublishState: publish,
		Interval:     env.Duration("FLOATILLA_TRACKER_INTERVAL", 30*time.Second),
	}
}
