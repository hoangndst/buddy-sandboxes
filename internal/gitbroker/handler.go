package gitbroker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/hoangndst/buddy-sandboxes/api/v1alpha1"
	"github.com/hoangndst/buddy-sandboxes/internal/credentials"
	"github.com/hoangndst/buddy-sandboxes/internal/identity"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	Audience             = "buddy-git-credential-broker"
	RedeemPath           = "/internal/v1/git-clone-credentials/redeem"
	CompletePath         = "/internal/v1/git-clone-credentials/complete"
	redemptionLabel      = "sandboxes.buddy.io/git-credential-redemption"
	creationIDAnnotation = "sandboxes.buddy.io/creation-id"
	maxRequestBody       = 16 << 10
	maxInitDuration      = 5 * time.Minute
	serviceUnavailable   = "service_unavailable"
)

type GitStore interface {
	Ready(context.Context) error
	Binding(context.Context, string, string, string, string, string) (credentials.GitBinding, error)
	ClaimPod(context.Context, string) (string, string, error)
	ClaimGitCredential(context.Context, credentials.GitBinding, string, string, time.Time, string) (string, error)
	CompleteGitCredential(context.Context, credentials.GitBinding, string, string) error
}

type UserStore interface {
	GetUser(context.Context, string) (identity.User, error)
}

type Handler struct {
	kube     kubernetes.Interface
	objects  client.Client
	git      GitStore
	users    UserStore
	audience string
	saName   string
}

func NewHandler(kube kubernetes.Interface, objects client.Client, git GitStore, users UserStore, audience string) *Handler {
	return &Handler{kube: kube, objects: objects, git: git, users: users, audience: audience, saName: "buddy-sandbox"}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/readyz" {
		if h.git == nil || h.git.Ready(r.Context()) != nil {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		return
	}
	if r.Method != http.MethodPost || (r.URL.Path != RedeemPath && r.URL.Path != CompletePath) {
		http.NotFound(w, r)
		return
	}
	requestID := newRequestID()
	w.Header().Set("X-Request-ID", requestID)
	started := time.Now()
	operation, result, podUID, sandboxUID := h.processBrokerRequest(w, r)
	logBrokerRequest(requestID, operation, result, podUID, sandboxUID, time.Since(started))
}

type brokerRequest struct{ Repository, Outcome string }

func (h *Handler) processBrokerRequest(w http.ResponseWriter, r *http.Request) (string, string, string, string) {
	operation := "redeem"
	if r.URL.Path == CompletePath {
		operation = "complete"
	}
	request, result := decodeBrokerRequest(w, r)
	if result != "" {
		return operation, result, "", ""
	}
	principal, pod, sandbox, binding, err := h.authorizePod(r.Context(), r.Header.Get("Authorization"), request.Repository)
	if result, handled := writeAuthorizationError(w, err, operation); handled {
		return operation, result, "", ""
	}
	if principal.ID != binding.UserID || pod.Namespace != binding.Namespace || sandbox.Name != binding.SandboxName {
		http.Error(w, "Git credential request is not authorized", http.StatusForbidden)
		return operation, "binding_identity_mismatch", string(pod.UID), string(sandbox.UID)
	}
	if operation == "complete" {
		result = h.completeClaim(w, r, request, binding, pod)
	} else {
		result = h.redeemClaim(w, r, binding, pod, sandbox)
	}
	return operation, result, string(pod.UID), string(sandbox.UID)
}

func decodeBrokerRequest(w http.ResponseWriter, r *http.Request) (brokerRequest, string) {
	if r.ContentLength > maxRequestBody {
		http.Error(w, "Request too large", http.StatusRequestEntityTooLarge)
		return brokerRequest{}, "request_too_large"
	}
	requestBody, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBody))
	if err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return brokerRequest{}, "invalid_request_body"
	}
	var request brokerRequest
	if err := json.Unmarshal(requestBody, &request); err != nil || request.Repository == "" {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return brokerRequest{}, "invalid_request"
	}
	return request, ""
}

func writeAuthorizationError(w http.ResponseWriter, err error, operation string) (string, bool) {
	if err == nil {
		return "", false
	}
	if errors.Is(err, credentials.ErrGitBindingNotFound) && operation == "redeem" {
		w.WriteHeader(http.StatusNoContent)
		return "no_matching_credential", true
	}
	if errors.Is(err, credentials.ErrGitBindingPending) {
		w.Header().Set("Retry-After", "1")
		http.Error(w, "Git credential binding is pending", http.StatusServiceUnavailable)
		return "binding_pending", true
	}
	if errors.Is(err, credentials.ErrGitBindingRevoked) {
		http.Error(w, "Git credential request is not authorized", http.StatusForbidden)
		return "binding_revoked", true
	}
	reason := rejectionReason(err)
	if isServiceFailure(reason) {
		http.Error(w, "Git credential service unavailable", http.StatusServiceUnavailable)
	} else {
		http.Error(w, "Git credential request is not authorized", http.StatusUnauthorized)
	}
	return reason, true
}

func (h *Handler) completeClaim(w http.ResponseWriter, r *http.Request, request brokerRequest, binding credentials.GitBinding, pod *corev1.Pod) string {
	if request.Outcome != "succeeded" && request.Outcome != "failed" {
		http.Error(w, "Invalid completion outcome", http.StatusBadRequest)
		return "invalid_completion_outcome"
	}
	if err := h.git.CompleteGitCredential(r.Context(), binding, string(pod.UID), request.Outcome); err != nil {
		http.Error(w, "Git credential completion rejected", http.StatusConflict)
		return "completion_rejected"
	}
	w.WriteHeader(http.StatusNoContent)
	return "completed"
}

func (h *Handler) redeemClaim(w http.ResponseWriter, r *http.Request, binding credentials.GitBinding, pod *corev1.Pod, sandbox *v1alpha1.Sandbox) string {
	user, err := h.users.GetUser(r.Context(), binding.UserID)
	if err != nil {
		http.Error(w, "Git credential service unavailable", http.StatusServiceUnavailable)
		return "user_lookup_failed"
	} else if user.Disabled {
		http.Error(w, "Git credential request is not authorized", http.StatusForbidden)
		return "user_disabled"
	}
	oldName, oldUID, err := h.git.ClaimPod(r.Context(), binding.ID)
	if err != nil {
		http.Error(w, "Git credential service unavailable", http.StatusServiceUnavailable)
		return "claim_lookup_failed"
	}
	checkedPriorPodUID := ""
	if oldUID != "" && oldUID != string(pod.UID) {
		oldPod, getErr := h.kube.CoreV1().Pods(binding.Namespace).Get(r.Context(), oldName, metav1.GetOptions{})
		if apierrors.IsNotFound(getErr) || getErr == nil && string(oldPod.UID) != oldUID {
			checkedPriorPodUID = oldUID
		}
		if getErr != nil && !apierrors.IsNotFound(getErr) {
			http.Error(w, "Git credential service unavailable", http.StatusServiceUnavailable)
			return "old_pod_lookup_failed"
		}
	}
	deadline := podDeadline(pod, sandbox)
	if deadline.IsZero() || time.Now().After(deadline) {
		http.Error(w, "Git clone initialization deadline exceeded", http.StatusGone)
		return "claim_deadline_exceeded"
	}
	value, err := h.git.ClaimGitCredential(r.Context(), binding, pod.Name, string(pod.UID), deadline, checkedPriorPodUID)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			http.Error(w, "Git credential service unavailable", http.StatusServiceUnavailable)
			return serviceUnavailable
		} else {
			http.Error(w, "Git credential request rejected", http.StatusConflict)
			return "credential_claim_rejected"
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, value)
	return "credential_delivered"
}

func logBrokerRequest(requestID, operation, result, podUID, sandboxUID string, duration time.Duration) {
	attrs := []any{"request_id", requestID, "operation", operation, "result", result, "duration_ms", duration.Milliseconds()}
	if podUID != "" {
		attrs = append(attrs, "pod_uid", podUID)
	}
	if sandboxUID != "" {
		attrs = append(attrs, "sandbox_uid", sandboxUID)
	}
	switch {
	case result == "credential_delivered" || result == "completed" || result == "no_matching_credential":
		slog.Info("git credential broker request", attrs...)
	case strings.HasSuffix(result, "_failed") || result == serviceUnavailable || result == "internal_error":
		slog.Error("git credential broker request", attrs...)
	default:
		slog.Warn("git credential broker request", attrs...)
	}
}

func newRequestID() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "unavailable"
	}
	return hex.EncodeToString(value[:])
}

type rejectionError struct{ reason string }

func (e rejectionError) Error() string { return e.reason }

func rejectionReason(err error) string {
	var rejected rejectionError
	if errors.As(err, &rejected) {
		return rejected.reason
	}
	return "authorization_failed"
}

func isServiceFailure(reason string) bool {
	return reason == "service_unavailable" || strings.HasSuffix(reason, "_lookup_failed") || reason == "token_review_unavailable"
}

func (h *Handler) authorizePod(ctx context.Context, authorization, repository string) (identity.Principal, *corev1.Pod, *v1alpha1.Sandbox, credentials.GitBinding, error) {
	if h.kube == nil || h.objects == nil || h.git == nil || h.users == nil || h.audience == "" {
		return identity.Principal{}, nil, nil, credentials.GitBinding{}, rejectionError{reason: serviceUnavailable}
	}
	namespace, podName, podUID, err := h.reviewPodToken(ctx, authorization)
	if err != nil {
		return identity.Principal{}, nil, nil, credentials.GitBinding{}, err
	}
	pod, sandbox, err := h.loadOwnedSandbox(ctx, namespace, podName, podUID, repository)
	if err != nil {
		return identity.Principal{}, nil, nil, credentials.GitBinding{}, err
	}
	creationID := sandbox.Annotations[creationIDAnnotation]
	binding, err := h.git.Binding(ctx, namespace, sandbox.Name, string(sandbox.UID), creationID, repository)
	if err != nil {
		if errors.Is(err, credentials.ErrGitBindingNotFound) || errors.Is(err, credentials.ErrGitBindingPending) || errors.Is(err, credentials.ErrGitBindingRevoked) {
			return identity.Principal{}, pod, sandbox, credentials.GitBinding{}, err
		}
		return identity.Principal{}, nil, nil, credentials.GitBinding{}, rejectionError{reason: "binding_lookup_failed"}
	}
	return identity.Principal{ID: binding.UserID, Namespace: binding.Namespace}, pod, sandbox, binding, nil
}

func (h *Handler) reviewPodToken(ctx context.Context, authorization string) (string, string, string, error) {
	parts := strings.Fields(authorization)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return "", "", "", rejectionError{reason: "missing_bearer_token"}
	}
	review, err := h.kube.AuthenticationV1().TokenReviews().Create(ctx, &authenticationv1.TokenReview{
		Spec: authenticationv1.TokenReviewSpec{Token: parts[1], Audiences: []string{h.audience}},
	}, metav1.CreateOptions{})
	if err != nil {
		return "", "", "", rejectionError{reason: "token_review_unavailable"}
	}
	if !review.Status.Authenticated || !contains(review.Status.Audiences, h.audience) {
		return "", "", "", rejectionError{reason: "token_review_rejected"}
	}
	user := review.Status.User
	expectedExtras := user.Extra
	namespaces := user.Groups
	if user.Username == "" || len(namespaces) == 0 {
		return "", "", "", rejectionError{reason: "token_identity_missing"}
	}
	var namespace string
	prefix := "system:serviceaccounts:"
	for _, group := range namespaces {
		if strings.HasPrefix(group, prefix) {
			namespace = strings.TrimPrefix(group, prefix)
			break
		}
	}
	if namespace == "" || user.Username != "system:serviceaccount:"+namespace+":"+h.saName {
		return "", "", "", rejectionError{reason: "service_account_mismatch"}
	}
	podName := first(expectedExtras["authentication.kubernetes.io/pod-name"])
	podUID := first(expectedExtras["authentication.kubernetes.io/pod-uid"])
	if podName == "" || podUID == "" {
		return "", "", "", rejectionError{reason: "pod_claims_missing"}
	}
	return namespace, podName, podUID, nil
}

func (h *Handler) loadOwnedSandbox(ctx context.Context, namespace, podName, podUID, repository string) (*corev1.Pod, *v1alpha1.Sandbox, error) {
	pod, err := h.kube.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return nil, nil, rejectionError{reason: "pod_lookup_failed"}
	}
	if string(pod.UID) != podUID || pod.Spec.ServiceAccountName != h.saName || pod.Labels[redemptionLabel] != "true" {
		return nil, nil, rejectionError{reason: "pod_identity_mismatch"}
	}
	var sandboxOwner *metav1.OwnerReference
	for i := range pod.OwnerReferences {
		owner := &pod.OwnerReferences[i]
		if owner.Kind == "Sandbox" && owner.APIVersion == v1alpha1.GroupVersion.String() && owner.Controller != nil && *owner.Controller {
			sandboxOwner = owner
			break
		}
	}
	if sandboxOwner == nil {
		return nil, nil, rejectionError{reason: "sandbox_owner_missing"}
	}
	sandbox := &v1alpha1.Sandbox{}
	if err := h.objects.Get(ctx, types.NamespacedName{Namespace: namespace, Name: sandboxOwner.Name}, sandbox); err != nil {
		return nil, nil, rejectionError{reason: "sandbox_lookup_failed"}
	}
	if string(sandbox.UID) != string(sandboxOwner.UID) {
		return nil, nil, rejectionError{reason: "sandbox_identity_mismatch"}
	}
	if sandbox.Annotations[creationIDAnnotation] == "" || sandbox.Spec.Workspace.Git == nil || sandbox.Spec.Workspace.Git.SecretRef != nil || sandbox.Spec.Workspace.Git.URL != repository {
		return nil, nil, rejectionError{reason: "repository_mismatch"}
	}
	return pod, sandbox, nil
}

func podDeadline(pod *corev1.Pod, sandbox *v1alpha1.Sandbox) time.Time {
	start := pod.CreationTimestamp.Time
	if pod.Status.StartTime != nil {
		start = pod.Status.StartTime.Time
	}
	if start.IsZero() {
		return time.Time{}
	}
	deadline := start.Add(maxInitDuration)
	if sandbox.Spec.MaxDurationSeconds != nil && !sandbox.CreationTimestamp.IsZero() {
		activeDeadline := sandbox.CreationTimestamp.Add(time.Duration(*sandbox.Spec.MaxDurationSeconds) * time.Second)
		if activeDeadline.Before(deadline) {
			deadline = activeDeadline
		}
	}
	return deadline
}

func contains(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

func first(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}
