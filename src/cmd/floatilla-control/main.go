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

	errc := make(chan error, 2)
	go func() { errc <- ctl.Run(ctx) }()
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
