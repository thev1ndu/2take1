package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
)

const priorityHashKey = "todo:priority"

// apiTodo mirrors the shape api/main.go's todo struct serializes to JSON.
type apiTodo struct {
	ID      int64  `json:"id"`
	Text    string `json:"text"`
	Done    bool   `json:"done"`
	Created string `json:"created"`
}

// mergedTodo is what tools return to the agent: api/'s todo plus the
// priority api/ has no concept of.
type mergedTodo struct {
	ID       int64  `json:"id"`
	Text     string `json:"text"`
	Done     bool   `json:"done"`
	Priority int    `json:"priority"`
}

func (s *Server) fetchTodos(ctx context.Context) ([]apiTodo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.apiBaseURL+"/api/todos", nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling api: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("api returned %d: %s", resp.StatusCode, body)
	}

	var todos []apiTodo
	if err := json.NewDecoder(resp.Body).Decode(&todos); err != nil {
		return nil, fmt.Errorf("decoding api response: %w", err)
	}
	return todos, nil
}

// mergeWithPriority attaches each todo's priority from Redis (default 0)
// and returns the list sorted by priority descending, then id ascending.
func (s *Server) mergeWithPriority(ctx context.Context, todos []apiTodo) ([]mergedTodo, error) {
	priorities, err := s.redis.HGetAll(ctx, priorityHashKey).Result()
	if err != nil {
		return nil, fmt.Errorf("reading priorities: %w", err)
	}

	merged := make([]mergedTodo, 0, len(todos))
	for _, t := range todos {
		priority := 0
		if v, ok := priorities[strconv.FormatInt(t.ID, 10)]; ok {
			if p, err := strconv.Atoi(v); err == nil {
				priority = p
			}
		}
		merged = append(merged, mergedTodo{ID: t.ID, Text: t.Text, Done: t.Done, Priority: priority})
	}

	sort.Slice(merged, func(i, j int) bool {
		if merged[i].Priority != merged[j].Priority {
			return merged[i].Priority > merged[j].Priority
		}
		return merged[i].ID < merged[j].ID
	})
	return merged, nil
}

func (s *Server) runListTodos(ctx context.Context) (string, error) {
	todos, err := s.fetchTodos(ctx)
	if err != nil {
		return "", err
	}
	merged, err := s.mergeWithPriority(ctx, todos)
	if err != nil {
		return "", err
	}
	out, err := json.Marshal(merged)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func (s *Server) runAddTodo(ctx context.Context, args map[string]any) (string, error) {
	text, ok := args["text"].(string)
	if !ok || text == "" {
		return "", fmt.Errorf("add_todo: 'text' is required")
	}

	body, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.apiBaseURL+"/api/todos", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("calling api: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("api returned %d: %s", resp.StatusCode, respBody)
	}

	var created apiTodo
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		return "", fmt.Errorf("decoding api response: %w", err)
	}
	out, err := json.Marshal(mergedTodo{ID: created.ID, Text: created.Text, Done: created.Done, Priority: 0})
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func (s *Server) runSetPriority(ctx context.Context, args map[string]any) (string, error) {
	id, err := requireIntArg(args, "id")
	if err != nil {
		return "", err
	}
	priority, err := requireIntArg(args, "priority")
	if err != nil {
		return "", err
	}
	if priority < 0 || priority > 100 {
		return "", fmt.Errorf("set_priority: 'priority' must be between 0 and 100, got %d", priority)
	}

	if err := s.redis.HSet(ctx, priorityHashKey, strconv.FormatInt(id, 10), priority).Err(); err != nil {
		return "", fmt.Errorf("storing priority: %w", err)
	}
	return fmt.Sprintf("todo %d priority set to %d", id, priority), nil
}

func (s *Server) runCompleteTodo(ctx context.Context, args map[string]any) (string, error) {
	id, err := requireIntArg(args, "id")
	if err != nil {
		return "", err
	}
	if err := s.patchTodo(ctx, id, map[string]any{"done": true}); err != nil {
		return "", err
	}
	return fmt.Sprintf("todo %d marked done", id), nil
}

func (s *Server) runDeleteTodo(ctx context.Context, args map[string]any) (string, error) {
	id, err := requireIntArg(args, "id")
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodDelete,
		fmt.Sprintf("%s/api/todos/%d", s.apiBaseURL, id), nil)
	if err != nil {
		return "", err
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("calling api: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("api returned %d: %s", resp.StatusCode, body)
	}

	if err := s.redis.HDel(ctx, priorityHashKey, strconv.FormatInt(id, 10)).Err(); err != nil {
		return "", fmt.Errorf("todo deleted but clearing priority failed: %w", err)
	}
	return fmt.Sprintf("todo %d deleted", id), nil
}

func (s *Server) patchTodo(ctx context.Context, id int64, fields map[string]any) error {
	body, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch,
		fmt.Sprintf("%s/api/todos/%d", s.apiBaseURL, id), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("calling api: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("api returned %d: %s", resp.StatusCode, respBody)
	}
	return nil
}

// requireIntArg reads an integer argument out of a tool's arguments map.
// JSON numbers decode to float64 in Go's `any`, so this accepts that as
// well as a literal int (the fake/test-construction path).
func requireIntArg(args map[string]any, name string) (int64, error) {
	v, ok := args[name]
	if !ok {
		return 0, fmt.Errorf("'%s' is required", name)
	}
	switch n := v.(type) {
	case float64:
		return int64(n), nil
	case int64:
		return n, nil
	case int:
		return int64(n), nil
	default:
		return 0, fmt.Errorf("'%s' must be a number", name)
	}
}
