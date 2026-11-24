package credentials

import (
	"context"
	"slices"
	"time"
)

// ServiceKind selects a service's typed credential policy and delivery path.
type ServiceKind string

const (
	ServiceOpenAI                        = "openai"
	ServiceAnthropic                     = "anthropic"
	ServiceGitHub                        = "github"
	ServiceKindProvider      ServiceKind = "provider"
	ServiceKindGitRepository ServiceKind = "git-repository"
)

// ServiceDescriptor is the closed public catalog entry for one built-in service.
type ServiceDescriptor struct {
	ID                     string
	DisplayName            string
	Kind                   ServiceKind
	ValueEnvironment       string
	BaseURLEnvironment     string
	ModelEnvironment       string
	HostBaseURLEnvironment string
	HostModelEnvironment   string
	BrokerRoute            string
	ClientBaseURL          string
	RequiresRepository     bool
	SupportsSandboxScope   bool
	SupportsCustomEndpoint bool
	SupportsDefaultModel   bool
	SupportsAuthType       bool
}

// ProviderCredentialPolicy exposes provider credential selection to platform use cases.
type ProviderCredentialPolicy interface {
	HasForSandbox(context.Context, string, string, string) (bool, error)
	GetRecordForSandbox(context.Context, string, string, string) (Record, error)
}

// ProviderCapabilityLifecycle exposes broker capability transitions to platform use cases.
type ProviderCapabilityLifecycle interface {
	IssuePending(context.Context, string, string, string, string, string, time.Time, []string) (string, []byte, error)
	Activate(context.Context, []byte, string) error
	Revoke(context.Context, []byte) error
	RevokeSandbox(context.Context, string, string, string) error
}

var serviceCatalog = map[string]ServiceDescriptor{
	ServiceOpenAI: {
		ID: ServiceOpenAI, DisplayName: "OpenAI", Kind: ServiceKindProvider,
		ValueEnvironment: "OPENAI_API_KEY", BaseURLEnvironment: "OPENAI_BASE_URL", ModelEnvironment: "OPENAI_MODEL",
		HostBaseURLEnvironment: "OPENAI_BASE_URL", HostModelEnvironment: "OPENAI_MODEL",
		BrokerRoute:          "POST /api/v1/provider/openai/v1/responses",
		ClientBaseURL:        "http://buddy-api.buddy-sandboxes-system.svc:8080/api/v1/provider/openai/v1",
		SupportsSandboxScope: true, SupportsCustomEndpoint: true, SupportsDefaultModel: true, SupportsAuthType: true,
	},
	ServiceAnthropic: {
		ID: ServiceAnthropic, DisplayName: "Anthropic", Kind: ServiceKindProvider,
		ValueEnvironment: "ANTHROPIC_API_KEY", BaseURLEnvironment: "ANTHROPIC_BASE_URL",
		BrokerRoute:          "POST /api/v1/provider/anthropic/v1/messages",
		ClientBaseURL:        "http://buddy-api.buddy-sandboxes-system.svc:8080/api/v1/provider/anthropic",
		SupportsSandboxScope: true, SupportsAuthType: true,
	},
	ServiceGitHub: {
		ID: ServiceGitHub, DisplayName: "GitHub", Kind: ServiceKindGitRepository,
		RequiresRepository: true, SupportsSandboxScope: true,
	},
}

func init() {
	for id, service := range serviceCatalog {
		if service.ID != id || service.DisplayName == "" || !service.SupportsSandboxScope {
			panic("invalid built-in credential service descriptor: " + id)
		}
		if service.Kind == ServiceKindProvider && (service.BrokerRoute == "" || service.ClientBaseURL == "" || service.ValueEnvironment == "") {
			panic("provider service is missing broker configuration: " + id)
		}
		if service.Kind == ServiceKindGitRepository && !service.RequiresRepository {
			panic("Git service is missing repository scope: " + id)
		}
	}
}

// LookupService returns a descriptor only for a service implemented by this binary.
func LookupService(id string) (ServiceDescriptor, bool) {
	service, ok := serviceCatalog[id]
	return service, ok
}

// RegisteredServices returns the built-in service catalog in stable order.
func RegisteredServices() []ServiceDescriptor {
	ids := make([]string, 0, len(serviceCatalog))
	for id := range serviceCatalog {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	services := make([]ServiceDescriptor, 0, len(ids))
	for _, id := range ids {
		services = append(services, serviceCatalog[id])
	}
	return services
}

// ServiceIDs returns the supported identifiers for actionable validation errors.
func ServiceIDs() []string {
	services := RegisteredServices()
	ids := make([]string, 0, len(services))
	for _, service := range services {
		ids = append(ids, service.ID)
	}
	return ids
}

// IsProviderService reports whether the catalog routes a service through model-provider brokerage.
func IsProviderService(id string) bool {
	service, ok := LookupService(id)
	return ok && service.Kind == ServiceKindProvider
}

// IsGitRepositoryService reports whether the catalog routes a service through repository clone policy.
func IsGitRepositoryService(id string) bool {
	service, ok := LookupService(id)
	return ok && service.Kind == ServiceKindGitRepository
}
