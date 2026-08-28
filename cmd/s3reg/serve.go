package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/stephenwilliams/s3-registry/internal/health"
	"github.com/stephenwilliams/s3-registry/internal/server"
)

func newServeCmd() *cobra.Command {
	var addr string
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the HTTP proxy server",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runServe(cmd.Context(), addr)
		},
	}
	cmd.Flags().StringVar(&addr, "addr", envOr("S3REG_ADDR", ":8080"), "listen address")
	return cmd
}

func runServe(ctx context.Context, addr string) error {
	st, err := newStore(ctx)
	if err != nil {
		return err
	}
	cfg := server.Config{
		APIToken:   os.Getenv("S3REG_API_TOKEN"),
		PresignTTL: envDuration("S3REG_PRESIGN_TTL", 5*time.Minute),
		CacheTTL:   envDuration("S3REG_CACHE_TTL", 60*time.Second),
		Health: health.Config{
			LivenessDeadline:      envDuration("LIVENESS_DEADLINE", 200*time.Millisecond),
			ReadinessDeadline:     envDuration("READINESS_DEADLINE", 500*time.Millisecond),
			StatusDeadline:        envDuration("STATUS_DEADLINE", time.Second),
			StatusCacheTTL:        envDuration("STATUS_CACHE_TTL", time.Second),
			ReadinessStartupGrace: envDuration("READINESS_STARTUP_GRACE", 15*time.Second),
		},
	}
	srv := server.New(cfg, st, nil)

	httpServer := &http.Server{
		Addr:              addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	sigCtx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		fmt.Fprintf(os.Stderr, "s3reg serving on %s\n", addr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-sigCtx.Done():
		fmt.Fprintln(os.Stderr, "shutdown signal received, draining")
		srv.Health().SetDraining()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()
		return httpServer.Shutdown(shutdownCtx)
	}
}

func envDuration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}
