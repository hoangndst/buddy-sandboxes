package v1

import (
	"time"
)

// Sub-channel multiplexing constants for WebSocket streams.
const (
	ChannelStdin  byte = 0
	ChannelStdout byte = 1
	ChannelStderr byte = 2
	ChannelExit   byte = 3
	ChannelResize byte = 4
)

// CreateSandboxRequest defines the input payload to provision a sandbox.
type CreateSandboxRequest struct {
	Name               string                `json:"name,omitempty"`
	Image              string                `json:"image"`
	Command            []string              `json:"command,omitempty"`
	Args               []string              `json:"args,omitempty"`
	WorkingDir         string                `json:"workingDir,omitempty"`
	RuntimeClassName   *string               `json:"runtimeClassName,omitempty"`
	MaxDurationSeconds *int64                `json:"maxDurationSeconds,omitempty"`
	Resources          *ResourceRequirements `json:"resources,omitempty"`
	Env                []EnvVar              `json:"env,omitempty"`
	Labels             map[string]string     `json:"labels,omitempty"`
	Workspace          *WorkspaceSpec        `json:"workspace,omitempty"`
	Network            *NetworkSpec          `json:"network,omitempty"`
}

// ResourceRequirements describes resource limits and requests.
type ResourceRequirements struct {
	Limits   map[string]string `json:"limits,omitempty"`
	Requests map[string]string `json:"requests,omitempty"`
}

// EnvVar represents an environment variable inside the container.
type EnvVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// WorkspaceSpec configures sandbox storage.
type WorkspaceSpec struct {
	Ephemeral bool   `json:"ephemeral"`
	MountPath string `json:"mountPath,omitempty"`
}

// NetworkSpec configures sandbox network boundaries.
type NetworkSpec struct {
	AllowInternetWeb     bool `json:"allowInternetWeb"`
	AllowDNS             bool `json:"allowDNS"`
	BlockClusterInternal bool `json:"blockClusterInternal"`
	BlockMetadataService bool `json:"blockMetadataService"`
}

// SandboxResponse represents the returned metadata and status of a sandbox.
type SandboxResponse struct {
	ID               string            `json:"id"`
	Name             string            `json:"name"`
	Namespace        string            `json:"namespace"`
	Image            string            `json:"image"`
	RuntimeClassName *string           `json:"runtimeClassName,omitempty"`
	Phase            string            `json:"phase"`
	PodName          string            `json:"podName,omitempty"`
	PodIP            string            `json:"podIP,omitempty"`
	Labels           map[string]string `json:"labels,omitempty"`
	StartTime        *time.Time        `json:"startTime,omitempty"`
	CompletionTime   *time.Time        `json:"completionTime,omitempty"`
	ExitCode         *int32            `json:"exitCode,omitempty"`
	CreatedAt        time.Time         `json:"createdAt"`
}

// ListSandboxesResponse represents a paginated list of sandboxes.
type ListSandboxesResponse struct {
	Items []SandboxResponse `json:"items"`
	Total int               `json:"total"`
}

// ResizeMessage communicates terminal dimension changes across Channel 4.
type ResizeMessage struct {
	Cols   int    `json:"cols,omitempty"`
	Rows   int    `json:"rows,omitempty"`
	Width  uint16 `json:"width,omitempty"`
	Height uint16 `json:"height,omitempty"`
}

// ExitMessage communicates process exit status across Channel 3.
type ExitMessage struct {
	ExitCode int    `json:"exitCode"`
	Error    string `json:"error,omitempty"`
}
