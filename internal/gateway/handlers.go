package gateway

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/go-chi/chi/v5"
	sandboxesv1alpha1 "github.com/hoangndst/buddy-sandboxes/api/v1alpha1"
	"github.com/hoangndst/buddy-sandboxes/internal/credentials"
	"github.com/hoangndst/buddy-sandboxes/internal/gitrepo"
	apiv1 "github.com/hoangndst/buddy-sandboxes/pkg/api/v1"
)

var dnsSubdomainRegex = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

const (
	sandboxCreationIDAnnotation       = "sandboxes.buddy.io/creation-id"
	gitCredentialRedemptionAnnotation = "sandboxes.buddy.io/git-credential-redemption"
	workspaceMountPath                = "/workspace"
	queryTrue                         = "true"
	agentLogContainer                 = "agent"
	gitInitLogContainer               = "git-init"
)

func validateSandboxName(name string) error {
	if len(name) < 1 || len(name) > 63 {
		return fmt.Errorf("sandbox name must be between 1 and 63 characters")
	}
	if !dnsSubdomainRegex.MatchString(name) {
		return fmt.Errorf("sandbox name must consist of lowercase alphanumeric characters or '-', and must start and end with an alphanumeric character (RFC 1123)")
	}
	return nil
}

func filterAndValidateLabels(userLabels map[string]string) (map[string]string, error) {
	out := map[string]string{
		"app.kubernetes.io/managed-by": "buddy-api",
		"buddy.io/created-at":          strconv.FormatInt(time.Now().Unix(), 10),
	}

	for k, v := range userLabels {
		lowerK := strings.ToLower(k)
		if strings.HasPrefix(lowerK, "buddy.io/") || strings.HasPrefix(lowerK, "kubernetes.io/") || strings.HasPrefix(lowerK, "k8s.io/") {
			return nil, fmt.Errorf("label key %q uses a reserved prefix (buddy.io/, kubernetes.io/, k8s.io/)", k)
		}
		out[k] = v
	}

	return out, nil
}

func generateRandomID(prefix string) string {
	bytes := make([]byte, 6)
	_, _ = rand.Read(bytes)
	return fmt.Sprintf("%s-%s", prefix, hex.EncodeToString(bytes))
}

func newCreationID() (string, error) {
	data := make([]byte, 16)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

// Handlers holds dependency references for REST route handlers.
type Handlers struct {
	client              client.Client
	kubeClient          kubernetes.Interface
	remoteExec          RemoteStreamExecutor
	providerCredentials credentials.ProviderCredentialPolicy
	brokerCapabilities  credentials.ProviderCapabilityLifecycle
	gitCredentials      credentials.GitSandboxCredentialService
}

func (h *Handlers) SetGitCredentials(store credentials.GitSandboxCredentialService) {
	h.gitCredentials = store
}

// NewHandlers creates a new Handlers instance.
func NewHandlers(c client.Client, k kubernetes.Interface, remoteExec RemoteStreamExecutor) *Handlers {
	return &Handlers{client: c, kubeClient: k, remoteExec: remoteExec}
}

func (h *Handlers) SetProviderServices(providerCredentials credentials.ProviderCredentialPolicy, brokerCapabilities credentials.ProviderCapabilityLifecycle) {
	h.providerCredentials = providerCredentials
	h.brokerCapabilities = brokerCapabilities
}

// CreateSandboxHandler handles POST /api/v1/sandboxes.
//
//nolint:gocyclo // This handler validates a single create request before translating it to the Kubernetes API.
func (h *Handlers) CreateSandboxHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ns, ok := TenantNamespaceFromContext(r.Context())
	if !ok || ns == "" {
		http.Error(w, "Tenant namespace not resolved", http.StatusUnauthorized)
		return
	}

	var req apiv1.CreateSandboxRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("Invalid request payload: %v", err), http.StatusBadRequest)
		return
	}

	if req.Image == "" {
		http.Error(w, "image is required", http.StatusBadRequest)
		return
	}
	providerService, providerSupported := credentials.LookupService(req.Provider)
	if req.Provider != "" && (!providerSupported || providerService.Kind != credentials.ServiceKindProvider) {
		http.Error(w, "Unsupported provider", http.StatusBadRequest)
		return
	}
	if req.Model != "" && req.Provider != "" && !providerService.SupportsDefaultModel {
		http.Error(w, "Selected provider does not support model overrides", http.StatusBadRequest)
		return
	}
	if req.Provider != "" && (h.providerCredentials == nil || h.brokerCapabilities == nil) {
		http.Error(w, "Provider broker is unavailable", http.StatusServiceUnavailable)
		return
	}
	var managedGitRepository *credentials.GitHubRepository
	if req.Workspace != nil && req.Workspace.Git != nil && req.Workspace.Git.SecretRef == "" {
		if repository, err := credentials.ParseGitHubRepository(req.Workspace.Git.URL); err == nil {
			managedGitRepository = &repository
		}
	}
	githubClone := managedGitRepository != nil
	if githubClone {
		// Creation IDs for Git credential bindings are generated only by the API.
		req.CreationID = ""
	}
	if req.CreationID != "" {
		decoded, err := base64.RawURLEncoding.DecodeString(req.CreationID)
		if err != nil || len(decoded) != 16 || base64.RawURLEncoding.EncodeToString(decoded) != req.CreationID {
			http.Error(w, "creationID must be a 16-byte unpadded base64url value", http.StatusBadRequest)
			return
		}
	}
	if req.MaxDurationSeconds != nil && *req.MaxDurationSeconds < 1 {
		http.Error(w, "maxDurationSeconds must be at least 1 when specified", http.StatusBadRequest)
		return
	}
	if req.MaxDurationSeconds != nil && *req.MaxDurationSeconds > sandboxesv1alpha1.MaxActiveDeadlineSeconds {
		http.Error(w, fmt.Sprintf("maxDurationSeconds cannot exceed %d", sandboxesv1alpha1.MaxActiveDeadlineSeconds), http.StatusBadRequest)
		return
	}
	for _, env := range req.Env {
		if sandboxesv1alpha1.IsReservedAgentEnvName(env.Name) || env.Name == "OPENAI_API_KEY" || env.Name == "ANTHROPIC_API_KEY" || env.Name == "BUDDY_PROVIDER_MODE" || env.Name == "ANTHROPIC_BASE_URL" || env.Name == "OPENAI_BASE_URL" {
			http.Error(w, fmt.Sprintf("environment variable %q is reserved by the sandbox controller", env.Name), http.StatusBadRequest)
			return
		}
	}
	if req.Workspace != nil && req.Workspace.Git != nil {
		if err := gitrepo.ValidateSubpath(req.Workspace.Git.Subpath); err != nil {
			http.Error(w, fmt.Sprintf("Invalid Git subpath: %v", err), http.StatusBadRequest)
			return
		}
		if managedGitRepository == nil {
			gitURL, err := url.Parse(req.Workspace.Git.URL)
			unsafeUserinfo := false
			if gitURL != nil && gitURL.User != nil {
				_, hasPassword := gitURL.User.Password()
				unsafeUserinfo = strings.EqualFold(gitURL.Scheme, "http") || strings.EqualFold(gitURL.Scheme, "https") || hasPassword
			}
			if (err != nil && strings.Contains(req.Workspace.Git.URL, "://")) || unsafeUserinfo {
				http.Error(w, "Git repository URLs must not contain userinfo", http.StatusBadRequest)
				return
			}
		}
		if req.Workspace.Git.SecretRef != "" {
			if err := gitrepo.ValidateCredentialURL(req.Workspace.Git.URL); err != nil {
				http.Error(w, fmt.Sprintf("Invalid credentialed Git repository URL: %v", err), http.StatusBadRequest)
				return
			}
		}
	}

	sandboxName := req.Name
	if sandboxName != "" {
		if err := validateSandboxName(sandboxName); err != nil {
			http.Error(w, fmt.Sprintf("Invalid sandbox name: %v", err), http.StatusBadRequest)
			return
		}
	} else {
		sandboxName = generateRandomID("sbx")
	}
	if req.Provider != "" && req.CreationID == "" {
		creationID, err := newCreationID()
		if err != nil {
			http.Error(w, "Unable to create Sandbox", http.StatusInternalServerError)
			return
		}
		req.CreationID = creationID
	}
	principal, hasPrincipal := PrincipalFromContext(r.Context())
	if req.Provider != "" && (!hasPrincipal || principal.ID == "" || principal.Namespace != ns) {
		http.Error(w, "Provider credentials require a user identity", http.StatusForbidden)
		return
	}

	mergedLabels, err := filterAndValidateLabels(req.Labels)
	if err != nil {
		http.Error(w, fmt.Sprintf("Invalid labels: %v", err), http.StatusBadRequest)
		return
	}

	sbx := sandboxesv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sandboxName,
			Namespace: ns,
			Labels:    mergedLabels,
			Annotations: func() map[string]string {
				if req.CreationID == "" {
					return nil
				}
				return map[string]string{sandboxCreationIDAnnotation: req.CreationID}
			}(),
		},
		Spec: sandboxesv1alpha1.SandboxSpec{
			Image:                 req.Image,
			Command:               req.Command,
			Args:                  req.Args,
			WorkingDir:            req.WorkingDir,
			MaxDurationSeconds:    req.MaxDurationSeconds,
			ShareProcessNamespace: req.ShareProcessNamespace,
			RuntimeClassName:      req.RuntimeClassName,
		},
	}

	if req.Workspace != nil {
		sbx.Spec.Workspace = sandboxesv1alpha1.WorkspaceSpec{
			Ephemeral: &req.Workspace.Ephemeral,
			MountPath: req.Workspace.MountPath,
		}
		if req.Workspace.Git != nil {
			sbx.Spec.Workspace.Git = &sandboxesv1alpha1.GitRepositorySpec{
				URL:      req.Workspace.Git.URL,
				Revision: req.Workspace.Git.Revision,
				Depth:    req.Workspace.Git.Depth,
				Subpath:  req.Workspace.Git.Subpath,
			}
			if req.Workspace.Git.SecretRef != "" {
				sbx.Spec.Workspace.Git.SecretRef = &corev1.LocalObjectReference{
					Name: req.Workspace.Git.SecretRef,
				}
			}
		}
		if req.Workspace.PersistentVolumeClaim != nil {
			sbx.Spec.Workspace.PersistentVolumeClaim = &sandboxesv1alpha1.PersistentVolumeClaimWorkspace{
				ClaimName: req.Workspace.PersistentVolumeClaim.ClaimName,
				ReadOnly:  req.Workspace.PersistentVolumeClaim.ReadOnly,
			}
		}
	} else {
		ephemeral := true
		sbx.Spec.Workspace = sandboxesv1alpha1.WorkspaceSpec{
			Ephemeral: &ephemeral,
			MountPath: workspaceMountPath,
		}
	}

	if req.Network != nil {
		sbx.Spec.Network = sandboxesv1alpha1.NetworkSpec{
			AllowAPIBroker:       req.Network.AllowAPIBroker,
			AllowInternetWeb:     req.Network.AllowInternetWeb,
			AllowDNS:             req.Network.AllowDNS,
			BlockClusterInternal: req.Network.BlockClusterInternal,
			BlockMetadataService: req.Network.BlockMetadataService,
			AllowedDomains:       req.Network.AllowedDomains,
		}
	} else {
		sbx.Spec.Network = sandboxesv1alpha1.NetworkSpec{
			AllowInternetWeb:     true,
			AllowDNS:             true,
			BlockClusterInternal: true,
			BlockMetadataService: true,
		}
	}

	if req.Resources != nil {
		resList := corev1.ResourceRequirements{
			Limits:   corev1.ResourceList{},
			Requests: corev1.ResourceList{},
		}
		for k, v := range req.Resources.Limits {
			q, err := resource.ParseQuantity(v)
			if err != nil {
				http.Error(w, fmt.Sprintf("Invalid resource limit for %s: %v", k, err), http.StatusBadRequest)
				return
			}
			resList.Limits[corev1.ResourceName(k)] = q
		}
		for k, v := range req.Resources.Requests {
			q, err := resource.ParseQuantity(v)
			if err != nil {
				http.Error(w, fmt.Sprintf("Invalid resource request for %s: %v", k, err), http.StatusBadRequest)
				return
			}
			resList.Requests[corev1.ResourceName(k)] = q
		}
		sbx.Spec.Resources = resList
	}

	if len(req.Env) > 0 {
		for _, e := range req.Env {
			sbx.Spec.Env = append(sbx.Spec.Env, corev1.EnvVar{
				Name:  e.Name,
				Value: e.Value,
			})
		}
	}
	var capabilityHash []byte
	var gitBinding credentials.GitBinding
	var hasGitBinding bool
	if githubClone && h.gitCredentials != nil {
		if !hasPrincipal || principal.ID == "" || principal.Namespace != ns {
			http.Error(w, "Git credentials require a user identity", http.StatusForbidden)
			return
		}
		creationID, err := newCreationID()
		if err != nil {
			http.Error(w, "Unable to create Sandbox", http.StatusInternalServerError)
			return
		}
		req.CreationID = creationID
		if sbx.Annotations == nil {
			sbx.Annotations = make(map[string]string)
		}
		sbx.Annotations[sandboxCreationIDAnnotation] = creationID
		gitBinding, hasGitBinding, err = h.gitCredentials.PrepareBinding(r.Context(), principal.ID, ns, sandboxName, creationID, *managedGitRepository)
		if err != nil {
			http.Error(w, "Unable to resolve GitHub credentials", http.StatusServiceUnavailable)
			return
		}
		if hasGitBinding {
			sbx.Annotations[gitCredentialRedemptionAnnotation] = queryTrue
		}
	}
	if req.Provider != "" {
		hash, err := BuildProviderBinding(r.Context(), h.providerCredentials, h.brokerCapabilities, ProviderBindingParams{
			UserID:             principal.ID,
			Namespace:          ns,
			SandboxName:        sandboxName,
			CreationID:         req.CreationID,
			Provider:           req.Provider,
			Model:              req.Model,
			MaxDurationSeconds: req.MaxDurationSeconds,
		}, &sbx)
		if err != nil {
			var pErr *ProviderBindingError
			if errors.As(err, &pErr) {
				http.Error(w, pErr.Message, pErr.StatusCode)
			} else {
				http.Error(w, err.Error(), http.StatusInternalServerError)
			}
			return
		}
		capabilityHash = hash
	}

	if err := applyMCPServers(&sbx, req.MCPServers); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if err := h.client.Create(r.Context(), &sbx); err != nil {
		if hasGitBinding {
			created := &sandboxesv1alpha1.Sandbox{}
			getErr := h.client.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: sandboxName}, created)
			if getErr == nil && created.Annotations[sandboxCreationIDAnnotation] == gitBinding.CreationID && created.UID != "" {
				if bindErr := h.gitCredentials.Bind(r.Context(), gitBinding.ID, string(created.UID)); bindErr == nil {
					sbx = *created
					hasGitBinding = false
				} else {
					_ = h.gitCredentials.RollbackBinding(r.Context(), gitBinding.ID)
					http.Error(w, "Unable to bind GitHub credentials to Sandbox", http.StatusServiceUnavailable)
					return
				}
			} else if getErr != nil && !apierrors.IsNotFound(getErr) {
				http.Error(w, "Unable to confirm Sandbox creation", http.StatusServiceUnavailable)
				return
			} else {
				_ = h.gitCredentials.RollbackBinding(r.Context(), gitBinding.ID)
				hasGitBinding = false
			}
		}
		if hasGitBinding {
			http.Error(w, "Unable to confirm Sandbox creation", http.StatusServiceUnavailable)
			return
		}
		if sbx.UID == "" {
			if len(capabilityHash) > 0 {
				_ = h.brokerCapabilities.Revoke(r.Context(), capabilityHash)
			}
			if apierrors.IsAlreadyExists(err) {
				http.Error(w, fmt.Sprintf("Sandbox %s already exists", sandboxName), http.StatusConflict)
				return
			}
			http.Error(w, fmt.Sprintf("Failed to create sandbox custom resource: %v", err), http.StatusInternalServerError)
			return
		}
	}
	if len(capabilityHash) > 0 {
		if err := h.brokerCapabilities.Activate(r.Context(), capabilityHash, string(sbx.UID)); err != nil {
			_ = h.brokerCapabilities.Revoke(r.Context(), capabilityHash)
			_ = h.client.Delete(r.Context(), &sbx)
			http.Error(w, "Unable to activate provider capability", http.StatusServiceUnavailable)
			return
		}
	}
	if hasGitBinding {
		if err := h.gitCredentials.Bind(r.Context(), gitBinding.ID, string(sbx.UID)); err != nil {
			_ = h.gitCredentials.RollbackBinding(r.Context(), gitBinding.ID)
			_ = h.client.Delete(r.Context(), &sbx)
			http.Error(w, "Unable to bind GitHub credentials to Sandbox", http.StatusServiceUnavailable)
			return
		}
	}

	resp := mapSandboxToResponse(&sbx)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(resp)
}

// ListSandboxesHandler handles GET /api/v1/sandboxes.
func (h *Handlers) ListSandboxesHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ns, ok := TenantNamespaceFromContext(r.Context())
	if !ok || ns == "" {
		http.Error(w, "Tenant namespace not resolved", http.StatusUnauthorized)
		return
	}

	var sbxList sandboxesv1alpha1.SandboxList
	if err := h.client.List(r.Context(), &sbxList, client.InNamespace(ns)); err != nil {
		http.Error(w, fmt.Sprintf("Failed to list sandboxes: %v", err), http.StatusInternalServerError)
		return
	}

	resp := apiv1.ListSandboxesResponse{
		Items: make([]apiv1.SandboxResponse, 0, len(sbxList.Items)),
		Total: len(sbxList.Items),
	}

	for i := range sbxList.Items {
		resp.Items = append(resp.Items, mapSandboxToResponse(&sbxList.Items[i]))
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

// GetSandboxHandler handles GET /api/v1/sandboxes/{id}.
func (h *Handlers) GetSandboxHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ns, ok := TenantNamespaceFromContext(r.Context())
	if !ok || ns == "" {
		http.Error(w, "Tenant namespace not resolved", http.StatusUnauthorized)
		return
	}

	id := extractIDFromRequest(r)
	if id == "" {
		http.Error(w, "Missing sandbox ID", http.StatusBadRequest)
		return
	}

	var sbx sandboxesv1alpha1.Sandbox
	if err := h.client.Get(r.Context(), types.NamespacedName{Namespace: ns, Name: id}, &sbx); err != nil {
		if apierrors.IsNotFound(err) {
			http.Error(w, fmt.Sprintf("Sandbox %s not found", id), http.StatusNotFound)
			return
		}
		http.Error(w, fmt.Sprintf("Failed to get sandbox: %v", err), http.StatusInternalServerError)
		return
	}

	resp := mapSandboxToResponse(&sbx)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

// DeleteSandboxHandler handles DELETE /api/v1/sandboxes/{id}.
func (h *Handlers) DeleteSandboxHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ns, ok := TenantNamespaceFromContext(r.Context())
	if !ok || ns == "" {
		http.Error(w, "Tenant namespace not resolved", http.StatusUnauthorized)
		return
	}

	id := extractIDFromRequest(r)
	if id == "" {
		http.Error(w, "Missing sandbox ID", http.StatusBadRequest)
		return
	}

	var sbx sandboxesv1alpha1.Sandbox
	if err := h.client.Get(r.Context(), types.NamespacedName{Namespace: ns, Name: id}, &sbx); err != nil {
		if apierrors.IsNotFound(err) {
			http.Error(w, fmt.Sprintf("Sandbox %s not found", id), http.StatusNotFound)
			return
		}
		http.Error(w, fmt.Sprintf("Failed to get sandbox: %v", err), http.StatusInternalServerError)
		return
	}

	if err := h.client.Delete(r.Context(), &sbx); err != nil {
		http.Error(w, fmt.Sprintf("Failed to delete sandbox: %v", err), http.StatusInternalServerError)
		return
	}
	h.revokeProviderCapabilities(r.Context(), ns, id, sbx.Annotations[sandboxCreationIDAnnotation])
	if h.gitCredentials != nil {
		_ = h.gitCredentials.RevokeSandbox(r.Context(), ns, id, string(sbx.UID), sbx.Annotations[sandboxCreationIDAnnotation])
	}

	w.WriteHeader(http.StatusNoContent)
}

// SetSandboxSuspensionHandler records suspend/resume desired state.
func (h *Handlers) SetSandboxSuspensionHandler(w http.ResponseWriter, r *http.Request, suspend bool) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ns, ok := TenantNamespaceFromContext(r.Context())
	if !ok || ns == "" {
		http.Error(w, "Tenant namespace not resolved", http.StatusUnauthorized)
		return
	}
	id := extractIDFromRequest(r)
	if id == "" {
		http.Error(w, "Missing sandbox ID", http.StatusBadRequest)
		return
	}
	var sbx sandboxesv1alpha1.Sandbox
	if err := h.client.Get(r.Context(), types.NamespacedName{Namespace: ns, Name: id}, &sbx); err != nil {
		if apierrors.IsNotFound(err) {
			http.Error(w, fmt.Sprintf("Sandbox %s not found", id), http.StatusNotFound)
			return
		}
		http.Error(w, fmt.Sprintf("Failed to get sandbox: %v", err), http.StatusInternalServerError)
		return
	}
	if sbx.Status.Phase == sandboxesv1alpha1.SandboxPhaseSucceeded || sbx.Status.Phase == sandboxesv1alpha1.SandboxPhaseFailed {
		http.Error(w, "Sandbox is terminal and cannot be suspended or resumed", http.StatusConflict)
		return
	}
	if !gatewayWorkspaceSupportsSuspension(&sbx) {
		http.Error(w, "Suspension requires a writable PVC-backed workspace", http.StatusConflict)
		return
	}
	if sbx.Spec.Suspend != suspend {
		sbx.Spec.Suspend = suspend
		if err := h.client.Update(r.Context(), &sbx); err != nil {
			if apierrors.IsConflict(err) {
				http.Error(w, "Sandbox changed concurrently; retry the request", http.StatusConflict)
				return
			}
			http.Error(w, fmt.Sprintf("Failed to update sandbox: %v", err), http.StatusInternalServerError)
			return
		}
	}
	if suspend {
		h.revokeProviderCapabilities(r.Context(), ns, id, sbx.Annotations[sandboxCreationIDAnnotation])
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(mapSandboxToResponse(&sbx))
}

func (h *Handlers) revokeProviderCapabilities(ctx context.Context, namespace, name, creationID string) {
	if h.brokerCapabilities != nil && creationID != "" {
		_ = h.brokerCapabilities.RevokeSandbox(ctx, namespace, name, creationID)
	}
}

func gatewayWorkspaceSupportsSuspension(sbx *sandboxesv1alpha1.Sandbox) bool {
	workspace := sbx.Spec.Workspace
	if pvc := workspace.PersistentVolumeClaim; pvc != nil && pvc.ClaimName != "" {
		return !pvc.ReadOnly
	}
	return !workspaceIsEphemeral(workspace) && workspace.Size != ""
}

func workspaceIsEphemeral(workspace sandboxesv1alpha1.WorkspaceSpec) bool {
	return workspace.Ephemeral == nil || *workspace.Ephemeral
}

func sandboxSuspensionBlocksPodAccess(sbx *sandboxesv1alpha1.Sandbox) bool {
	if sbx.Status.Phase == sandboxesv1alpha1.SandboxPhaseSucceeded || sbx.Status.Phase == sandboxesv1alpha1.SandboxPhaseFailed {
		return false
	}
	return sbx.Spec.Suspend || (sbx.Status.SuspensionPhase != "" && sbx.Status.SuspensionPhase != sandboxesv1alpha1.SuspensionPhaseNone)
}

func rejectSuspendedPodAccess(w http.ResponseWriter, sbx *sandboxesv1alpha1.Sandbox) bool {
	if !sandboxSuspensionBlocksPodAccess(sbx) {
		return false
	}
	http.Error(w, "Sandbox is suspended or transitioning; resume it before accessing the Pod", http.StatusConflict)
	return true
}

// LogsHandler handles GET /api/v1/sandboxes/{id}/logs.
func (h *Handlers) LogsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ns, ok := TenantNamespaceFromContext(r.Context())
	if !ok || ns == "" {
		http.Error(w, "Tenant namespace not resolved", http.StatusUnauthorized)
		return
	}

	id := extractIDFromRequest(r)
	if id == "" {
		http.Error(w, "Missing sandbox ID", http.StatusBadRequest)
		return
	}

	var sbx sandboxesv1alpha1.Sandbox
	if err := h.client.Get(r.Context(), types.NamespacedName{Namespace: ns, Name: id}, &sbx); err != nil {
		if apierrors.IsNotFound(err) {
			http.Error(w, fmt.Sprintf("Sandbox %s not found", id), http.StatusNotFound)
			return
		}
		http.Error(w, fmt.Sprintf("Failed to get sandbox: %v", err), http.StatusInternalServerError)
		return
	}
	if rejectSuspendedPodAccess(w, &sbx) {
		return
	}

	podName := sbx.Status.PodName
	if podName == "" {
		podName = sbx.Name
	}

	follow := r.URL.Query().Get("follow") == queryTrue
	tailStr := r.URL.Query().Get("tailLines")
	var tailLines *int64
	if tailStr != "" {
		if t, err := strconv.ParseInt(tailStr, 10, 64); err == nil && t > 0 {
			tailLines = &t
		}
	}
	timestamps := r.URL.Query().Get("timestamps") == queryTrue
	containerName := r.URL.Query().Get("container")
	if containerName != "" && containerName != agentLogContainer && containerName != gitInitLogContainer {
		http.Error(w, "Unsupported log container", http.StatusBadRequest)
		return
	}

	if h.kubeClient != nil {
		if containerName == "" {
			containerName = agentLogContainer
		}
		if pod, err := h.kubeClient.CoreV1().Pods(ns).Get(r.Context(), podName, metav1.GetOptions{}); err == nil {
			if containerName == gitInitLogContainer && !hasInitContainer(pod, containerName) {
				http.Error(w, "Sandbox has no git-init container", http.StatusNotFound)
				return
			}
		}

		logOpts := &corev1.PodLogOptions{
			Container:  containerName,
			Follow:     follow,
			TailLines:  tailLines,
			Timestamps: timestamps,
		}

		stream, err := h.kubeClient.CoreV1().Pods(ns).GetLogs(podName, logOpts).Stream(r.Context())
		if err != nil {
			http.Error(w, fmt.Sprintf("Failed to get logs for pod %s: %v", podName, err), http.StatusBadGateway)
			return
		}
		defer func() { _ = stream.Close() }()
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		if follow {
			if flusher, ok := w.(http.Flusher); ok {
				buf := make([]byte, 4096)
				for {
					n, readErr := stream.Read(buf)
					if n > 0 {
						_, _ = w.Write(buf[:n])
						flusher.Flush()
					}
					if readErr != nil {
						break
					}
				}
				return
			}
		}
		_, _ = io.Copy(w, stream)
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(w, "[sandbox %s] status: %s\n", id, sbx.Status.Phase)
}

// WhoAmIHandler handles GET /api/v1/auth/whoami.
func (h *Handlers) WhoAmIHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ns, ok := TenantNamespaceFromContext(r.Context())
	if !ok || ns == "" {
		http.Error(w, "Tenant namespace not resolved", http.StatusUnauthorized)
		return
	}

	resp := apiv1.WhoAmIResponse{
		User:      "developer",
		Namespace: ns,
		Roles:     []string{"developer"},
		AuthMode:  "bearer-token",
	}
	if principal, ok := PrincipalFromContext(r.Context()); ok {
		resp.ID = principal.ID
		resp.User = principal.DisplayName
		resp.Role = string(principal.Role)
		resp.Roles = []string{string(principal.Role)}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

// UploadHandler handles POST /api/v1/sandboxes/{id}/upload.
func (h *Handlers) UploadHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ns, ok := TenantNamespaceFromContext(r.Context())
	if !ok || ns == "" {
		http.Error(w, "Tenant namespace not resolved", http.StatusUnauthorized)
		return
	}

	id := extractIDFromRequest(r)
	if id == "" {
		http.Error(w, "Missing sandbox ID", http.StatusBadRequest)
		return
	}

	destPath := r.URL.Query().Get("path")
	if destPath == "" {
		destPath = workspaceMountPath
	}

	var sbx sandboxesv1alpha1.Sandbox
	if err := h.client.Get(r.Context(), types.NamespacedName{Namespace: ns, Name: id}, &sbx); err != nil {
		if apierrors.IsNotFound(err) {
			http.Error(w, fmt.Sprintf("Sandbox %s not found", id), http.StatusNotFound)
			return
		}
		http.Error(w, fmt.Sprintf("Failed to get sandbox: %v", err), http.StatusInternalServerError)
		return
	}
	if rejectSuspendedPodAccess(w, &sbx) {
		return
	}

	if sbx.Status.Phase != sandboxesv1alpha1.SandboxPhaseRunning {
		http.Error(w, fmt.Sprintf("Sandbox is in phase %s, must be Running to upload files", sbx.Status.Phase), http.StatusConflict)
		return
	}

	podName := sbx.Status.PodName
	if podName == "" {
		podName = sbx.Name
	}

	if h.remoteExec == nil {
		http.Error(w, "Remote execution is not configured on gateway", http.StatusInternalServerError)
		return
	}

	cmd := []string{"sh", "-c", "mkdir -p \"$1\" && umask 000 && exec tar -xf - -C \"$1\"", "--", destPath}
	if archiveName := r.URL.Query().Get("file"); archiveName != "" {
		if path.Base(archiveName) != archiveName || archiveName == "." || archiveName == "/" {
			http.Error(w, "Invalid upload filename", http.StatusBadRequest)
			return
		}
		fileScript := `set -e
DEST="$1"
NAME="$2"
if [ -d "$DEST" ]; then
  umask 000
  exec tar -xf - -C "$DEST"
fi
PARENT=$(dirname "$DEST")
mkdir -p "$PARENT"
TMP=$(mktemp -d "$PARENT/.buddy-upload.XXXXXX")
trap 'rm -rf "$TMP"' EXIT
umask 000
tar -xf - -C "$TMP"
if [ ! -f "$TMP/$NAME" ]; then
  echo "archive does not contain expected file $NAME" >&2
  exit 1
fi
mv "$TMP/$NAME" "$DEST"
`
		cmd = []string{"sh", "-c", fileScript, "--", destPath, archiveName}
	} else if archiveName := r.URL.Query().Get("directory"); archiveName != "" {
		if path.Base(archiveName) != archiveName || archiveName == "." || archiveName == "/" {
			http.Error(w, "Invalid upload directory name", http.StatusBadRequest)
			return
		}
		directoryScript := `set -e
DEST="$1"
NAME="$2"
if [ -d "$DEST" ]; then
  umask 000
  exec tar -xf - -C "$DEST"
fi
PARENT=$(dirname "$DEST")
mkdir -p "$PARENT"
TMP=$(mktemp -d "$PARENT/.buddy-upload.XXXXXX")
trap 'rm -rf "$TMP"' EXIT
umask 000
tar -xf - -C "$TMP"
if [ ! -d "$TMP/$NAME" ]; then
  echo "archive does not contain expected directory $NAME" >&2
  exit 1
fi
mv "$TMP/$NAME" "$DEST"
`
		cmd = []string{"sh", "-c", directoryScript, "--", destPath, archiveName}
	}
	var stderrBuf strings.Builder
	exitCode, err := h.remoteExec.StreamExec(r.Context(), ns, podName, cmd, false, r.Body, nil, &stderrBuf, nil)
	if err != nil || exitCode != 0 {
		errMsg := strings.TrimSpace(stderrBuf.String())
		if errMsg == "" && err != nil {
			errMsg = err.Error()
		}
		http.Error(w, fmt.Sprintf("Upload failed (exit code %d): %s", exitCode, errMsg), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status": "uploaded",
		"path":   destPath,
	})
}

// DownloadHandler handles GET /api/v1/sandboxes/{id}/download.
func (h *Handlers) DownloadHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ns, ok := TenantNamespaceFromContext(r.Context())
	if !ok || ns == "" {
		http.Error(w, "Tenant namespace not resolved", http.StatusUnauthorized)
		return
	}

	id := extractIDFromRequest(r)
	if id == "" {
		http.Error(w, "Missing sandbox ID", http.StatusBadRequest)
		return
	}

	srcPath := r.URL.Query().Get("path")
	if srcPath == "" {
		srcPath = workspaceMountPath
	}

	var sbx sandboxesv1alpha1.Sandbox
	if err := h.client.Get(r.Context(), types.NamespacedName{Namespace: ns, Name: id}, &sbx); err != nil {
		if apierrors.IsNotFound(err) {
			http.Error(w, fmt.Sprintf("Sandbox %s not found", id), http.StatusNotFound)
			return
		}
		http.Error(w, fmt.Sprintf("Failed to get sandbox: %v", err), http.StatusInternalServerError)
		return
	}
	if rejectSuspendedPodAccess(w, &sbx) {
		return
	}

	if sbx.Status.Phase != sandboxesv1alpha1.SandboxPhaseRunning {
		http.Error(w, fmt.Sprintf("Sandbox is in phase %s, must be Running to download files", sbx.Status.Phase), http.StatusConflict)
		return
	}

	podName := sbx.Status.PodName
	if podName == "" {
		podName = sbx.Name
	}

	if h.remoteExec == nil {
		http.Error(w, "Remote execution is not configured on gateway", http.StatusInternalServerError)
		return
	}

	srcPath = path.Clean(srcPath)
	dir := path.Dir(srcPath)
	base := path.Base(srcPath)
	if srcPath == "/" {
		dir = "/"
		base = "."
	}

	cmd := []string{"tar", "-cf", "-", "-C", dir, base}
	if r.URL.Query().Get("workspaceRoot") == queryTrue {
		if srcPath != workspaceMountPath {
			http.Error(w, "workspaceRoot is supported only for /workspace", http.StatusBadRequest)
			return
		}
		cmd = []string{"tar", "-cf", "-", "-C", workspaceMountPath, "."}
	}

	pr, pw := io.Pipe()
	defer func() { _ = pr.Close() }()
	var stderrBuf strings.Builder

	go func() {
		exitCode, execErr := h.remoteExec.StreamExec(r.Context(), ns, podName, cmd, false, nil, pw, &stderrBuf, nil)
		if execErr != nil {
			_ = pw.CloseWithError(execErr)
		} else if exitCode != 0 {
			_ = pw.CloseWithError(fmt.Errorf("tar command failed with exit code %d: %s", exitCode, strings.TrimSpace(stderrBuf.String())))
		} else {
			_ = pw.Close()
		}
	}()

	w.Header().Set("Content-Type", "application/x-tar")
	w.Header().Add("Trailer", "X-Buddy-Stream-Error")
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, pr); err != nil {
		w.Header().Set("X-Buddy-Stream-Error", err.Error())
	}
}

func mapSandboxToResponse(sbx *sandboxesv1alpha1.Sandbox) apiv1.SandboxResponse {
	phase := string(sbx.Status.Phase)
	if phase == "" {
		phase = string(sandboxesv1alpha1.SandboxPhasePending)
	}
	suspensionPhase := string(sbx.Status.SuspensionPhase)
	if suspensionPhase == "" {
		suspensionPhase = string(sandboxesv1alpha1.SuspensionPhaseNone)
	}

	var startTime *time.Time
	if sbx.Status.StartTime != nil {
		t := sbx.Status.StartTime.Time
		startTime = &t
	}

	var completionTime *time.Time
	if sbx.Status.CompletionTime != nil {
		t := sbx.Status.CompletionTime.Time
		completionTime = &t
	}
	var failureReason, failureMessage string
	if phase == string(sandboxesv1alpha1.SandboxPhaseFailed) {
		if ready := meta.FindStatusCondition(sbx.Status.Conditions, sandboxesv1alpha1.ConditionReady); ready != nil && ready.Status == metav1.ConditionFalse {
			failureReason = ready.Reason
			failureMessage = ready.Message
		}
	}

	var wsSpec *apiv1.WorkspaceSpec
	if sbx.Spec.Workspace.MountPath != "" || sbx.Spec.Workspace.Ephemeral != nil || sbx.Spec.Workspace.Git != nil || sbx.Spec.Workspace.PersistentVolumeClaim != nil {
		wsSpec = &apiv1.WorkspaceSpec{
			Ephemeral: workspaceIsEphemeral(sbx.Spec.Workspace),
			MountPath: sbx.Spec.Workspace.MountPath,
		}
		if sbx.Spec.Workspace.Git != nil {
			wsSpec.Git = &apiv1.GitRepositorySpec{
				URL:      sbx.Spec.Workspace.Git.URL,
				Revision: sbx.Spec.Workspace.Git.Revision,
				Depth:    sbx.Spec.Workspace.Git.Depth,
				Subpath:  sbx.Spec.Workspace.Git.Subpath,
			}
			if sbx.Spec.Workspace.Git.SecretRef != nil {
				wsSpec.Git.SecretRef = sbx.Spec.Workspace.Git.SecretRef.Name
			}
		}
		if sbx.Spec.Workspace.PersistentVolumeClaim != nil {
			wsSpec.PersistentVolumeClaim = &apiv1.PersistentVolumeClaimWorkspace{
				ClaimName: sbx.Spec.Workspace.PersistentVolumeClaim.ClaimName,
				ReadOnly:  sbx.Spec.Workspace.PersistentVolumeClaim.ReadOnly,
			}
		}
	}

	var mcpServers []apiv1.MCPServerSpec
	if len(sbx.Spec.MCPServers) > 0 {
		for _, s := range sbx.Spec.MCPServers {
			mcpSpec := apiv1.MCPServerSpec{
				Name:                   s.Name,
				Image:                  s.Image,
				Port:                   s.Port,
				Command:                s.Command,
				Args:                   s.Args,
				MountWorkspace:         s.MountWorkspace,
				MountWorkspaceReadOnly: s.MountWorkspaceReadOnly,
			}
			if len(s.Env) > 0 {
				for _, e := range s.Env {
					mcpSpec.Env = append(mcpSpec.Env, apiv1.EnvVar{
						Name:  e.Name,
						Value: e.Value,
					})
				}
			}
			if len(s.Resources.Limits) > 0 || len(s.Resources.Requests) > 0 {
				mcpSpec.Resources = &apiv1.ResourceRequirements{
					Limits:   make(map[string]string),
					Requests: make(map[string]string),
				}
				for k, v := range s.Resources.Limits {
					mcpSpec.Resources.Limits[string(k)] = v.String()
				}
				for k, v := range s.Resources.Requests {
					mcpSpec.Resources.Requests[string(k)] = v.String()
				}
			}
			mcpServers = append(mcpServers, mcpSpec)
		}
	}

	return apiv1.SandboxResponse{
		ID:                    sbx.Name,
		Name:                  sbx.Name,
		Namespace:             sbx.Namespace,
		Image:                 sbx.Spec.Image,
		RuntimeClassName:      sbx.Spec.RuntimeClassName,
		ShareProcessNamespace: sbx.Spec.ShareProcessNamespace != nil && *sbx.Spec.ShareProcessNamespace,
		Phase:                 phase,
		FailureReason:         failureReason,
		FailureMessage:        failureMessage,
		Suspend:               sbx.Spec.Suspend,
		SuspensionPhase:       suspensionPhase,
		PodName:               sbx.Status.PodName,
		PodUID:                sbx.Status.PodUID,
		Labels:                sbx.Labels,
		Workspace:             wsSpec,
		MCPServers:            mcpServers,
		Network: &apiv1.NetworkSpec{
			AllowAPIBroker:       sbx.Spec.Network.AllowAPIBroker,
			AllowInternetWeb:     sbx.Spec.Network.AllowInternetWeb,
			AllowDNS:             sbx.Spec.Network.AllowDNS,
			BlockClusterInternal: sbx.Spec.Network.BlockClusterInternal,
			BlockMetadataService: sbx.Spec.Network.BlockMetadataService,
			AllowedDomains:       sbx.Spec.Network.AllowedDomains,
		},
		StartTime:      startTime,
		CompletionTime: completionTime,
		ExitCode:       sbx.Status.ExitCode,
		CreatedAt:      sbx.CreationTimestamp.Time,
		CreationID:     sbx.Annotations[sandboxCreationIDAnnotation],
	}
}

func hasInitContainer(pod *corev1.Pod, name string) bool {
	for _, container := range pod.Spec.InitContainers {
		if container.Name == name {
			return true
		}
	}
	return false
}

const sandboxPathPrefix = "/api/v1/sandboxes/"

func extractIDFromRequest(r *http.Request) string {
	if r != nil {
		if param := chi.URLParam(r, "name"); param != "" {
			return param
		}
		return extractIDFromPath(r.URL.Path)
	}
	return ""
}

func extractIDFromPath(fullPath string) string {
	trimmed := strings.TrimPrefix(fullPath, sandboxPathPrefix)
	parts := strings.Split(trimmed, "/")
	if len(parts) > 0 {
		return parts[0]
	}
	return ""
}

func applyMCPServers(sbx *sandboxesv1alpha1.Sandbox, reqMCPServers []apiv1.MCPServerSpec) error {
	if len(reqMCPServers) == 0 {
		return nil
	}
	for _, mcp := range reqMCPServers {
		serverSpec := sandboxesv1alpha1.MCPServerSpec{
			Name:                   mcp.Name,
			Image:                  mcp.Image,
			Port:                   mcp.Port,
			Command:                mcp.Command,
			Args:                   mcp.Args,
			MountWorkspace:         mcp.MountWorkspace,
			MountWorkspaceReadOnly: mcp.MountWorkspaceReadOnly,
		}
		if len(mcp.Env) > 0 {
			for _, e := range mcp.Env {
				serverSpec.Env = append(serverSpec.Env, corev1.EnvVar{
					Name:  e.Name,
					Value: e.Value,
				})
			}
		}
		if mcp.Resources != nil {
			resList := corev1.ResourceRequirements{
				Limits:   corev1.ResourceList{},
				Requests: corev1.ResourceList{},
			}
			for k, v := range mcp.Resources.Limits {
				q, err := resource.ParseQuantity(v)
				if err != nil {
					return fmt.Errorf("invalid resource limit for MCP sidecar %s %s: %w", mcp.Name, k, err)
				}
				resList.Limits[corev1.ResourceName(k)] = q
			}
			for k, v := range mcp.Resources.Requests {
				q, err := resource.ParseQuantity(v)
				if err != nil {
					return fmt.Errorf("invalid resource request for MCP sidecar %s %s: %w", mcp.Name, k, err)
				}
				resList.Requests[corev1.ResourceName(k)] = q
			}
			serverSpec.Resources = resList
		}
		sbx.Spec.MCPServers = append(sbx.Spec.MCPServers, serverSpec)
	}
	return nil
}
