package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
)

const maxBodySize = 4 << 10

// server holds the dependencies HTTP handlers need — just the store for
// now. No Claude client, no JWT validation (those land in later
// milestones per agent/PLAN.md).
type server struct {
	store *Store
}

// newMux builds the HTTP handler for the backend: /api/healthz and
// /api/readyz for kubelet probes, and the session/message CRUD routes.
func newMux(s *server) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/healthz", s.healthz)
	mux.HandleFunc("GET /api/readyz", s.readyz)
	mux.HandleFunc("POST /api/sessions", s.createSession)
	mux.HandleFunc("GET /api/sessions", s.listSessions)
	mux.HandleFunc("GET /api/sessions/{id}", s.getSession)
	mux.HandleFunc("DELETE /api/sessions/{id}", s.deleteSession)
	mux.HandleFunc("POST /api/sessions/{id}/messages", s.appendMessage)
	return mux
}

// healthz is a pure liveness check: the process can answer HTTP at all.
func (s *server) healthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

// readyz additionally confirms the bbolt store is open.
func (s *server) readyz(w http.ResponseWriter, _ *http.Request) {
	if err := s.store.Ready(); err != nil {
		log.Printf("readyz: %v", err)
		http.Error(w, "database unavailable", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

func (s *server) createSession(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Title string `json:"title"`
	}
	if !decodeBody(w, r, &body) {
		return
	}

	sess, err := s.store.CreateSession(strings.TrimSpace(body.Title))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, sess)
}

func (s *server) listSessions(w http.ResponseWriter, r *http.Request) {
	sessions, err := s.store.ListSessions()
	if err != nil {
		fail(w, err)
		return
	}
	if sessions == nil {
		sessions = []Session{}
	}
	writeJSON(w, http.StatusOK, sessions)
}

func (s *server) getSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	sess, err := s.store.GetSession(id)
	if errors.Is(err, ErrNotFound) {
		http.Error(w, "no such session", http.StatusNotFound)
		return
	}
	if err != nil {
		fail(w, err)
		return
	}

	messages, err := s.store.ListMessages(id)
	if err != nil {
		fail(w, err)
		return
	}
	if messages == nil {
		messages = []Message{}
	}

	writeJSON(w, http.StatusOK, SessionDetail{Session: sess, Messages: messages})
}

func (s *server) deleteSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	err := s.store.DeleteSession(id)
	if errors.Is(err, ErrNotFound) {
		http.Error(w, "no such session", http.StatusNotFound)
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// appendMessage is the storage primitive milestone 4's Claude tool-use loop
// will call after each turn. No Claude call happens here — just persisting
// a role/content pair to the session's message log.
func (s *server) appendMessage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var body struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	if !decodeBody(w, r, &body) {
		return
	}

	role := strings.TrimSpace(body.Role)
	content := strings.TrimSpace(body.Content)
	if role != "user" && role != "assistant" {
		http.Error(w, `role must be "user" or "assistant"`, http.StatusBadRequest)
		return
	}
	if content == "" {
		http.Error(w, "content is required", http.StatusBadRequest)
		return
	}

	msg, err := s.store.AppendMessage(id, role, content)
	if errors.Is(err, ErrNotFound) {
		http.Error(w, "no such session", http.StatusNotFound)
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, msg)
}

// decodeBody reads and JSON-decodes the request body into v. An empty body
// is treated as "use defaults" rather than an error, since POST /api/sessions
// is valid with no body at all.
func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodySize))
	if err != nil {
		http.Error(w, "request body too large", http.StatusBadRequest)
		return false
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return true
	}
	if err := json.Unmarshal(data, v); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("write response: %v", err)
	}
}

func fail(w http.ResponseWriter, err error) {
	log.Printf("store: %v", err)
	http.Error(w, "storage unavailable", http.StatusBadGateway)
}
