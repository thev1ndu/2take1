package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Seam under test: the HTTP handler mux returned by newMux(), backed by a
// real bbolt file in a temp dir. This is the same boundary the milestone 4
// Claude tool-use loop and kubelet probes hit — tests never reach into Store
// internals directly.

func newTestServer(t *testing.T) http.Handler {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "klyde.db")
	store, err := openStore(dbPath)
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return newMux(&server{store: store})
}

func doJSON(t *testing.T, mux http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reqBody *strings.Reader
	if body == "" {
		reqBody = strings.NewReader("")
	} else {
		reqBody = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reqBody)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func decodeJSON(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("response is not valid JSON: %v\nbody: %s", err, rec.Body.String())
	}
}

func TestHealthz(t *testing.T) {
	mux := newTestServer(t)
	rec := doJSON(t, mux, http.MethodGet, "/api/healthz", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

func TestReadyz(t *testing.T) {
	mux := newTestServer(t)
	rec := doJSON(t, mux, http.MethodGet, "/api/readyz", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

func TestCreateSessionReturnsSessionWithID(t *testing.T) {
	mux := newTestServer(t)
	rec := doJSON(t, mux, http.MethodPost, "/api/sessions", `{"title":"my chat"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var sess Session
	decodeJSON(t, rec, &sess)
	if sess.ID == "" {
		t.Fatalf("expected non-empty session id, got %+v", sess)
	}
	if sess.Title != "my chat" {
		t.Fatalf("expected title %q, got %q", "my chat", sess.Title)
	}
	if sess.Created == "" || sess.Updated == "" {
		t.Fatalf("expected created/updated timestamps, got %+v", sess)
	}
}

func TestCreateSessionWithoutTitleGetsDefault(t *testing.T) {
	mux := newTestServer(t)
	rec := doJSON(t, mux, http.MethodPost, "/api/sessions", "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var sess Session
	decodeJSON(t, rec, &sess)
	if sess.Title == "" {
		t.Fatalf("expected a default title, got empty")
	}
}

func TestListSessionsMostRecentlyUpdatedFirst(t *testing.T) {
	mux := newTestServer(t)

	rec1 := doJSON(t, mux, http.MethodPost, "/api/sessions", `{"title":"first"}`)
	var s1 Session
	decodeJSON(t, rec1, &s1)

	rec2 := doJSON(t, mux, http.MethodPost, "/api/sessions", `{"title":"second"}`)
	var s2 Session
	decodeJSON(t, rec2, &s2)

	// Touch s1 so it becomes the most recently updated.
	rec3 := doJSON(t, mux, http.MethodPost, "/api/sessions/"+s1.ID+"/messages", `{"role":"user","content":"hello"}`)
	if rec3.Code != http.StatusCreated {
		t.Fatalf("expected 201 appending message, got %d: %s", rec3.Code, rec3.Body.String())
	}

	rec := doJSON(t, mux, http.MethodGet, "/api/sessions", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var sessions []Session
	decodeJSON(t, rec, &sessions)
	if len(sessions) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(sessions))
	}
	if sessions[0].ID != s1.ID {
		t.Fatalf("expected most-recently-updated session %q first, got %+v", s1.ID, sessions)
	}
}

func TestGetSessionReturnsSessionAndMessagesInOrder(t *testing.T) {
	mux := newTestServer(t)

	rec := doJSON(t, mux, http.MethodPost, "/api/sessions", `{"title":"chat"}`)
	var sess Session
	decodeJSON(t, rec, &sess)

	doJSON(t, mux, http.MethodPost, "/api/sessions/"+sess.ID+"/messages", `{"role":"user","content":"one"}`)
	doJSON(t, mux, http.MethodPost, "/api/sessions/"+sess.ID+"/messages", `{"role":"assistant","content":"two"}`)
	doJSON(t, mux, http.MethodPost, "/api/sessions/"+sess.ID+"/messages", `{"role":"user","content":"three"}`)

	getRec := doJSON(t, mux, http.MethodGet, "/api/sessions/"+sess.ID, "")
	if getRec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", getRec.Code, getRec.Body.String())
	}

	var detail SessionDetail
	decodeJSON(t, getRec, &detail)
	if detail.ID != sess.ID {
		t.Fatalf("expected session id %q, got %q", sess.ID, detail.ID)
	}
	if len(detail.Messages) != 3 {
		t.Fatalf("expected 3 messages, got %d: %+v", len(detail.Messages), detail.Messages)
	}
	contents := []string{detail.Messages[0].Content, detail.Messages[1].Content, detail.Messages[2].Content}
	if contents[0] != "one" || contents[1] != "two" || contents[2] != "three" {
		t.Fatalf("expected chronological order [one two three], got %v", contents)
	}
}

func TestGetSessionNotFound(t *testing.T) {
	mux := newTestServer(t)
	rec := doJSON(t, mux, http.MethodGet, "/api/sessions/does-not-exist", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestDeleteSessionRemovesSessionAndMessages(t *testing.T) {
	mux := newTestServer(t)

	rec := doJSON(t, mux, http.MethodPost, "/api/sessions", `{"title":"chat"}`)
	var sess Session
	decodeJSON(t, rec, &sess)

	doJSON(t, mux, http.MethodPost, "/api/sessions/"+sess.ID+"/messages", `{"role":"user","content":"hi"}`)

	delRec := doJSON(t, mux, http.MethodDelete, "/api/sessions/"+sess.ID, "")
	if delRec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", delRec.Code, delRec.Body.String())
	}

	getRec := doJSON(t, mux, http.MethodGet, "/api/sessions/"+sess.ID, "")
	if getRec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 after delete, got %d", getRec.Code)
	}
}

func TestDeleteSessionNotFound(t *testing.T) {
	mux := newTestServer(t)
	rec := doJSON(t, mux, http.MethodDelete, "/api/sessions/does-not-exist", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAppendMessageToUnknownSessionReturns404(t *testing.T) {
	mux := newTestServer(t)
	rec := doJSON(t, mux, http.MethodPost, "/api/sessions/does-not-exist/messages", `{"role":"user","content":"hi"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAppendMessageRejectsInvalidRole(t *testing.T) {
	mux := newTestServer(t)
	rec := doJSON(t, mux, http.MethodPost, "/api/sessions", `{"title":"chat"}`)
	var sess Session
	decodeJSON(t, rec, &sess)

	badRec := doJSON(t, mux, http.MethodPost, "/api/sessions/"+sess.ID+"/messages", `{"role":"system","content":"hi"}`)
	if badRec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid role, got %d: %s", badRec.Code, badRec.Body.String())
	}
}

func TestAppendMessageRejectsEmptyContent(t *testing.T) {
	mux := newTestServer(t)
	rec := doJSON(t, mux, http.MethodPost, "/api/sessions", `{"title":"chat"}`)
	var sess Session
	decodeJSON(t, rec, &sess)

	badRec := doJSON(t, mux, http.MethodPost, "/api/sessions/"+sess.ID+"/messages", `{"role":"user","content":""}`)
	if badRec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty content, got %d: %s", badRec.Code, badRec.Body.String())
	}
}

func TestManyMessagesStayInChronologicalOrderPastKeyPadding(t *testing.T) {
	// Regression guard for the zero-padded seq key scheme: bbolt iterates
	// keys lexicographically, so without zero-padding "msg:x:10" would sort
	// before "msg:x:2". This appends past the single-digit boundary.
	mux := newTestServer(t)
	rec := doJSON(t, mux, http.MethodPost, "/api/sessions", `{"title":"chat"}`)
	var sess Session
	decodeJSON(t, rec, &sess)

	for i := 0; i < 12; i++ {
		doJSON(t, mux, http.MethodPost, "/api/sessions/"+sess.ID+"/messages",
			`{"role":"user","content":"msg`+strconv.Itoa(i)+`"}`)
	}

	getRec := doJSON(t, mux, http.MethodGet, "/api/sessions/"+sess.ID, "")
	var detail SessionDetail
	decodeJSON(t, getRec, &detail)
	if len(detail.Messages) != 12 {
		t.Fatalf("expected 12 messages, got %d", len(detail.Messages))
	}
	for i, m := range detail.Messages {
		want := "msg" + strconv.Itoa(i)
		if m.Content != want {
			t.Fatalf("message %d out of order: expected content %q, got %q (full: %+v)", i, want, m.Content, detail.Messages)
		}
		if m.Seq != i {
			t.Fatalf("message %d expected seq %d, got %d", i, i, m.Seq)
		}
	}
}
