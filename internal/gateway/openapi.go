package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
	apiv1 "github.com/hoangndst/buddy-sandboxes/pkg/api/v1"
)

// NewHumaConfig returns the canonical OpenAPI 3.1 configuration for Buddy Sandboxes.
func NewHumaConfig() huma.Config {
	cfg := huma.DefaultConfig("Buddy Sandboxes API", "v1.0.0")
	cfg.CreateHooks = nil
	cfg.OnAddOperation = append(cfg.OnAddOperation, func(oapi *huma.OpenAPI, op *huma.Operation) {
		if op.Responses == nil {
			op.Responses = make(map[string]*huma.Response)
		}
		op.Responses["default"] = &huma.Response{
			Description: "Error response",
			Content: map[string]*huma.MediaType{
				"text/plain": {
					Schema: &huma.Schema{
						Type:        "string",
						Description: "Error message",
					},
				},
			},
		}
		if oapi.Components != nil && oapi.Components.Schemas != nil {
			delete(oapi.Components.Schemas.Map(), "ErrorModel")
		}
	})
	cfg.OpenAPI.Info.Description = "Kubernetes-native isolated execution sandbox platform for AI coding agents."
	cfg.DocsPath = "/docs"
	cfg.OpenAPIPath = "/openapi"
	if cfg.OpenAPI.Components == nil {
		cfg.OpenAPI.Components = &huma.Components{}
	}
	cfg.OpenAPI.Components.SecuritySchemes = map[string]*huma.SecurityScheme{
		"BearerAuth": {
			Type:         "http",
			Scheme:       "bearer",
			BearerFormat: "Token",
			Description:  "Buddy user authentication token or service capability token",
		},
	}
	cfg.OpenAPI.Security = []map[string][]string{
		{"BearerAuth": {}},
	}
	return cfg
}

// Sandbox Inputs and Outputs for OpenAPI Documentation

type CreateSandboxInput struct {
	Body apiv1.CreateSandboxRequest
}

type CreateSandboxOutput struct {
	Body apiv1.SandboxResponse
}

type ListSandboxesInput struct{}

type ListSandboxesOutput struct {
	Body apiv1.ListSandboxesResponse
}

type GetSandboxInput struct {
	Name string `path:"name" doc:"Unique sandbox identifier" minLength:"1" maxLength:"63"`
}

type GetSandboxOutput struct {
	Body apiv1.SandboxResponse
}

type DeleteSandboxInput struct {
	Name string `path:"name" doc:"Unique sandbox identifier" minLength:"1" maxLength:"63"`
}

type DeleteSandboxOutput struct {
	Status int
}

type GetSandboxLogsInput struct {
	Name       string `path:"name" doc:"Unique sandbox identifier" minLength:"1" maxLength:"63"`
	Follow     bool   `query:"follow" doc:"Stream logs continuously"`
	TailLines  int64  `query:"tailLines" doc:"Number of lines to read from the end of the logs"`
	Timestamps bool   `query:"timestamps" doc:"Include RFC3339 timestamps in log output"`
	Container  string `query:"container" doc:"Target container to stream logs from (e.g. agent, git-init)"`
}

type GetSandboxLogsOutput struct {
	ContentType string `header:"Content-Type" example:"text/plain; charset=utf-8"`
	Body        []byte
}

type SuspendSandboxInput struct {
	Name string `path:"name" doc:"Unique sandbox identifier" minLength:"1" maxLength:"63"`
}

type SuspendSandboxOutput struct {
	Body apiv1.SandboxResponse
}

type ResumeSandboxInput struct {
	Name string `path:"name" doc:"Unique sandbox identifier" minLength:"1" maxLength:"63"`
}

type ResumeSandboxOutput struct {
	Body apiv1.SandboxResponse
}

type UploadSandboxWorkspaceResponse struct {
	Status string `json:"status" doc:"Upload operation status" example:"uploaded"`
	Path   string `json:"path" doc:"Destination filesystem path in sandbox" example:"/workspace"`
}

type UploadSandboxWorkspaceInput struct {
	Name      string `path:"name" doc:"Unique sandbox identifier" minLength:"1" maxLength:"63"`
	Path      string `query:"path" doc:"Target directory inside sandbox workspace"`
	File      string `query:"file" doc:"Filename when uploading a single file"`
	Directory string `query:"directory" doc:"Directory name when uploading a directory"`
	RawBody   []byte
}

type UploadSandboxWorkspaceOutput struct {
	Body UploadSandboxWorkspaceResponse
}

type DownloadSandboxWorkspaceInput struct {
	Name          string `path:"name" doc:"Unique sandbox identifier" minLength:"1" maxLength:"63"`
	Path          string `query:"path" doc:"Path within workspace to archive and download"`
	WorkspaceRoot bool   `query:"workspaceRoot" doc:"Archive entire workspace directory (/workspace)"`
}

type DownloadSandboxWorkspaceOutput struct {
	ContentType string `header:"Content-Type" example:"application/x-tar"`
	Body        []byte
}

type ExecSandboxInput struct {
	Name string   `path:"name" doc:"Unique sandbox identifier" minLength:"1" maxLength:"63"`
	Cmd  []string `query:"cmd" doc:"Command and arguments to execute"`
	TTY  bool     `query:"tty" doc:"Allocate pseudo-TTY"`
}

type PortForwardSandboxInput struct {
	Name string `path:"name" doc:"Unique sandbox identifier" minLength:"1" maxLength:"63"`
	Port int    `query:"port" required:"true" doc:"Target TCP port inside sandbox pod"`
}

// Session Inputs and Outputs for OpenAPI Documentation

type CreateSessionInput struct {
	SandboxName string `path:"name" doc:"Parent sandbox identifier"`
	Body        apiv1.CreateSessionRequest
}

type CreateSessionOutput struct {
	Body apiv1.SessionResponse
}

type ListSessionsInput struct {
	SandboxName string `path:"name" doc:"Parent sandbox identifier"`
}

type ListSessionsOutput struct {
	Body apiv1.ListSessionsResponse
}

type GetSessionInput struct {
	SandboxName string `path:"name" doc:"Parent sandbox identifier"`
	SessionID   string `path:"sessionID" doc:"Unique session UUID"`
}

type GetSessionOutput struct {
	Body apiv1.SessionResponse
}

type DeleteSessionInput struct {
	SandboxName string `path:"name" doc:"Parent sandbox identifier"`
	SessionID   string `path:"sessionID" doc:"Unique session UUID"`
}

type DeleteSessionOutput struct {
	Body struct {
		Status string `json:"status" doc:"Session termination status" example:"terminated"`
	}
}

type StreamSessionInput struct {
	SandboxName string `path:"name" doc:"Parent sandbox identifier"`
	SessionID   string `path:"sessionID" doc:"Unique session UUID"`
}

// Task Inputs and Outputs for OpenAPI Documentation

type CreateTaskInput struct {
	IdempotencyKey string `header:"Idempotency-Key" required:"true" maxLength:"128" doc:"Unique idempotency key for task submission"`
	Body           apiv1.CreateTaskRequest
}

type CreateTaskOutput struct {
	Body apiv1.TaskResponse
}

type ListTasksInput struct {
	Status string `query:"status" doc:"Filter tasks by lifecycle status (pending, running, succeeded, failed, cancelled)"`
	Limit  int    `query:"limit" doc:"Maximum number of tasks to return"`
}

type ListTasksOutput struct {
	Body apiv1.ListTasksResponse
}

type GetTaskInput struct {
	ID string `path:"id" doc:"Unique task identifier"`
}

type GetTaskOutput struct {
	Body apiv1.TaskResponse
}

type CancelTaskInput struct {
	ID string `path:"id" doc:"Unique task identifier"`
}

type CancelTaskOutput struct {
	Body struct {
		ID     string `json:"id" doc:"Cancelled task identifier"`
		Status string `json:"status" doc:"Cancellation status (cancelled)" example:"cancelled"`
	}
}

type ListExecutionsInput struct {
	TaskID string `path:"id" doc:"Parent task identifier"`
}

type ListExecutionsOutput struct {
	Body apiv1.ListExecutionsResponse
}

type GetExecutionInput struct {
	ID string `path:"id" doc:"Unique execution identifier"`
}

type GetExecutionOutput struct {
	Body apiv1.ExecutionResponse
}

type GetExecutionLogsInput struct {
	ID string `path:"id" doc:"Unique execution identifier"`
}

type GetExecutionLogsOutput struct {
	ContentType string `header:"Content-Type" example:"text/plain; charset=utf-8"`
	Body        []byte
}

type CreateAgentProfileInput struct {
	Body apiv1.CreateAgentProfileRequest
}

type CreateAgentProfileOutput struct {
	Body apiv1.AgentProfileResponse
}

type ListAgentProfilesInput struct{}

type ListAgentProfilesOutput struct {
	Body apiv1.ListAgentProfilesResponse
}

type GetAgentProfileInput struct {
	ID string `path:"id" doc:"Unique agent harness profile identifier" minLength:"1"`
}

type GetAgentProfileOutput struct {
	Body apiv1.AgentProfileResponse
}

// Identity and Credential Inputs and Outputs

type WhoAmIInput struct{}

type WhoAmIOutput struct {
	Body apiv1.WhoAmIResponse
}

type ListUsersInput struct{}

type ListUsersOutput struct {
	Body struct {
		Items []apiv1.User `json:"items" doc:"List of registered users"`
	}
}

type CreateUserInput struct {
	Body struct {
		DisplayName string `json:"displayName" doc:"User display name" minLength:"1" maxLength:"80"`
		Namespace   string `json:"namespace" doc:"Tenant Kubernetes namespace to allocate"`
	}
}

type CreateUserOutput struct {
	Body apiv1.User
}

type UpdateUserStatusInput struct {
	ID   string `path:"id" doc:"Target user identifier"`
	Body struct {
		Disabled bool `json:"disabled" doc:"Whether the user account is disabled"`
	}
}

type UpdateUserStatusOutput struct {
	Status int
}

type UpdateUserRoleInput struct {
	ID   string `path:"id" doc:"Target user identifier"`
	Body struct {
		Role string `json:"role" enum:"admin,user" doc:"Assigned user role"`
	}
}

type UpdateUserRoleOutput struct {
	Status int
}

type IssueUserTokenInput struct {
	ID string `path:"id" doc:"Target user identifier"`
}

type IssueUserTokenOutput struct {
	Body struct {
		ID        string    `json:"id" doc:"Token identifier"`
		Token     string    `json:"token" doc:"Bearer authentication token secret"`
		ExpiresAt time.Time `json:"expiresAt" doc:"Token expiration timestamp"`
	}
}

type RevokeUserTokenInput struct {
	ID      string `path:"id" doc:"Target user identifier"`
	TokenID string `path:"tokenID" doc:"Token identifier to revoke"`
}

type RevokeUserTokenOutput struct {
	Status int
}

type ListAdminUserSandboxesInput struct {
	ID string `path:"id" doc:"Target user identifier"`
}

type DeleteAdminUserSandboxInput struct {
	ID   string `path:"id" doc:"Target user identifier"`
	Name string `path:"name" doc:"Sandbox identifier"`
}

type ListCredentialsInput struct{}

type ListCredentialsOutput struct {
	Body struct {
		Items []apiv1.CredentialMetadata `json:"items" doc:"List of credential metadata records"`
	}
}

type SetCredentialInput struct {
	Service string `path:"service" doc:"Model provider service name (e.g. openai, anthropic)"`
	Sandbox string `query:"sandbox" doc:"Optional sandbox scope name"`
	Body    apiv1.SetCredentialRequest
}

type SetCredentialOutput struct {
	Status int
}

type DeleteCredentialInput struct {
	Service string `path:"service" doc:"Model provider service name"`
	Sandbox string `query:"sandbox" doc:"Optional sandbox scope name"`
}

type DeleteCredentialOutput struct {
	Status int
}

type SetGitCredentialInput struct {
	Repository string `query:"repository" required:"true" doc:"Git repository URL"`
	Sandbox    string `query:"sandbox" doc:"Optional sandbox scope name"`
	Body       apiv1.SetGitCredentialRequest
}

type SetGitCredentialOutput struct {
	Status int
}

type DeleteGitCredentialInput struct {
	Repository string `query:"repository" required:"true" doc:"Git repository URL"`
	Sandbox    string `query:"sandbox" doc:"Optional sandbox scope name"`
}

type DeleteGitCredentialOutput struct {
	Status int
}

// RegisterOpenAPIOperations populates the OpenAPI specification with all Buddy Sandboxes operations.
func RegisterOpenAPIOperations(api huma.API) {
	// --- Sandboxes ---
	huma.Register(api, huma.Operation{
		OperationID:   "createSandbox",
		Method:        http.MethodPost,
		Path:          "/api/v1/sandboxes",
		Summary:       "Create a sandbox",
		Description:   "Provisions an isolated execution sandbox pod managed by the Kubernetes controller.",
		DefaultStatus: http.StatusCreated,
		Tags:          []string{"Sandboxes"},
	}, func(ctx context.Context, input *CreateSandboxInput) (*CreateSandboxOutput, error) {
		return &CreateSandboxOutput{}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "listSandboxes",
		Method:        http.MethodGet,
		Path:          "/api/v1/sandboxes",
		Summary:       "List sandboxes",
		Description:   "Lists execution sandboxes within the caller's tenant namespace.",
		DefaultStatus: http.StatusOK,
		Tags:          []string{"Sandboxes"},
	}, func(ctx context.Context, input *ListSandboxesInput) (*ListSandboxesOutput, error) {
		return &ListSandboxesOutput{}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "getSandbox",
		Method:        http.MethodGet,
		Path:          "/api/v1/sandboxes/{name}",
		Summary:       "Inspect sandbox details",
		Description:   "Retrieves status conditions, runtime metadata, and networking for a specific sandbox.",
		DefaultStatus: http.StatusOK,
		Tags:          []string{"Sandboxes"},
	}, func(ctx context.Context, input *GetSandboxInput) (*GetSandboxOutput, error) {
		return &GetSandboxOutput{}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "deleteSandbox",
		Method:        http.MethodDelete,
		Path:          "/api/v1/sandboxes/{name}",
		Summary:       "Delete a sandbox",
		Description:   "Initiates cascading deletion of the sandbox custom resource and child pods.",
		DefaultStatus: http.StatusNoContent,
		Tags:          []string{"Sandboxes"},
	}, func(ctx context.Context, input *DeleteSandboxInput) (*DeleteSandboxOutput, error) {
		return &DeleteSandboxOutput{Status: http.StatusNoContent}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "getSandboxLogs",
		Method:        http.MethodGet,
		Path:          "/api/v1/sandboxes/{name}/logs",
		Summary:       "Retrieve sandbox logs",
		Description:   "Fetches standard output and standard error logs from the sandbox agent container.",
		DefaultStatus: http.StatusOK,
		Tags:          []string{"Sandboxes"},
		Responses: map[string]*huma.Response{
			"200": {
				Description: "Standard output and error stream logs",
				Content: map[string]*huma.MediaType{
					"text/plain": {
						Schema: &huma.Schema{Type: "string"},
					},
				},
			},
		},
	}, func(ctx context.Context, input *GetSandboxLogsInput) (*GetSandboxLogsOutput, error) {
		return &GetSandboxLogsOutput{ContentType: "text/plain; charset=utf-8"}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "suspendSandbox",
		Method:        http.MethodPost,
		Path:          "/api/v1/sandboxes/{name}/suspend",
		Summary:       "Suspend sandbox execution",
		Description:   "Terminates active compute pods while preserving persistent volume workspace data.",
		DefaultStatus: http.StatusAccepted,
		Tags:          []string{"Sandboxes"},
	}, func(ctx context.Context, input *SuspendSandboxInput) (*SuspendSandboxOutput, error) {
		return &SuspendSandboxOutput{}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "resumeSandbox",
		Method:        http.MethodPost,
		Path:          "/api/v1/sandboxes/{name}/resume",
		Summary:       "Resume suspended sandbox",
		Description:   "Re-schedules compute pods for a previously suspended persistent workspace.",
		DefaultStatus: http.StatusAccepted,
		Tags:          []string{"Sandboxes"},
	}, func(ctx context.Context, input *ResumeSandboxInput) (*ResumeSandboxOutput, error) {
		return &ResumeSandboxOutput{}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "uploadSandboxWorkspace",
		Method:        http.MethodPost,
		Path:          "/api/v1/sandboxes/{name}/upload",
		Summary:       "Upload files to sandbox workspace",
		Description:   "Extracts an uploaded tar archive stream into the sandbox workspace directory.",
		DefaultStatus: http.StatusOK,
		Tags:          []string{"Sandboxes"},
		RequestBody: &huma.RequestBody{
			Description: "Tar archive stream containing workspace files",
			Content: map[string]*huma.MediaType{
				"application/x-tar": {
					Schema: &huma.Schema{Type: "string", Format: "binary"},
				},
				"application/octet-stream": {
					Schema: &huma.Schema{Type: "string", Format: "binary"},
				},
			},
			Required: true,
		},
	}, func(ctx context.Context, input *UploadSandboxWorkspaceInput) (*UploadSandboxWorkspaceOutput, error) {
		return &UploadSandboxWorkspaceOutput{Body: UploadSandboxWorkspaceResponse{Status: "uploaded", Path: "/workspace"}}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "downloadSandboxWorkspace",
		Method:        http.MethodGet,
		Path:          "/api/v1/sandboxes/{name}/download",
		Summary:       "Download files from sandbox workspace",
		Description:   "Streams a tar archive of the sandbox workspace directory.",
		DefaultStatus: http.StatusOK,
		Tags:          []string{"Sandboxes"},
		Responses: map[string]*huma.Response{
			"200": {
				Description: "Workspace directory tar archive stream",
				Content: map[string]*huma.MediaType{
					"application/x-tar": {
						Schema: &huma.Schema{Type: "string", Format: "binary"},
					},
				},
			},
		},
	}, func(ctx context.Context, input *DownloadSandboxWorkspaceInput) (*DownloadSandboxWorkspaceOutput, error) {
		return &DownloadSandboxWorkspaceOutput{ContentType: "application/x-tar"}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "execSandbox",
		Method:        http.MethodGet,
		Path:          "/api/v1/sandboxes/{name}/exec",
		Summary:       "Interactive terminal exec WebSocket",
		Description:   "Upgrades connection to WebSocket for interactive shell execution in the sandbox container.",
		DefaultStatus: http.StatusSwitchingProtocols,
		Tags:          []string{"Sandboxes"},
		Responses: map[string]*huma.Response{
			"101": {
				Description: "WebSocket connection upgraded for interactive terminal streaming",
			},
		},
	}, func(ctx context.Context, input *ExecSandboxInput) (*struct{}, error) {
		return &struct{}{}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "portForwardSandbox",
		Method:        http.MethodGet,
		Path:          "/api/v1/sandboxes/{name}/portforward",
		Summary:       "TCP port forward WebSocket",
		Description:   "Upgrades connection to WebSocket for binary TCP tunneling to target sandbox container port.",
		DefaultStatus: http.StatusSwitchingProtocols,
		Tags:          []string{"Sandboxes"},
		Responses: map[string]*huma.Response{
			"101": {
				Description: "WebSocket binary tunnel established",
			},
		},
	}, func(ctx context.Context, input *PortForwardSandboxInput) (*struct{}, error) {
		return &struct{}{}, nil
	})

	// --- Sessions ---
	huma.Register(api, huma.Operation{
		OperationID:   "createSession",
		Method:        http.MethodPost,
		Path:          "/api/v1/sandboxes/{name}/sessions",
		Summary:       "Create in-guest session",
		Description:   "Launches a command supervised by buddy-runner with PTY ring buffering.",
		DefaultStatus: http.StatusCreated,
		Tags:          []string{"Sessions"},
	}, func(ctx context.Context, input *CreateSessionInput) (*CreateSessionOutput, error) {
		return &CreateSessionOutput{}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "listSessions",
		Method:        http.MethodGet,
		Path:          "/api/v1/sandboxes/{name}/sessions",
		Summary:       "List sessions in sandbox",
		Description:   "Lists active and historical supervisor sessions for a given sandbox.",
		DefaultStatus: http.StatusOK,
		Tags:          []string{"Sessions"},
	}, func(ctx context.Context, input *ListSessionsInput) (*ListSessionsOutput, error) {
		return &ListSessionsOutput{}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "getSession",
		Method:        http.MethodGet,
		Path:          "/api/v1/sandboxes/{name}/sessions/{sessionID}",
		Summary:       "Inspect session details",
		Description:   "Retrieves state, exit code, and timestamps for an individual session.",
		DefaultStatus: http.StatusOK,
		Tags:          []string{"Sessions"},
	}, func(ctx context.Context, input *GetSessionInput) (*GetSessionOutput, error) {
		return &GetSessionOutput{}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "deleteSession",
		Method:        http.MethodDelete,
		Path:          "/api/v1/sandboxes/{name}/sessions/{sessionID}",
		Summary:       "Stop running session",
		Description:   "Sends termination signal to in-guest supervisor session.",
		DefaultStatus: http.StatusOK,
		Tags:          []string{"Sessions"},
	}, func(ctx context.Context, input *DeleteSessionInput) (*DeleteSessionOutput, error) {
		return &DeleteSessionOutput{}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "streamSession",
		Method:        http.MethodGet,
		Path:          "/api/v1/sandboxes/{name}/sessions/{sessionID}/stream",
		Summary:       "Stream live session terminal WebSocket",
		Description:   "Upgrades connection to WebSocket to stream live session terminal output and exit events.",
		DefaultStatus: http.StatusSwitchingProtocols,
		Tags:          []string{"Sessions"},
		Responses: map[string]*huma.Response{
			"101": {
				Description: "WebSocket live session stream established",
			},
		},
	}, func(ctx context.Context, input *StreamSessionInput) (*struct{}, error) {
		return &struct{}{}, nil
	})

	// --- Tasks & Executions ---
	huma.Register(api, huma.Operation{
		OperationID:   "createTask",
		Method:        http.MethodPost,
		Path:          "/api/v1/tasks",
		Summary:       "Submit autonomous task",
		Description:   "Submits a task for asynchronous execution against warm sandbox runner pools.",
		DefaultStatus: http.StatusCreated,
		Tags:          []string{"Tasks"},
		Responses: map[string]*huma.Response{
			"200": {
				Description: "Idempotent replay of previously submitted task",
				Content: map[string]*huma.MediaType{
					"application/json": {
						Schema: &huma.Schema{
							Ref: "#/components/schemas/TaskResponse",
						},
					},
				},
			},
			"201": {
				Description: "Task created successfully and queued for execution",
			},
		},
	}, func(ctx context.Context, input *CreateTaskInput) (*CreateTaskOutput, error) {
		return &CreateTaskOutput{}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "listTasks",
		Method:        http.MethodGet,
		Path:          "/api/v1/tasks",
		Summary:       "List autonomous tasks",
		Description:   "Queries tasks across status states within the caller namespace.",
		DefaultStatus: http.StatusOK,
		Tags:          []string{"Tasks"},
	}, func(ctx context.Context, input *ListTasksInput) (*ListTasksOutput, error) {
		return &ListTasksOutput{}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "getTask",
		Method:        http.MethodGet,
		Path:          "/api/v1/tasks/{id}",
		Summary:       "Inspect task details",
		Description:   "Retrieves task execution state, prompts, and active execution identifiers.",
		DefaultStatus: http.StatusOK,
		Tags:          []string{"Tasks"},
	}, func(ctx context.Context, input *GetTaskInput) (*GetTaskOutput, error) {
		return &GetTaskOutput{}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "deleteTask",
		Method:        http.MethodDelete,
		Path:          "/api/v1/tasks/{id}",
		Summary:       "Cancel autonomous task",
		Description:   "Cancels an active or pending task and releases claimed sandbox leases.",
		DefaultStatus: http.StatusOK,
		Tags:          []string{"Tasks"},
	}, func(ctx context.Context, input *CancelTaskInput) (*CancelTaskOutput, error) {
		return &CancelTaskOutput{}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "cancelTask",
		Method:        http.MethodPost,
		Path:          "/api/v1/tasks/{id}/cancel",
		Summary:       "Cancel pending or running task",
		Description:   "Transitions active task to cancelled status and releases claimed sandbox leases.",
		DefaultStatus: http.StatusOK,
		Tags:          []string{"Tasks"},
	}, func(ctx context.Context, input *CancelTaskInput) (*CancelTaskOutput, error) {
		return &CancelTaskOutput{}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "listExecutions",
		Method:        http.MethodGet,
		Path:          "/api/v1/tasks/{id}/executions",
		Summary:       "List execution attempts for task",
		Description:   "Lists all historical execution attempts, verification outcomes, and diff statistics for a task.",
		DefaultStatus: http.StatusOK,
		Tags:          []string{"Tasks"},
	}, func(ctx context.Context, input *ListExecutionsInput) (*ListExecutionsOutput, error) {
		return &ListExecutionsOutput{}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "getExecution",
		Method:        http.MethodGet,
		Path:          "/api/v1/executions/{id}",
		Summary:       "Inspect execution attempt",
		Description:   "Retrieves execution outcome, verification command results, and log buffers.",
		DefaultStatus: http.StatusOK,
		Tags:          []string{"Tasks"},
	}, func(ctx context.Context, input *GetExecutionInput) (*GetExecutionOutput, error) {
		return &GetExecutionOutput{}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "getExecutionLogs",
		Method:        http.MethodGet,
		Path:          "/api/v1/executions/{id}/logs",
		Summary:       "Retrieve execution logs",
		Description:   "Fetches text execution output logs for a task execution.",
		DefaultStatus: http.StatusOK,
		Tags:          []string{"Tasks"},
		Responses: map[string]*huma.Response{
			"200": {
				Description: "Execution logs text output",
				Content: map[string]*huma.MediaType{
					"text/plain": {
						Schema: &huma.Schema{Type: "string"},
					},
				},
			},
		},
	}, func(ctx context.Context, input *GetExecutionLogsInput) (*GetExecutionLogsOutput, error) {
		return &GetExecutionLogsOutput{ContentType: "text/plain; charset=utf-8"}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "createAgentProfile",
		Method:        http.MethodPost,
		Path:          "/api/v1/agent-profiles",
		Summary:       "Create agent harness profile (Admin)",
		Description:   "Registers a new agent harness configuration profile in the control plane.",
		DefaultStatus: http.StatusCreated,
		Tags:          []string{"AgentProfiles"},
	}, func(ctx context.Context, input *CreateAgentProfileInput) (*CreateAgentProfileOutput, error) {
		return &CreateAgentProfileOutput{}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "listAgentProfiles",
		Method:        http.MethodGet,
		Path:          "/api/v1/agent-profiles",
		Summary:       "List agent harness profiles",
		Description:   "Lists predefined agent harness profiles configured in the control plane.",
		DefaultStatus: http.StatusOK,
		Tags:          []string{"AgentProfiles"},
	}, func(ctx context.Context, input *ListAgentProfilesInput) (*ListAgentProfilesOutput, error) {
		return &ListAgentProfilesOutput{}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "getAgentProfile",
		Method:        http.MethodGet,
		Path:          "/api/v1/agent-profiles/{id}",
		Summary:       "Inspect agent profile",
		Description:   "Retrieves model routing, timeout budgets, and harness configuration for an agent profile by ID.",
		DefaultStatus: http.StatusOK,
		Tags:          []string{"AgentProfiles"},
	}, func(ctx context.Context, input *GetAgentProfileInput) (*GetAgentProfileOutput, error) {
		return &GetAgentProfileOutput{}, nil
	})

	// --- Identity & Credentials ---
	huma.Register(api, huma.Operation{
		OperationID:   "whoAmI",
		Method:        http.MethodGet,
		Path:          "/api/v1/auth/whoami",
		Summary:       "Check authenticated identity",
		Description:   "Returns the resolved principal name, tenant namespace, and role for the current caller.",
		DefaultStatus: http.StatusOK,
		Tags:          []string{"Identity"},
	}, func(ctx context.Context, input *WhoAmIInput) (*WhoAmIOutput, error) {
		return &WhoAmIOutput{}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "listUsers",
		Method:        http.MethodGet,
		Path:          "/api/v1/admin/users",
		Summary:       "List tenant users (Admin)",
		Description:   "Lists all registered users and assigned tenant namespaces across the cluster.",
		DefaultStatus: http.StatusOK,
		Tags:          []string{"Admin"},
	}, func(ctx context.Context, input *ListUsersInput) (*ListUsersOutput, error) {
		return &ListUsersOutput{}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "createUser",
		Method:        http.MethodPost,
		Path:          "/api/v1/admin/users",
		Summary:       "Create tenant user (Admin)",
		Description:   "Provisions a new tenant user and assigns an isolated Kubernetes namespace.",
		DefaultStatus: http.StatusCreated,
		Tags:          []string{"Admin"},
	}, func(ctx context.Context, input *CreateUserInput) (*CreateUserOutput, error) {
		return &CreateUserOutput{}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "updateUserStatus",
		Method:        http.MethodPatch,
		Path:          "/api/v1/admin/users/{id}",
		Summary:       "Update tenant user status (Admin)",
		Description:   "Enables or disables a tenant user account.",
		DefaultStatus: http.StatusNoContent,
		Tags:          []string{"Admin"},
	}, func(ctx context.Context, input *UpdateUserStatusInput) (*UpdateUserStatusOutput, error) {
		return &UpdateUserStatusOutput{Status: http.StatusNoContent}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "updateUserRole",
		Method:        http.MethodPatch,
		Path:          "/api/v1/admin/users/{id}/role",
		Summary:       "Update tenant user role (Admin)",
		Description:   "Assigns an administrative or standard user role to a tenant user.",
		DefaultStatus: http.StatusNoContent,
		Tags:          []string{"Admin"},
	}, func(ctx context.Context, input *UpdateUserRoleInput) (*UpdateUserRoleOutput, error) {
		return &UpdateUserRoleOutput{Status: http.StatusNoContent}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "issueUserToken",
		Method:        http.MethodPost,
		Path:          "/api/v1/admin/users/{id}/tokens",
		Summary:       "Issue user authentication token (Admin)",
		Description:   "Generates a new Bearer authentication token for a tenant user.",
		DefaultStatus: http.StatusCreated,
		Tags:          []string{"Admin"},
	}, func(ctx context.Context, input *IssueUserTokenInput) (*IssueUserTokenOutput, error) {
		return &IssueUserTokenOutput{}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "revokeUserToken",
		Method:        http.MethodDelete,
		Path:          "/api/v1/admin/users/{id}/tokens/{tokenID}",
		Summary:       "Revoke user authentication token (Admin)",
		Description:   "Revokes an active Bearer authentication token for a tenant user.",
		DefaultStatus: http.StatusNoContent,
		Tags:          []string{"Admin"},
	}, func(ctx context.Context, input *RevokeUserTokenInput) (*RevokeUserTokenOutput, error) {
		return &RevokeUserTokenOutput{Status: http.StatusNoContent}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "listAdminUserSandboxes",
		Method:        http.MethodGet,
		Path:          "/api/v1/admin/users/{id}/sandboxes",
		Summary:       "List tenant user sandboxes (Admin)",
		Description:   "Lists all sandboxes owned by a specific tenant user across their tenant namespace.",
		DefaultStatus: http.StatusOK,
		Tags:          []string{"Admin"},
	}, func(ctx context.Context, input *ListAdminUserSandboxesInput) (*ListSandboxesOutput, error) {
		return &ListSandboxesOutput{}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "deleteAdminUserSandbox",
		Method:        http.MethodDelete,
		Path:          "/api/v1/admin/users/{id}/sandboxes/{name}",
		Summary:       "Delete tenant user sandbox (Admin)",
		Description:   "Deletes an individual sandbox owned by a specific tenant user.",
		DefaultStatus: http.StatusNoContent,
		Tags:          []string{"Admin"},
	}, func(ctx context.Context, input *DeleteAdminUserSandboxInput) (*DeleteSandboxOutput, error) {
		return &DeleteSandboxOutput{Status: http.StatusNoContent}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "listCredentials",
		Method:        http.MethodGet,
		Path:          "/api/v1/credentials",
		Summary:       "List configured provider credentials",
		Description:   "Lists model provider API credentials configured for the caller.",
		DefaultStatus: http.StatusOK,
		Tags:          []string{"Credentials"},
	}, func(ctx context.Context, input *ListCredentialsInput) (*ListCredentialsOutput, error) {
		return &ListCredentialsOutput{}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "setCredential",
		Method:        http.MethodPut,
		Path:          "/api/v1/credentials/{service}",
		Summary:       "Configure model provider credential",
		Description:   "Stores or updates API key credentials and rate limit budgets for a model provider.",
		DefaultStatus: http.StatusNoContent,
		Tags:          []string{"Credentials"},
	}, func(ctx context.Context, input *SetCredentialInput) (*SetCredentialOutput, error) {
		return &SetCredentialOutput{Status: http.StatusNoContent}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "deleteCredential",
		Method:        http.MethodDelete,
		Path:          "/api/v1/credentials/{service}",
		Summary:       "Delete model provider credential",
		Description:   "Deletes stored credentials for a model provider at user or sandbox scope.",
		DefaultStatus: http.StatusNoContent,
		Tags:          []string{"Credentials"},
	}, func(ctx context.Context, input *DeleteCredentialInput) (*DeleteCredentialOutput, error) {
		return &DeleteCredentialOutput{Status: http.StatusNoContent}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "setGitCredentials",
		Method:        http.MethodPut,
		Path:          "/api/v1/git-credentials",
		Summary:       "Configure git repository credentials",
		Description:   "Configures an access token for authenticating git clone and push operations.",
		DefaultStatus: http.StatusNoContent,
		Tags:          []string{"Credentials"},
	}, func(ctx context.Context, input *SetGitCredentialInput) (*SetGitCredentialOutput, error) {
		return &SetGitCredentialOutput{Status: http.StatusNoContent}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "deleteGitCredentials",
		Method:        http.MethodDelete,
		Path:          "/api/v1/git-credentials",
		Summary:       "Delete git repository credentials",
		Description:   "Removes configured access tokens for a git repository.",
		DefaultStatus: http.StatusNoContent,
		Tags:          []string{"Credentials"},
	}, func(ctx context.Context, input *DeleteGitCredentialInput) (*DeleteGitCredentialOutput, error) {
		return &DeleteGitCredentialOutput{Status: http.StatusNoContent}, nil
	})
}

// GenerateOpenAPISpec exports the complete OpenAPI 3.1 specification as formatted JSON.
func GenerateOpenAPISpec() ([]byte, error) {
	r := chi.NewRouter()
	cfg := NewHumaConfig()
	api := humachi.New(r, cfg)
	RegisterOpenAPIOperations(api)
	return json.MarshalIndent(api.OpenAPI(), "", "  ")
}
