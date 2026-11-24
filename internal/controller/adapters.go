/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxesv1alpha1 "github.com/hoangndst/buddy-sandboxes/api/v1alpha1"
)

// StorageMode indicates whether the workspace is backed by an ephemeral or persistent volume.
type StorageMode string

const (
	StorageModeEphemeral  StorageMode = "Ephemeral"
	StorageModePersistent StorageMode = "Persistent"
)

// ResolvedWorkspace encapsulates normalized storage parameters across inline and external CR definitions.
type ResolvedWorkspace struct {
	Mode          StorageMode
	PVCName       string
	MountPath     string
	ReadOnly      bool
	Git           *sandboxesv1alpha1.GitRepositorySpec
	ReclaimPolicy sandboxesv1alpha1.WorkspaceReclaimPolicy
	IsExternalRef bool
}

// ResolvedNetwork encapsulates normalized network parameters across inline and external CR definitions.
type ResolvedNetwork struct {
	ProfileName          string
	AllowedDomains       []string
	AllowInternetWeb     bool
	AllowDNS             bool
	BlockClusterInternal bool
	BlockMetadataService bool
	AllowAPIBroker       bool
	IsProfileRef         bool
}

func (r *SandboxReconciler) resolveWorkspace(ctx context.Context, sandbox *sandboxesv1alpha1.Sandbox) (*ResolvedWorkspace, error) {
	if sandbox.Spec.WorkspaceRef != nil && sandbox.Spec.WorkspaceRef.Name != "" {
		var ws sandboxesv1alpha1.Workspace
		key := client.ObjectKey{Namespace: sandbox.Namespace, Name: sandbox.Spec.WorkspaceRef.Name}
		if err := r.Get(ctx, key, &ws); err != nil {
			return nil, err
		}
		if ws.Status.Phase != sandboxesv1alpha1.WorkspacePhaseReady && ws.Status.Phase != sandboxesv1alpha1.WorkspacePhaseBound && !(ws.Status.Phase == sandboxesv1alpha1.WorkspacePhasePending && ws.Status.PVCName != "") {
			return nil, fmt.Errorf("referenced workspace %s is not Ready (current phase: %s)", ws.Name, ws.Status.Phase)
		}
		mountPath := ws.Spec.MountPath
		if mountPath == "" {
			mountPath = defaultMountPath
		}
		readOnly := false
		if ws.Spec.PersistentVolumeClaim != nil && ws.Spec.PersistentVolumeClaim.ReadOnly {
			readOnly = true
		}
		return &ResolvedWorkspace{
			Mode:          StorageModePersistent,
			PVCName:       ws.Status.PVCName,
			MountPath:     mountPath,
			ReadOnly:      readOnly,
			Git:           ws.Spec.Git,
			ReclaimPolicy: ws.Spec.ReclaimPolicy,
			IsExternalRef: true,
		}, nil
	}

	// Fallback to legacy inline workspace
	mountPath := sandbox.Spec.Workspace.MountPath
	if mountPath == "" {
		mountPath = defaultMountPath
	}
	if !workspaceIsEphemeral(sandbox.Spec.Workspace) && sandbox.Spec.Workspace.Size != "" {
		return &ResolvedWorkspace{
			Mode:          StorageModePersistent,
			PVCName:       fmt.Sprintf("%s-workspace", sandbox.Name),
			MountPath:     mountPath,
			ReadOnly:      false,
			Git:           sandbox.Spec.Workspace.Git,
			ReclaimPolicy: sandbox.Spec.Workspace.ReclaimPolicy,
			IsExternalRef: false,
		}, nil
	}
	if pvc := sandbox.Spec.Workspace.PersistentVolumeClaim; pvc != nil && pvc.ClaimName != "" {
		return &ResolvedWorkspace{
			Mode:          StorageModePersistent,
			PVCName:       pvc.ClaimName,
			MountPath:     mountPath,
			ReadOnly:      pvc.ReadOnly,
			Git:           sandbox.Spec.Workspace.Git,
			ReclaimPolicy: sandboxesv1alpha1.WorkspaceReclaimRetain,
			IsExternalRef: false,
		}, nil
	}

	return &ResolvedWorkspace{
		Mode:          StorageModeEphemeral,
		MountPath:     mountPath,
		ReadOnly:      false,
		Git:           sandbox.Spec.Workspace.Git,
		IsExternalRef: false,
	}, nil
}

func (r *SandboxReconciler) resolveNetwork(ctx context.Context, sandbox *sandboxesv1alpha1.Sandbox) (*ResolvedNetwork, error) {
	if sandbox.Spec.NetworkProfileRef != nil && sandbox.Spec.NetworkProfileRef.Name != "" {
		var profile sandboxesv1alpha1.NetworkProfile
		key := client.ObjectKey{Namespace: sandbox.Namespace, Name: sandbox.Spec.NetworkProfileRef.Name}
		if err := r.Get(ctx, key, &profile); err != nil {
			return nil, err
		}
		allowInternet := true
		if profile.Spec.AllowInternetWeb != nil {
			allowInternet = *profile.Spec.AllowInternetWeb
		}
		allowDNS := true
		if profile.Spec.AllowDNS != nil {
			allowDNS = *profile.Spec.AllowDNS
		}
		blockCluster := true
		if profile.Spec.BlockClusterInternal != nil {
			blockCluster = *profile.Spec.BlockClusterInternal
		}
		blockMetadata := true
		if profile.Spec.BlockMetadataService != nil {
			blockMetadata = *profile.Spec.BlockMetadataService
		}
		if profile.Spec.IsolationLevel == sandboxesv1alpha1.IsolationDefaultDenyAll {
			if profile.Spec.AllowInternetWeb == nil {
				allowInternet = false
			}
			if profile.Spec.AllowDNS == nil {
				allowDNS = false
			}
		}

		return &ResolvedNetwork{
			ProfileName:          profile.Name,
			AllowedDomains:       profile.Spec.AllowedDomains,
			AllowInternetWeb:     allowInternet,
			AllowDNS:             allowDNS,
			BlockClusterInternal: blockCluster,
			BlockMetadataService: blockMetadata,
			AllowAPIBroker:       profile.Spec.AllowAPIBroker,
			IsProfileRef:         true,
		}, nil
	}

	// Fallback to legacy inline network
	net := sandbox.Spec.Network
	return &ResolvedNetwork{
		AllowedDomains:       net.AllowedDomains,
		AllowInternetWeb:     net.AllowInternetWeb,
		AllowDNS:             net.AllowDNS,
		BlockClusterInternal: net.BlockClusterInternal,
		BlockMetadataService: net.BlockMetadataService,
		AllowAPIBroker:       net.AllowAPIBroker,
		IsProfileRef:         false,
	}, nil
}
