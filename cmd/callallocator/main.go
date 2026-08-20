// Command callallocator runs the Call Allocation Service.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/kevin907/call-allocation-service/internal/allocation"
	"github.com/kevin907/call-allocation-service/internal/httpapi"
)

type config struct {
	addr            string
	shutdownTimeout time.Duration
	logLevel        slog.Level
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.logLevel}))
	registry := allocation.New()

	srv := &http.Server{
		Addr:    cfg.addr,
		Handler: httpapi.NewServer(registry, log).Handler(),
		// Without these a single stalled client holds a connection indefinitely.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelError),
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Bind before announcing it, so a port clash is reported as a failure to
	// start rather than after a log line claiming success.
	listener, err := net.Listen("tcp", cfg.addr)
	if err != nil {
		return err
	}
	log.Info("listening", "addr", listener.Addr().String())

	serverErr := make(chan error, 1)
	go func() {
		if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	select {
	case err := <-serverErr:
		return err
	case <-ctx.Done():
	}

	// Kubernetes has sent SIGTERM; finish the requests already in flight rather
	// than dropping them, then exit. Everything held in memory goes with us.
	log.Info("shutdown requested, draining")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.shutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	log.Info("shutdown complete")
	return nil
}

// loadConfig reads the environment and fails rather than falling back on a
// default, because a mistyped value that silently reverts is worse than a
// process that refuses to start.
func loadConfig() (config, error) {
	cfg := config{
		addr:            ":8080",
		shutdownTimeout: 10 * time.Second,
		logLevel:        slog.LevelInfo,
	}

	if v, ok := os.LookupEnv("PORT"); ok {
		port, err := strconv.Atoi(v)
		if err != nil || port < 1 || port > 65535 {
			return cfg, fmt.Errorf("PORT: %q is not a valid port", v)
		}
		cfg.addr = ":" + v
	}

	if v, ok := os.LookupEnv("SHUTDOWN_TIMEOUT"); ok {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return cfg, fmt.Errorf("SHUTDOWN_TIMEOUT: %q is not a positive duration", v)
		}
		cfg.shutdownTimeout = d
	}

	if v, ok := os.LookupEnv("LOG_LEVEL"); ok {
		if err := cfg.logLevel.UnmarshalText([]byte(v)); err != nil {
			return cfg, fmt.Errorf("LOG_LEVEL: %q is not one of debug, info, warn, error", v)
		}
	}

	return cfg, nil
}
