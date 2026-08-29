// Package main implements the klyde-agent backend: a chat-session CRUD API
// backed by an embedded bbolt key-value store on disk.
//
// Key scheme (chosen up front so a later swap to Redis stays a storage-layer
// change, not a rewrite — see agent/PLAN.md "Known limitations"):
//   - bucket "sessions": key "session:<id>"        -> JSON Session
//   - bucket "messages": key "msg:<session>:<seq>" -> JSON Message
//
// Message keys zero-pad seq to a fixed width so bbolt's lexicographic key
// iteration order equals chronological order.
package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	bolt "go.etcd.io/bbolt"
)

const (
	bucketSessions = "sessions"
	bucketMessages = "messages"

	defaultSessionTitle = "New session"
)

// ErrNotFound is returned by Store methods when the requested session does
// not exist.
var ErrNotFound = errors.New("not found")

// Session is a chat session's metadata, stored as JSON under
// "session:<id>" in the sessions bucket.
type Session struct {
	ID      string `json:"id"`
	Created string `json:"created"`
	Updated string `json:"updated"`
	Title   string `json:"title"`
}

// Message is one chat turn, stored as JSON under "msg:<session>:<seq>" in
// the messages bucket.
type Message struct {
	Session string `json:"session"`
	Seq     int    `json:"seq"`
	Role    string `json:"role"`
	Content string `json:"content"`
	Created string `json:"created"`
}

// SessionDetail is a session together with its messages in chronological
// order, the shape returned by GET /api/sessions/{id}.
type SessionDetail struct {
	Session
	Messages []Message `json:"messages"`
}

// Store wraps a bbolt database, exposing session/message CRUD as the
// storage-layer seam. Every method opens its own transaction; bbolt permits
// a single writer at a time (documented limitation, see PLAN.md).
type Store struct {
	db *bolt.DB
}

// openStore opens (creating if absent) the bbolt file at path and ensures
// the sessions/messages buckets exist.
func openStore(path string) (*Store, error) {
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("open bbolt file %s: %w", path, err)
	}

	err = db.Update(func(tx *bolt.Tx) error {
		if _, err := tx.CreateBucketIfNotExists([]byte(bucketSessions)); err != nil {
			return err
		}
		if _, err := tx.CreateBucketIfNotExists([]byte(bucketMessages)); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create buckets: %w", err)
	}

	return &Store{db: db}, nil
}

// Close closes the underlying bbolt file.
func (s *Store) Close() error {
	return s.db.Close()
}

// Ready reports whether the bbolt database is open and usable, for the
// readiness probe.
func (s *Store) Ready() error {
	return s.db.View(func(tx *bolt.Tx) error { return nil })
}

func newID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand.Read failing is effectively unrecoverable; fall back
		// to a timestamp so callers never see an empty ID.
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

func nowRFC3339() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

func sessionKey(id string) []byte {
	return []byte("session:" + id)
}

func messagePrefix(sessionID string) []byte {
	return []byte(fmt.Sprintf("msg:%s:", sessionID))
}

func messageKey(sessionID string, seq int) []byte {
	// 10 digits, zero-padded: lexicographic order == numeric order up to
	// 9,999,999,999 messages per session.
	return []byte(fmt.Sprintf("msg:%s:%010d", sessionID, seq))
}

// CreateSession creates a new session with the given title (defaulted if
// empty) and returns it.
func (s *Store) CreateSession(title string) (Session, error) {
	if title == "" {
		title = defaultSessionTitle
	}
	now := nowRFC3339()
	sess := Session{ID: newID(), Created: now, Updated: now, Title: title}

	err := s.db.Update(func(tx *bolt.Tx) error {
		data, err := json.Marshal(sess)
		if err != nil {
			return err
		}
		return tx.Bucket([]byte(bucketSessions)).Put(sessionKey(sess.ID), data)
	})
	if err != nil {
		return Session{}, err
	}
	return sess, nil
}

// ListSessions returns all sessions, most-recently-updated first.
func (s *Store) ListSessions() ([]Session, error) {
	var sessions []Session
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(bucketSessions)).ForEach(func(_, v []byte) error {
			var sess Session
			if err := json.Unmarshal(v, &sess); err != nil {
				return err
			}
			sessions = append(sessions, sess)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}

	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].Updated > sessions[j].Updated
	})
	return sessions, nil
}

// GetSession fetches one session by id, or ErrNotFound.
func (s *Store) GetSession(id string) (Session, error) {
	var sess Session
	found := false

	err := s.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket([]byte(bucketSessions)).Get(sessionKey(id))
		if v == nil {
			return nil
		}
		found = true
		return json.Unmarshal(v, &sess)
	})
	if err != nil {
		return Session{}, err
	}
	if !found {
		return Session{}, ErrNotFound
	}
	return sess, nil
}

// ListMessages returns all messages for a session in chronological
// (ascending seq) order.
func (s *Store) ListMessages(sessionID string) ([]Message, error) {
	var messages []Message
	prefix := messagePrefix(sessionID)

	err := s.db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket([]byte(bucketMessages)).Cursor()
		for k, v := c.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, v = c.Next() {
			var m Message
			if err := json.Unmarshal(v, &m); err != nil {
				return err
			}
			messages = append(messages, m)
		}
		return nil
	})
	return messages, err
}

// AppendMessage appends a message to a session, advancing its per-session
// sequence counter and bumping the session's Updated timestamp. Returns
// ErrNotFound if the session does not exist.
func (s *Store) AppendMessage(sessionID, role, content string) (Message, error) {
	var msg Message

	err := s.db.Update(func(tx *bolt.Tx) error {
		sessions := tx.Bucket([]byte(bucketSessions))
		sv := sessions.Get(sessionKey(sessionID))
		if sv == nil {
			return ErrNotFound
		}
		var sess Session
		if err := json.Unmarshal(sv, &sess); err != nil {
			return err
		}

		messages := tx.Bucket([]byte(bucketMessages))
		prefix := messagePrefix(sessionID)
		seq := 0
		c := messages.Cursor()
		for k, _ := c.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, _ = c.Next() {
			seq++
		}

		now := nowRFC3339()
		msg = Message{Session: sessionID, Seq: seq, Role: role, Content: content, Created: now}
		data, err := json.Marshal(msg)
		if err != nil {
			return err
		}
		if err := messages.Put(messageKey(sessionID, seq), data); err != nil {
			return err
		}

		sess.Updated = now
		sdata, err := json.Marshal(sess)
		if err != nil {
			return err
		}
		return sessions.Put(sessionKey(sessionID), sdata)
	})
	if err != nil {
		return Message{}, err
	}
	return msg, nil
}

// DeleteSession removes a session and all of its messages. Returns
// ErrNotFound if the session does not exist.
func (s *Store) DeleteSession(id string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		sessions := tx.Bucket([]byte(bucketSessions))
		if sessions.Get(sessionKey(id)) == nil {
			return ErrNotFound
		}
		if err := sessions.Delete(sessionKey(id)); err != nil {
			return err
		}

		messages := tx.Bucket([]byte(bucketMessages))
		prefix := messagePrefix(id)
		var keys [][]byte
		c := messages.Cursor()
		for k, _ := c.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, _ = c.Next() {
			keys = append(keys, append([]byte(nil), k...))
		}
		for _, k := range keys {
			if err := messages.Delete(k); err != nil {
				return err
			}
		}
		return nil
	})
}
