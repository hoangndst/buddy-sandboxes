package sbx

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/hoangndst/buddy-sandboxes/pkg/api/v1"
	"github.com/hoangndst/buddy-sandboxes/pkg/client"
)

func TestSuspensionOperationComplete(t *testing.T) {
	if !suspensionOperationComplete(&apiv1.SandboxResponse{SuspensionPhase: "Suspended", Phase: "Running"}, true) {
		t.Fatal("suspend should complete at Suspended")
	}
	if !suspensionOperationComplete(&apiv1.SandboxResponse{SuspensionPhase: "None", Phase: "Running"}, false) {
		t.Fatal("resume should complete at active Running")
	}
	if suspensionOperationComplete(&apiv1.SandboxResponse{SuspensionPhase: "None", Phase: "Pending"}, false) {
		t.Fatal("resume must wait until execution is Running")
	}
}

func TestWaitForSuspensionTimeoutKeepsDesiredState(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(apiv1.SandboxResponse{
			ID: "workspace", Phase: "Running", Suspend: true,
			SuspensionPhase: "Suspending", PodName: "workspace-sandbox",
		})
	}))
	defer server.Close()
	c, err := client.NewClient(client.Config{Endpoint: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	var stdout, stderr bytes.Buffer
	err = waitForSuspension(ctx, c, "workspace", "suspend", true, &apiv1.SandboxResponse{Suspend: true}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "desired state remains recorded") {
		t.Fatalf("expected timeout that preserves desired state, got %v", err)
	}
	if !strings.Contains(stderr.String(), "suspensionPhase=Suspending") {
		t.Fatalf("expected latest status in stderr, got %q", stderr.String())
	}
}

func TestWaitForSuspensionRejectsTerminalPhase(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(apiv1.SandboxResponse{ID: "workspace", Phase: "Failed", SuspensionPhase: "Suspended"})
	}))
	defer server.Close()
	c, err := client.NewClient(client.Config{Endpoint: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err = waitForSuspension(ctx, c, "workspace", "resume", false, nil, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "terminal phase") {
		t.Fatalf("expected terminal error, got %v", err)
	}
}
