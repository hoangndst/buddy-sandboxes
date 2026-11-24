package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	apiv1 "github.com/hoangndst/buddy-sandboxes/pkg/api/v1"
	"github.com/hoangndst/buddy-sandboxes/pkg/client"
)

func TestCLISessionListAndInspect(t *testing.T) {
	mockSession := apiv1.SessionResponse{
		ID:          "sess_cli_test",
		Namespace:   "tenant",
		SandboxName: "test-sbx",
		Command:     []string{"echo", "cli"},
		State:       apiv1.SessionStateCompleted,
		CreatedAt:   time.Now().UTC(),
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/sandboxes/test-sbx/sessions":
			_ = json.NewEncoder(w).Encode(apiv1.ListSessionsResponse{
				Sessions: []apiv1.SessionResponse{mockSession},
			})
		case "/api/v1/sandboxes/test-sbx/sessions/sess_cli_test":
			_ = json.NewEncoder(w).Encode(mockSession)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	c, err := client.NewClient(client.Config{Endpoint: server.URL})
	if err != nil {
		t.Fatalf("failed to initialize client: %v", err)
	}

	listResp, err := c.ListSessions(context.Background(), "test-sbx")
	if err != nil {
		t.Fatalf("ListSessions error: %v", err)
	}
	if len(listResp.Sessions) != 1 || listResp.Sessions[0].ID != "sess_cli_test" {
		t.Fatalf("unexpected list response: %+v", listResp)
	}

	getResp, err := c.GetSession(context.Background(), "test-sbx", "sess_cli_test")
	if err != nil {
		t.Fatalf("GetSession error: %v", err)
	}
	if getResp.ID != "sess_cli_test" || getResp.State != apiv1.SessionStateCompleted {
		t.Fatalf("unexpected get response: %+v", getResp)
	}
}
