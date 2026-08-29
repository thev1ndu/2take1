// Package main implements websocket-gateway: the only piece of this project
// a browser talks to directly. It pushes a live todo-list snapshot on
// connect, forwards chat messages into the agent:tasks Redis Stream for
// agent-worker to pick up, and relays agent-worker's replies (and
// todos-changed notifications) back over the same socket via Redis Pub/Sub.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

const agentTaskStream = "agent:tasks"

type chatMessage struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("websocket-gateway: ")

	redisAddr := env("REDIS_ADDR", "redis:6379")
	mcpURL := env("MCP_SERVER_URL", "http://mcp-server:8081/mcp")
	rdb := redis.NewClient(&redis.Options{Addr: redisAddr})
	defer rdb.Close()

	boot, cancelBoot := context.WithTimeout(context.Background(), 30*time.Second)
	if err := waitReady(boot, rdb); err != nil {
		log.Fatal(err)
	}
	cancelBoot()
	log.Print("redis ready")

	h := newHub()

	ctx, cancel := context.WithCancel(context.Background())
	go subscribeUpdates(ctx, rdb, mcpURL, h)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if err := rdb.Ping(r.Context()).Err(); err != nil {
			http.Error(w, "redis unreachable", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		handleWS(w, r, rdb, mcpURL, h)
	})

	httpSrv := &http.Server{
		Addr:              ":" + env("PORT", "8082"),
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	drained := make(chan struct{})
	go func() {
		defer close(drained)
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		cancel()
		shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancelShutdown()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()

	log.Printf("listening on %s", httpSrv.Addr)
	if err := httpSrv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("serve: %v", err)
	}
	<-drained
}

func handleWS(w http.ResponseWriter, r *http.Request, rdb *redis.Client, mcpURL string, h *hub) {
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		log.Printf("accept: %v", err)
		return
	}
	sessionID := uuid.NewString()
	h.register(sessionID, conn)
	defer func() {
		h.unregister(sessionID)
		conn.Close(websocket.StatusNormalClosure, "")
	}()

	ctx := r.Context()
	if todos, err := callTool(ctx, mcpURL, "list_todos"); err == nil {
		_ = wsjson.Write(ctx, conn, map[string]any{"type": "todos", "todos": json.RawMessage(todos)})
	} else {
		log.Printf("initial list_todos: %v", err)
	}

	for {
		var msg chatMessage
		if err := wsjson.Read(ctx, conn, &msg); err != nil {
			return
		}

		switch msg.Type {
		case "chat":
			if strings.TrimSpace(msg.Text) == "" {
				continue
			}
			err := rdb.XAdd(ctx, &redis.XAddArgs{
				Stream: agentTaskStream,
				Values: map[string]any{"session_id": sessionID, "message": msg.Text},
			}).Err()
			if err != nil {
				log.Printf("queueing chat message: %v", err)
			}
		case "refresh":
			// After a manual add/toggle/delete against `api` directly, the
			// browser asks for a fresh priority-merged snapshot rather than
			// re-deriving it client-side.
			if todos, err := callTool(ctx, mcpURL, "list_todos"); err == nil {
				_ = wsjson.Write(ctx, conn, map[string]any{"type": "todos", "todos": json.RawMessage(todos)})
			}
		}
	}
}

// subscribeUpdates relays agent-worker's Redis Pub/Sub output back to
// browsers: a message on `chat:<session_id>` goes to that one connection, a
// message on `todos:changed` re-fetches the merged list and goes to every
// connection.
func subscribeUpdates(ctx context.Context, rdb *redis.Client, mcpURL string, h *hub) {
	sub := rdb.PSubscribe(ctx, "todos:changed", "chat:*")
	defer sub.Close()

	for msg := range sub.Channel() {
		if msg.Channel == "todos:changed" {
			todos, err := callTool(ctx, mcpURL, "list_todos")
			if err != nil {
				log.Printf("list_todos on change: %v", err)
				continue
			}
			h.broadcast(ctx, map[string]any{"type": "todos", "todos": json.RawMessage(todos)})
			continue
		}
		if sessionID, ok := strings.CutPrefix(msg.Channel, "chat:"); ok {
			h.sendTo(ctx, sessionID, map[string]any{"type": "chat_reply", "text": msg.Payload})
		}
	}
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
