// Command backend is the klyde-agent chat-session API (milestone 3).
//
// Scope for this milestone: bbolt-backed session/message CRUD only. No
// Claude/LLM integration, no JWT auth, no web frontend — those land in
// later milestones per agent/PLAN.md. This pod has zero Kubernetes RBAC:
// it never talks to the K8s API, only (in milestone 4) to mcp-server over
// internal HTTP.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("klyde-backend: ")

	dbPath := env("DB_PATH", "/data/klyde.db")
	store, err := openStore(dbPath)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer store.Close()
	log.Printf("bbolt store ready at %s", dbPath)

	srv := &server{store: store}
	mux := newMux(srv)

	httpSrv := &http.Server{
		Addr:              ":" + env("PORT", "8080"),
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	drained := make(chan struct{})
	go func() {
		defer close(drained)
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(ctx); err != nil {
			log.Printf("shutdown: %v", err)
		}
	}()

	log.Printf("listening on %s", httpSrv.Addr)
	if err := httpSrv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("serve: %v", err)
	}
	<-drained
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
