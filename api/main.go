package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	maxTextLen  = 500
	maxBodySize = 4 << 10
)

type todo struct {
	ID      int64     `json:"id"`
	Text    string    `json:"text"`
	Done    bool      `json:"done"`
	Created time.Time `json:"created"`
}

type server struct {
	db *sql.DB
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("klyde-api: ")

	db, err := open()
	if err != nil {
		log.Fatalf("configure database: %v", err)
	}
	defer db.Close()

	boot, cancelBoot := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancelBoot()
	if err := waitReady(boot, db); err != nil {
		log.Fatal(err)
	}
	if _, err := db.ExecContext(boot, schema); err != nil {
		log.Fatalf("apply schema: %v", err)
	}
	log.Print("database ready")

	srv := &server{db: db}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/healthz", srv.health)
	mux.HandleFunc("GET /api/todos", srv.list)
	mux.HandleFunc("POST /api/todos", srv.create)
	mux.HandleFunc("PATCH /api/todos/{id}", srv.update)
	mux.HandleFunc("DELETE /api/todos/{id}", srv.remove)

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

func (s *server) health(w http.ResponseWriter, r *http.Request) {
	if err := s.db.PingContext(r.Context()); err != nil {
		log.Printf("health: %v", err)
		http.Error(w, "database unreachable", http.StatusServiceUnavailable)
		return
	}
	w.Write([]byte("ok\n"))
}

func (s *server) list(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.QueryContext(r.Context(),
		"SELECT id, title, done, created FROM todos ORDER BY id")
	if err != nil {
		fail(w, err)
		return
	}
	defer rows.Close()

	todos := []todo{}
	for rows.Next() {
		var t todo
		if err := rows.Scan(&t.ID, &t.Text, &t.Done, &t.Created); err != nil {
			fail(w, err)
			return
		}
		todos = append(todos, t)
	}
	if err := rows.Err(); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, todos)
}

func (s *server) create(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text string `json:"text"`
	}
	if !decode(w, r, &body) {
		return
	}
	text, ok := cleanText(w, body.Text)
	if !ok {
		return
	}

	res, err := s.db.ExecContext(r.Context(),
		"INSERT INTO todos (title) VALUES (?)", text)
	if err != nil {
		fail(w, err)
		return
	}
	id, err := res.LastInsertId()
	if err != nil {
		fail(w, err)
		return
	}

	t, err := s.byID(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, t)
}

func (s *server) update(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var body struct {
		Text *string `json:"text"`
		Done *bool   `json:"done"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.Text == nil && body.Done == nil {
		http.Error(w, "nothing to update", http.StatusBadRequest)
		return
	}

	sets := make([]string, 0, 2)
	args := make([]any, 0, 3)
	if body.Text != nil {
		text, ok := cleanText(w, *body.Text)
		if !ok {
			return
		}
		sets = append(sets, "title = ?")
		args = append(args, text)
	}
	if body.Done != nil {
		sets = append(sets, "done = ?")
		args = append(args, *body.Done)
	}
	args = append(args, id)

	res, err := s.db.ExecContext(r.Context(),
		"UPDATE todos SET "+strings.Join(sets, ", ")+" WHERE id = ?", args...)
	if err != nil {
		fail(w, err)
		return
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		if _, err := s.byID(r.Context(), id); errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "no such todo", http.StatusNotFound)
			return
		}
	}

	t, err := s.byID(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "no such todo", http.StatusNotFound)
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (s *server) remove(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	res, err := s.db.ExecContext(r.Context(), "DELETE FROM todos WHERE id = ?", id)
	if err != nil {
		fail(w, err)
		return
	}
	n, err := res.RowsAffected()
	if err != nil {
		fail(w, err)
		return
	}
	if n == 0 {
		http.Error(w, "no such todo", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) byID(ctx context.Context, id int64) (todo, error) {
	var t todo
	err := s.db.QueryRowContext(ctx,
		"SELECT id, title, done, created FROM todos WHERE id = ?", id).
		Scan(&t.ID, &t.Text, &t.Done, &t.Created)
	return t, err
}

func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return 0, false
	}
	return id, true
}

func cleanText(w http.ResponseWriter, s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		http.Error(w, "text is required", http.StatusBadRequest)
		return "", false
	}
	if len([]rune(s)) > maxTextLen {
		http.Error(w, "text is too long", http.StatusBadRequest)
		return "", false
	}
	return s, true
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodySize))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
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

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
