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
	Name                  string                `json:"name,omitempty"`
	CreationID            string                `json:"creationID,omitempty"`
	Image                 string                `json:"image"`
	Command               []string              `json:"command,omitempty"`
	Args                  []string              `json:"args,omitempty"`
	WorkingDir            string                `json:"workingDir,omitempty"`
	RuntimeClassName      *string               `json:"runtimeClassName,omitempty"`
	ShareProcessNamespace *bool                 `json:"shareProcessNamespace,omitempty"`
	MaxDurationSeconds    *int64                `json:"maxDurationSeconds,omitempty"`
	Resources             *ResourceRequirements `json:"resources,omitempty"`
	Env                   []EnvVar              `json:"env,omitempty"`
	Labels                map[string]string     `json:"labels,omitempty"`
	Workspace             *WorkspaceSpec        `json:"workspace,omitempty"`
	Network               *NetworkSpec          `json:"network,omitempty"`
	MCPServers            []MCPServerSpec       `json:"mcpServers,omitempty"`
}

// MCPServerSpec defines an MCP sidecar in the external REST API.
type MCPServerSpec struct {
	Name                   string                `json:"name"`
	Image                  string                `json:"image"`
	Port                   int32                 `json:"port"`
	Command                []string              `json:"command,omitempty"`
	Args                   []string              `json:"args,omitempty"`
	Env                    []EnvVar              `json:"env,omitempty"`
	Resources              *ResourceRequirements `json:"resources,omitempty"`
	MountWorkspace         bool                  `json:"mountWorkspace,omitempty"`
	MountWorkspaceReadOnly bool                  `json:"mountWorkspaceReadOnly,omitempty"`
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

// GitRepositorySpec defines repository seeding configuration.
type GitRepositorySpec struct {
	URL       string `json:"url"`
	Revision  string `json:"revision,omitempty"`
	Depth     int32  `json:"depth,omitempty"`
	Subpath   string `json:"subpath,omitempty"`
	SecretRef string `json:"secretRef,omitempty"`
}

// PersistentVolumeClaimWorkspace defines a pre-existing PVC to mount.
type PersistentVolumeClaimWorkspace struct {
	ClaimName string `json:"claimName"`
	ReadOnly  bool   `json:"readOnly,omitempty"`
}

// WorkspaceSpec configures sandbox storage.
type WorkspaceSpec struct {
	Ephemeral             bool                            `json:"ephemeral"`
	MountPath             string                          `json:"mountPath,omitempty"`
	Git                   *GitRepositorySpec              `json:"git,omitempty"`
	PersistentVolumeClaim *PersistentVolumeClaimWorkspace `json:"persistentVolumeClaim,omitempty"`
}

// WhoAmIResponse represents the authenticated caller identity and assigned namespace.
type WhoAmIResponse struct {
	User      string   `json:"user"`
	Namespace string   `json:"namespace"`
	Roles     []string `json:"roles,omitempty"`
	AuthMode  string   `json:"authMode"`
}

// RegisterCredentialSessionRequest installs a short-lived provider credential for one Sandbox.
type RegisterCredentialSessionRequest struct {
	SandboxName        string `json:"sandboxName"`
	SessionToken       string `json:"sessionToken"`
	CreationID         string `json:"creationID"`
	OpenAIKey          string `json:"openAIKey"`
	MaxLifetimeSeconds int64  `json:"maxLifetimeSeconds,omitempty"`
}

// RegisterCredentialSessionResponse returns the registered Sandbox identity.
type RegisterCredentialSessionResponse struct {
	SandboxName string `json:"sandboxName"`
}

// NetworkSpec configures sandbox network boundaries.
type NetworkSpec struct {
	AllowAPIBroker       bool     `json:"allowAPIBroker,omitempty"`
	AllowInternetWeb     bool     `json:"allowInternetWeb"`
	AllowDNS             bool     `json:"allowDNS"`
	BlockClusterInternal bool     `json:"blockClusterInternal"`
	BlockMetadataService bool     `json:"blockMetadataService"`
	AllowedDomains       []string `json:"allowedDomains,omitempty"`
}

// SandboxResponse represents the returned metadata and status of a sandbox.
type SandboxResponse struct {
	ID                    string            `json:"id"`
	CreationID            string            `json:"creationID,omitempty"`
	Name                  string            `json:"name"`
	Namespace             string            `json:"namespace"`
	Image                 string            `json:"image"`
	RuntimeClassName      *string           `json:"runtimeClassName,omitempty"`
	ShareProcessNamespace bool              `json:"shareProcessNamespace"`
	Phase                 string            `json:"phase"`
	Suspend               bool              `json:"suspend"`
	SuspensionPhase       string            `json:"suspensionPhase"`
	PodName               string            `json:"podName,omitempty"`
	PodUID                string            `json:"podUID,omitempty"`
	PodIP                 string            `json:"podIP,omitempty"`
	Labels                map[string]string `json:"labels,omitempty"`
	Workspace             *WorkspaceSpec    `json:"workspace,omitempty"`
	StartTime             *time.Time        `json:"startTime,omitempty"`
	CompletionTime        *time.Time        `json:"completionTime,omitempty"`
	ExitCode              *int32            `json:"exitCode,omitempty"`
	MCPServers            []MCPServerSpec   `json:"mcpServers,omitempty"`
	Network               *NetworkSpec      `json:"network,omitempty"`
	CreatedAt             time.Time         `json:"createdAt"`
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
