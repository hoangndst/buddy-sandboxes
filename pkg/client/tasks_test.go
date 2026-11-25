package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	apiv1 "github.com/hoangndst/buddy-sandboxes/pkg/api/v1"
)

func TestClientListTasksPaginatesAutomatically(t *testing.T) {
	// Generate 120 mock tasks across multiple pages
	totalTasks := 120
	allMockTasks := make([]apiv1.TaskResponse, totalTasks)
	for i := 0; i < totalTasks; i++ {
		allMockTasks[i] = apiv1.TaskResponse{
			ID:            fmt.Sprintf("task_%03d", i),
			UserID:        "user-1",
			Namespace:     "default",
			WorkspaceName: "ws-test",
			Prompt:        fmt.Sprintf("Task prompt %d", i),
			Status:        apiv1.TaskStatePending,
			CreatedAt:     time.Now().UTC(),
		}
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/tasks" {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query()
		limit, _ := strconv.Atoi(q.Get("limit"))
		if limit <= 0 {
			limit = 50
		}
		offset, _ := strconv.Atoi(q.Get("offset"))

		end := offset + limit
		if end > len(allMockTasks) {
			end = len(allMockTasks)
		}
		var page []apiv1.TaskResponse
		if offset < len(allMockTasks) {
			page = allMockTasks[offset:end]
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(apiv1.ListTasksResponse{
			Tasks: page,
		})
	}))
	defer server.Close()

	c, err := NewClient(Config{Endpoint: server.URL, Token: "test-token"})
	if err != nil {
		t.Fatalf("NewClient error: %v", err)
	}

	resp, err := c.ListTasks(context.Background(), "default", "ws-test", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(resp.Tasks) != totalTasks {
		t.Fatalf("expected %d tasks retrieved across pages, got %d", totalTasks, len(resp.Tasks))
	}

	if resp.Tasks[0].ID != "task_000" || resp.Tasks[totalTasks-1].ID != "task_119" {
		t.Fatalf("unexpected task order: first=%s, last=%s", resp.Tasks[0].ID, resp.Tasks[totalTasks-1].ID)
	}
}

func TestClientListTasksWithCustomLimitNormalizesPageSize(t *testing.T) {
	totalTasks := 120
	allMockTasks := make([]apiv1.TaskResponse, totalTasks)
	for i := 0; i < totalTasks; i++ {
		allMockTasks[i] = apiv1.TaskResponse{
			ID:            fmt.Sprintf("task_%03d", i),
			UserID:        "user-1",
			Namespace:     "default",
			WorkspaceName: "ws-test",
			Prompt:        fmt.Sprintf("Task prompt %d", i),
			Status:        apiv1.TaskStatePending,
			CreatedAt:     time.Now().UTC(),
		}
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		limit, _ := strconv.Atoi(q.Get("limit"))
		if limit <= 0 || limit > 100 {
			limit = 50
		}
		offset, _ := strconv.Atoi(q.Get("offset"))

		end := offset + limit
		if end > len(allMockTasks) {
			end = len(allMockTasks)
		}
		var page []apiv1.TaskResponse
		if offset < len(allMockTasks) {
			page = allMockTasks[offset:end]
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(apiv1.ListTasksResponse{
			Tasks: page,
		})
	}))
	defer server.Close()

	c, err := NewClient(Config{Endpoint: server.URL, Token: "test-token"})
	if err != nil {
		t.Fatalf("NewClient error: %v", err)
	}

	// Request with limit 200 (which exceeds server limit and is normalized)
	resp, err := c.ListAllTasks(context.Background(), TaskListOptions{
		Limit: 200,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(resp.Tasks) != totalTasks {
		t.Fatalf("expected all %d tasks retrieved despite requested limit 200, got %d", totalTasks, len(resp.Tasks))
	}
}
