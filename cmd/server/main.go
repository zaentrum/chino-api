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

	"github.com/zaentrum/chino-api/internal/config"
	"github.com/zaentrum/chino-api/internal/eventsse"
	chinohttp "github.com/zaentrum/chino-api/internal/http"
	"github.com/zaentrum/chino-api/internal/store"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg := config.Load()
	slog.Info("starting chino-api", "addr", cfg.Addr, "oidc_issuer", cfg.OIDCIssuer, "oidc_audience", cfg.OIDCAudience, "pg", cfg.PgURL != "")

	st, err := store.New(context.Background(), cfg.PgURL)
	if err != nil {
		slog.Error("store init failed", "err", err)
		os.Exit(1)
	}
	defer st.Close()
	if err := st.Migrate(context.Background()); err != nil {
		slog.Error("store migrate failed", "err", err)
		os.Exit(1)
	}

	// Live-refresh SSE bridge: tail the catalog Kafka topics into an in-process
	// broker (inert without brokers); browsers subscribe on /api/v1/events.
	events := eventsse.NewBroker()

	router, err := chinohttp.NewRouter(cfg, st, events)
	if err != nil {
		slog.Error("router init failed", "err", err)
		os.Exit(1)
	}

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           router,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		// WriteTimeout=0 → no per-response deadline. The 30s default
		// was killing /play streams mid-transcode: chino-api stopped
		// writing to the client after 30s of wall time, which cancels
		// the upstream proxy's context, which SIGKILLs ffmpeg. Player
		// saw a truncated empty_moov duration, fired `ended`, and
		// reconnected ~every 20s. Long-lived video responses need no
		// write deadline; the upstream's own context cancel (client
		// disconnect, server shutdown) still terminates the request.
		WriteTimeout: 0,
		IdleTimeout:  120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go events.Run(ctx, cfg.KafkaBrokers, cfg.KafkaCertDir)

	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("listen failed", "err", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}
