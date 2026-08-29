package main

import (
	"context"
	"sync"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// hub tracks every connected browser tab by the session id assigned at
// connect time, so a chat reply (keyed by session id on the Redis pub/sub
// channel) can be routed back to the one tab that asked, while a
// todos-changed notification goes to everyone.
type hub struct {
	mu    sync.Mutex
	conns map[string]*websocket.Conn
}

func newHub() *hub {
	return &hub{conns: make(map[string]*websocket.Conn)}
}

func (h *hub) register(sessionID string, conn *websocket.Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.conns[sessionID] = conn
}

func (h *hub) unregister(sessionID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.conns, sessionID)
}

func (h *hub) sendTo(ctx context.Context, sessionID string, v any) {
	h.mu.Lock()
	conn, ok := h.conns[sessionID]
	h.mu.Unlock()
	if !ok {
		return
	}
	_ = wsjson.Write(ctx, conn, v)
}

func (h *hub) broadcast(ctx context.Context, v any) {
	h.mu.Lock()
	conns := make([]*websocket.Conn, 0, len(h.conns))
	for _, c := range h.conns {
		conns = append(conns, c)
	}
	h.mu.Unlock()

	for _, conn := range conns {
		_ = wsjson.Write(ctx, conn, v)
	}
}
