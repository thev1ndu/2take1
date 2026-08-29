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

	"github.com/redis/go-redis/v9"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("mcp-server: ")

	rdb := redis.NewClient(&redis.Options{
		Addr: env("REDIS_ADDR", "redis:6379"),
	})
	defer rdb.Close()

	boot, cancelBoot := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelBoot()
	if err := waitReady(boot, rdb); err != nil {
		log.Fatal(err)
	}
	log.Print("redis ready")

	s := &Server{
		apiBaseURL: env("API_BASE_URL", "http://api.2take1.svc.cluster.local:8080"),
		httpClient: &http.Client{Timeout: 10 * time.Second},
		redis:      rdb,
	}

	httpSrv := &http.Server{
		Addr:              ":" + env("PORT", "8081"),
		Handler:           NewMux(s),
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

	log.Printf("listening on %s, proxying todos to %s", httpSrv.Addr, s.apiBaseURL)
	if err := httpSrv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("serve: %v", err)
	}
	<-drained
}

func waitReady(ctx context.Context, rdb *redis.Client) error {
	const retry = 2 * time.Second
	for attempt := 1; ; attempt++ {
		pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := rdb.Ping(pingCtx).Err()
		cancel()
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return errors.New("redis never became ready: " + err.Error())
		}
		log.Printf("redis not ready (attempt %d): %v", attempt, err)

		select {
		case <-time.After(retry):
		case <-ctx.Done():
			return errors.New("redis never became ready: " + err.Error())
		}
	}
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
