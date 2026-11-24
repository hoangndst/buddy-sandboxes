package controller

import (
	"context"
	"log/slog"
	"time"

	"github.com/hoangndst/buddy-sandboxes/internal/credentials"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxesv1alpha1 "github.com/hoangndst/buddy-sandboxes/api/v1alpha1"
)

type gitBindingLifecycle interface {
	ListBindings(context.Context) ([]credentials.GitBindingRecord, error)
	RevokeSandbox(context.Context, string, string, string, string) error
	CleanupBinding(context.Context, string, string, string, string, string) error
	ExpirePendingBindings(context.Context) error
	ExpireUnclaimedSandboxCredentials(context.Context) error
}

// RunGitBindingSweeper reconciles durable credential bindings with Sandbox lifecycle state.
func RunGitBindingSweeper(ctx context.Context, store gitBindingLifecycle, kube client.Client) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		if err := sweepGitBindings(ctx, store, kube); err != nil && ctx.Err() == nil {
			slog.Error("unable to reconcile Git credential bindings", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func sweepGitBindings(ctx context.Context, store gitBindingLifecycle, kube client.Client) error {
	if err := store.ExpirePendingBindings(ctx); err != nil {
		return err
	}
	if err := store.ExpireUnclaimedSandboxCredentials(ctx); err != nil {
		return err
	}
	bindings, err := store.ListBindings(ctx)
	if err != nil {
		return err
	}
	for _, binding := range bindings {
		if binding.State == "pending" {
			continue
		}
		sandbox := &sandboxesv1alpha1.Sandbox{}
		err := kube.Get(ctx, client.ObjectKey{Namespace: binding.Namespace, Name: binding.SandboxName}, sandbox)
		if apierrors.IsNotFound(err) || err == nil && string(sandbox.UID) != binding.SandboxUID {
			if cleanupErr := store.CleanupBinding(ctx, binding.ID, binding.Namespace, binding.SandboxName, binding.SandboxUID, binding.CreationID); cleanupErr != nil {
				return cleanupErr
			}
			continue
		}
		if err != nil {
			return err
		}
		if !sandbox.DeletionTimestamp.IsZero() {
			if err := store.RevokeSandbox(ctx, binding.Namespace, binding.SandboxName, binding.SandboxUID, binding.CreationID); err != nil {
				return err
			}
		}
	}
	return nil
}
