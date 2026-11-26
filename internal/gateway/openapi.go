package gateway

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
	apiv1 "github.com/hoangndst/buddy-sandboxes/pkg/api/v1"
)

// NewHumaConfig returns the canonical OpenAPI 3.1 configuration for Buddy Sandboxes.
func NewHumaConfig() huma.Config {
	cfg := huma.DefaultConfig("Buddy Sandboxes API", "v1.0.0")
	cfg.OpenAPI.Info.Description = "Kubernetes-native isolated execution sandbox platform for AI coding agents."
	cfg.DocsPath = "/docs"
	cfg.OpenAPIPath = "/openapi"
	cfg.OpenAPI.Components = &huma.Components{
		SecuritySchemes: map[string]*huma.SecurityScheme{
			"BearerAuth": {
				Type:         "http",
				Scheme:       "bearer",
				BearerFormat: "Token",
				Description:  "Buddy user authentication token or service capability token",
			},
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

type ListSandboxesInput struct {
	Status        string `query:"status" doc:"Filter sandboxes by status phase (Pending, Running, Succeeded, Failed, Suspended)"`
	LabelSelector string `query:"labelSelector" doc:"Kubernetes label selector expression"`
}

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
	Name      string `path:"name" doc:"Unique sandbox identifier" minLength:"1" maxLength:"63"`
	Follow    bool   `query:"follow" doc:"Stream logs continuously"`
	TailLines int64  `query:"tailLines" doc:"Number of lines to read from the end of the logs"`
}

type GetSandboxLogsOutput struct {
	ContentType string `header:"Content-Type" example:"text/plain; charset=utf-8"`
	Body        []byte
}

type SuspendSandboxInput struct {
	Name string `path:"name" doc:"Unique sandbox identifier" minLength:"1" maxLength:"63"`
}

type SuspendSandboxOutput struct {
	Status int
}

type ResumeSandboxInput struct {
	Name string `path:"name" doc:"Unique sandbox identifier" minLength:"1" maxLength:"63"`
}

type ResumeSandboxOutput struct {
	Status int
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
	Status int
}

// Task Inputs and Outputs for OpenAPI Documentation

type CreateTaskInput struct {
	Body apiv1.CreateTaskRequest
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
	Body apiv1.TaskResponse
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

type ListAgentProfilesInput struct {
	Limit int `query:"limit" doc:"Maximum number of agent profiles to return"`
}

type ListAgentProfilesOutput struct {
	Body apiv1.ListAgentProfilesResponse
}

type GetAgentProfileInput struct {
	Name string `path:"name" doc:"Agent harness profile name"`
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
		DisplayName string `json:"displayName" doc:"User display name"`
		Namespace   string `json:"namespace" doc:"Tenant Kubernetes namespace to allocate"`
	}
}

type CreateUserOutput struct {
	Body apiv1.User
}

type ListCredentialsInput struct{}

type ListCredentialsOutput struct {
	Body struct {
		Items []apiv1.CredentialMetadata `json:"items" doc:"List of credential metadata records"`
	}
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
		DefaultStatus: http.StatusOK,
		Tags:          []string{"Sandboxes"},
	}, func(ctx context.Context, input *SuspendSandboxInput) (*SuspendSandboxOutput, error) {
		return &SuspendSandboxOutput{Status: http.StatusOK}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "resumeSandbox",
		Method:        http.MethodPost,
		Path:          "/api/v1/sandboxes/{name}/resume",
		Summary:       "Resume suspended sandbox",
		Description:   "Re-schedules compute pods for a previously suspended persistent workspace.",
		DefaultStatus: http.StatusOK,
		Tags:          []string{"Sandboxes"},
	}, func(ctx context.Context, input *ResumeSandboxInput) (*ResumeSandboxOutput, error) {
		return &ResumeSandboxOutput{Status: http.StatusOK}, nil
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
		DefaultStatus: http.StatusNoContent,
		Tags:          []string{"Sessions"},
	}, func(ctx context.Context, input *DeleteSessionInput) (*DeleteSessionOutput, error) {
		return &DeleteSessionOutput{Status: http.StatusNoContent}, nil
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
		Path:          "/api/v1/agent-profiles/{name}",
		Summary:       "Inspect agent profile",
		Description:   "Retrieves model routing, timeout budgets, and harness configuration for a named profile.",
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
		Description:   "Provisions a new tenant user and generates an initial Buddy authentication token.",
		DefaultStatus: http.StatusCreated,
		Tags:          []string{"Admin"},
	}, func(ctx context.Context, input *CreateUserInput) (*CreateUserOutput, error) {
		return &CreateUserOutput{}, nil
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
}

// GenerateOpenAPISpec exports the complete OpenAPI 3.1 specification as formatted JSON.
func GenerateOpenAPISpec() ([]byte, error) {
	r := chi.NewRouter()
	cfg := NewHumaConfig()
	api := humachi.New(r, cfg)
	RegisterOpenAPIOperations(api)
	return json.MarshalIndent(api.OpenAPI(), "", "  ")
}
