package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	sandboxesv1alpha1 "github.com/hoangndst/buddy-sandboxes/api/v1alpha1"
)

func TestValidateGitWorkspace(t *testing.T) {
	tests := []struct {
		name string
		spec *sandboxesv1alpha1.GitRepositorySpec
		want bool
	}{
		{name: "legacy SSH public clone", spec: &sandboxesv1alpha1.GitRepositorySpec{URL: "git@github.com:team/repo.git"}, want: true},
		{name: "credential-free HTTPS clone", spec: &sandboxesv1alpha1.GitRepositorySpec{URL: "https://github.com/team/repo.git"}, want: true},
		{name: "reject URL userinfo", spec: &sandboxesv1alpha1.GitRepositorySpec{URL: "https://user:token@github.com/team/repo.git"}},
		{name: "reject traversal subpath", spec: &sandboxesv1alpha1.GitRepositorySpec{URL: "https://github.com/team/repo.git", Subpath: "../../outside"}},
		{name: "credential requires canonical HTTPS", spec: &sandboxesv1alpha1.GitRepositorySpec{
			URL: "http://github.com/team/repo.git", SecretRef: &corev1.LocalObjectReference{Name: "git"},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateGitWorkspace(tt.spec)
			if (err == nil) != tt.want {
				t.Fatalf("validateGitWorkspace() error=%v, wantValid=%t", err, tt.want)
			}
		})
	}
}

func TestShouldRetainFailedGitInitPod(t *testing.T) {
	sandbox := &sandboxesv1alpha1.Sandbox{}
	meta.SetStatusCondition(&sandbox.Status.Conditions, metav1.Condition{
		Type: sandboxesv1alpha1.ConditionReady, Status: metav1.ConditionFalse, Reason: "InitContainerFailed",
	})
	if !shouldRetainFailurePod(sandbox) {
		t.Fatal("failed init-container Pod must remain available for logs")
	}
	meta.SetStatusCondition(&sandbox.Status.Conditions, metav1.Condition{
		Type: sandboxesv1alpha1.ConditionReady, Status: metav1.ConditionFalse, Reason: "DeadlineExceeded",
	})
	if shouldRetainFailurePod(sandbox) {
		t.Fatal("deadline failure Pod should retain the existing cleanup behavior")
	}
}
