package gateway

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxesv1alpha1 "github.com/hoangndst/buddy-sandboxes/api/v1alpha1"
	apiv1 "github.com/hoangndst/buddy-sandboxes/pkg/api/v1"
)

var dnsSubdomainRegex = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

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

// Handlers holds dependency references for REST route handlers.
type Handlers struct {
	client     client.Client
	kubeClient kubernetes.Interface
}

// NewHandlers creates a new Handlers instance.
func NewHandlers(c client.Client, k kubernetes.Interface) *Handlers {
	return &Handlers{client: c, kubeClient: k}
}

// CreateSandboxHandler handles POST /api/v1/sandboxes.
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

	sandboxName := req.Name
	if sandboxName != "" {
		if err := validateSandboxName(sandboxName); err != nil {
			http.Error(w, fmt.Sprintf("Invalid sandbox name: %v", err), http.StatusBadRequest)
			return
		}
	} else {
		sandboxName = generateRandomID("sbx")
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
		},
		Spec: sandboxesv1alpha1.SandboxSpec{
			Image:              req.Image,
			Command:            req.Command,
			Args:               req.Args,
			WorkingDir:         req.WorkingDir,
			MaxDurationSeconds: req.MaxDurationSeconds,
		},
	}

	if req.Workspace != nil {
		sbx.Spec.Workspace = sandboxesv1alpha1.WorkspaceSpec{
			Ephemeral: req.Workspace.Ephemeral,
			MountPath: req.Workspace.MountPath,
		}
	} else {
		sbx.Spec.Workspace = sandboxesv1alpha1.WorkspaceSpec{
			Ephemeral: true,
			MountPath: "/workspace",
		}
	}

	if req.Network != nil {
		sbx.Spec.Network = sandboxesv1alpha1.NetworkSpec{
			AllowInternetWeb:     req.Network.AllowInternetWeb,
			AllowDNS:             req.Network.AllowDNS,
			BlockClusterInternal: req.Network.BlockClusterInternal,
			BlockMetadataService: req.Network.BlockMetadataService,
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

	if err := h.client.Create(r.Context(), &sbx); err != nil {
		if apierrors.IsAlreadyExists(err) {
			http.Error(w, fmt.Sprintf("Sandbox %s already exists", sandboxName), http.StatusConflict)
			return
		}
		http.Error(w, fmt.Sprintf("Failed to create sandbox custom resource: %v", err), http.StatusInternalServerError)
		return
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

	id := extractIDFromPath(r.URL.Path)
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

	id := extractIDFromPath(r.URL.Path)
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

	w.WriteHeader(http.StatusNoContent)
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

	id := extractIDFromPath(r.URL.Path)
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

	podName := sbx.Status.PodName
	if podName == "" {
		podName = sbx.Name
	}

	follow := r.URL.Query().Get("follow") == "true"
	tailStr := r.URL.Query().Get("tailLines")
	var tailLines *int64
	if tailStr != "" {
		if t, err := strconv.ParseInt(tailStr, 10, 64); err == nil && t > 0 {
			tailLines = &t
		}
	}
	timestamps := r.URL.Query().Get("timestamps") == "true"

	if h.kubeClient != nil {
		containerName := "agent"
		if pod, err := h.kubeClient.CoreV1().Pods(ns).Get(r.Context(), podName, metav1.GetOptions{}); err == nil && len(pod.Spec.Containers) > 0 {
			containerName = pod.Spec.Containers[0].Name
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

func mapSandboxToResponse(sbx *sandboxesv1alpha1.Sandbox) apiv1.SandboxResponse {
	phase := string(sbx.Status.Phase)
	if phase == "" {
		phase = string(sandboxesv1alpha1.SandboxPhasePending)
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

	return apiv1.SandboxResponse{
		ID:             sbx.Name,
		Name:           sbx.Name,
		Namespace:      sbx.Namespace,
		Image:          sbx.Spec.Image,
		Phase:          phase,
		PodName:        sbx.Status.PodName,
		Labels:         sbx.Labels,
		StartTime:      startTime,
		CompletionTime: completionTime,
		ExitCode:       sbx.Status.ExitCode,
		CreatedAt:      sbx.CreationTimestamp.Time,
	}
}

const sandboxPathPrefix = "/api/v1/sandboxes/"

func extractIDFromPath(fullPath string) string {
	trimmed := strings.TrimPrefix(fullPath, sandboxPathPrefix)
	parts := strings.Split(trimmed, "/")
	if len(parts) > 0 {
		return parts[0]
	}
	return ""
}
