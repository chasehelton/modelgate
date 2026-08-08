// Command modelgate serves model rollout assignments over HTTP.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/chasehelton/modelgate/internal/httpapi"
	"github.com/chasehelton/modelgate/internal/rollout"
	"github.com/chasehelton/modelgate/internal/store"
)

func main() {
	// Structured JSON logs. In K8s your logs go to stdout and a collector ships
	// them; JSON means fields stay queryable instead of you regexing a string.
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	addr := ":" + env("PORT", "8080")

	st := store.NewMemory()

	// Simulated startup work. Until Seed() runs, /readyz returns 503 and K8s
	// keeps traffic off this pod. Bump STARTUP_DELAY_SECONDS to watch a rolling
	// update wait for readiness instead of blindly cutting over.
	delay, _ := strconv.Atoi(env("STARTUP_DELAY_SECONDS", "0"))
	go func() {
		if delay > 0 {
			log.Info("warming up", "seconds", delay)
			time.Sleep(time.Duration(delay) * time.Second)
		}
		st.Seed([]rollout.Model{
			{ID: "gpt-5-mini", Percent: 25},
			{ID: "gpt-5-preview", Percent: 0},
		})
		log.Info("store seeded; now ready")
	}()

	srv := &http.Server{
		Addr:              addr,
		Handler:           httpapi.New(st, log).Routes(),
		ReadHeaderTimeout: 5 * time.Second, // cheap slowloris protection
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// Graceful shutdown. This is the part that makes K8s rolling updates
	// lossless. On SIGTERM we stop accepting new connections and let in-flight
	// requests finish. Without it, every deploy drops the requests that were
	// mid-flight -- a small, constant, very confusing error rate.
	idleClosed := make(chan struct{})
	go func() {
		sigint := make(chan os.Signal, 1)
		signal.Notify(sigint, os.Interrupt, syscall.SIGTERM)
		<-sigint

		log.Info("shutdown signal received; draining")
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			log.Error("graceful shutdown failed", "err", err)
		}
		close(idleClosed)
	}()

	log.Info("listening", "addr", addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("server failed", "err", err)
		os.Exit(1)
	}

	<-idleClosed
	log.Info("shutdown complete")
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// TODO(exercise 6): K8s sends SIGTERM and removes the pod from the Service
// endpoints at the SAME time, and those propagate at different speeds. The
// standard fix is a short sleep BEFORE Shutdown starts, so load balancers stop
// sending traffic first. Add it, and write down why in the README.
