package gateway

import (
	"context"
	"fmt"
	"net/http"
	"time"

	sandboxesv1alpha1 "github.com/hoangndst/buddy-sandboxes/api/v1alpha1"
	"github.com/hoangndst/buddy-sandboxes/internal/credentials"
	corev1 "k8s.io/api/core/v1"
)

// ProviderBindingError wraps an error with an HTTP status code for gateway handlers.
type ProviderBindingError struct {
	StatusCode int
	Message    string
	Err        error
}

func (e *ProviderBindingError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %v", e.Message, e.Err)
	}
	return e.Message
}

func (e *ProviderBindingError) Unwrap() error {
	return e.Err
}

// ProviderBindingParams contains the parameters needed to bind a provider credential and broker capability to a sandbox.
type ProviderBindingParams struct {
	UserID             string
	Namespace          string
	SandboxName        string
	CreationID         string
	Provider           string
	Model              string
	MaxDurationSeconds *int64
	ExpiresAt          *time.Time
}

// BuildProviderBinding validates provider credentials, configures environment variables,
// enables API broker networking, and issues a pending capability token for the sandbox.
// It returns the capability hash if a capability was issued, or nil if no provider was requested.
func BuildProviderBinding(
	ctx context.Context,
	providerCreds credentials.ProviderCredentialPolicy,
	brokerCaps credentials.ProviderCapabilityLifecycle,
	params ProviderBindingParams,
	sbx *sandboxesv1alpha1.Sandbox,
) ([]byte, error) {
	if params.Provider == "" {
		return nil, nil
	}
	if providerCreds == nil || brokerCaps == nil {
		return nil, &ProviderBindingError{
			StatusCode: http.StatusServiceUnavailable,
			Message:    "Provider credential store is unavailable",
		}
	}

	providerService, providerSupported := credentials.LookupService(params.Provider)
	if !providerSupported || providerService.Kind != credentials.ServiceKindProvider {
		return nil, &ProviderBindingError{
			StatusCode: http.StatusBadRequest,
			Message:    fmt.Sprintf("Unsupported provider %q", params.Provider),
		}
	}

	exists, err := providerCreds.HasForSandbox(ctx, params.UserID, params.Provider, params.SandboxName)
	if err != nil {
		return nil, &ProviderBindingError{
			StatusCode: http.StatusServiceUnavailable,
			Message:    "Provider credential store is unavailable",
			Err:        err,
		}
	}
	if !exists {
		return nil, &ProviderBindingError{
			StatusCode: http.StatusConflict,
			Message:    "Provider credential is not configured; run buddy-sbx secret set first",
		}
	}

	expiresAt := time.Now().Add(2 * time.Hour)
	if params.ExpiresAt != nil && !params.ExpiresAt.IsZero() {
		expiresAt = *params.ExpiresAt
	} else if params.MaxDurationSeconds != nil && *params.MaxDurationSeconds > 0 {
		dur := time.Duration(*params.MaxDurationSeconds) * time.Second
		if dur < 2*time.Hour {
			expiresAt = time.Now().Add(dur)
		}
	}

	routes := []string{providerService.BrokerRoute}
	if providerService.BaseURLEnvironment != "" {
		sbx.Spec.Env = append(sbx.Spec.Env, corev1.EnvVar{
			Name:  providerService.BaseURLEnvironment,
			Value: providerService.ClientBaseURL,
		})
	}

	if providerService.SupportsDefaultModel {
		effectiveModel := params.Model
		if effectiveModel == "" {
			if rec, err := providerCreds.GetRecordForSandbox(ctx, params.UserID, params.Provider, params.SandboxName); err == nil && rec.DefaultModel != "" {
				effectiveModel = rec.DefaultModel
			}
		}
		if effectiveModel != "" {
			if err := credentials.ValidateModelName(effectiveModel); err != nil {
				return nil, &ProviderBindingError{
					StatusCode: http.StatusBadRequest,
					Message:    err.Error(),
					Err:        err,
				}
			}
			sbx.Spec.Env = append(sbx.Spec.Env, corev1.EnvVar{
				Name:  providerService.ModelEnvironment,
				Value: effectiveModel,
			})
		}
	}

	token, hash, err := brokerCaps.IssuePending(ctx, params.UserID, params.Namespace, params.SandboxName, params.CreationID, params.Provider, expiresAt, routes)
	if err != nil {
		return nil, &ProviderBindingError{
			StatusCode: http.StatusServiceUnavailable,
			Message:    "Unable to create provider capability",
			Err:        err,
		}
	}

	sbx.Spec.Env = append(sbx.Spec.Env,
		corev1.EnvVar{Name: providerService.ValueEnvironment, Value: token},
		corev1.EnvVar{Name: "BUDDY_PROVIDER_MODE", Value: "broker"},
	)
	sbx.Spec.Network.AllowAPIBroker = true

	return hash, nil
}
