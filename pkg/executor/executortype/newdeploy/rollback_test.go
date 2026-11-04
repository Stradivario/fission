// SPDX-FileCopyrightText: The Fission Authors
//
// SPDX-License-Identifier: Apache-2.0

package newdeploy

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/go-logr/logr/testr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	apiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	fv1 "github.com/fission/fission/pkg/apis/core/v1"
	fissionfake "github.com/fission/fission/pkg/generated/clientset/versioned/fake"
)

const (
	ns       = "6aeb17a306af2d7ab94d44a6"
	fnName   = "api"
	deplName = "newdeploy-api"
)

func pkgRef(name string) fv1.PackageRef {
	return fv1.PackageRef{Namespace: ns, Name: name, ResourceVersion: name + "-rv"}
}

func function(pkg fv1.PackageRef, lastReady *fv1.PackageRef) *fv1.Function {
	fn := &fv1.Function{
		ObjectMeta: metav1.ObjectMeta{Name: fnName, Namespace: ns, Annotations: map[string]string{}},
	}
	fn.Spec.Package.PackageRef = pkg
	if lastReady != nil {
		raw, _ := json.Marshal(lastReady)
		fn.Annotations[AnnotationLastReadyPackage] = string(raw)
	}
	return fn
}

func TestRollbackTarget(t *testing.T) {
	t.Parallel()
	old, cur := pkgRef("api-aaaaaa"), pkgRef("api-bbbbbb")
	tests := []struct {
		name            string
		fn              *fv1.Function
		previous        *fv1.PackageRef
		previousServing bool
		wantTarget      string
		wantGuard       bool
	}{
		{"recorded last ready package wins", function(cur, &old), nil, false, "api-aaaaaa", true},
		{"update that is not a package change is not guarded", function(cur, &cur), &old, true, "", false},
		{"no record yet: the package it served before", function(cur, nil), &old, true, "api-aaaaaa", true},
		{"no record and the old one was not serving: record only", function(cur, nil), &old, false, "", true},
		{"no record, no previous: record only", function(cur, nil), nil, false, "", true},
		{"no package at all", function(fv1.PackageRef{}, nil), &old, true, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			target, guard := rollbackTarget(tt.fn, tt.previous, tt.previousServing)
			assert.Equal(t, tt.wantGuard, guard)
			assert.Equal(t, tt.wantTarget, targetName(target))
		})
	}
}

func deployment(replicas int32, status appsv1.DeploymentStatus) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: deplName, Namespace: ns, Generation: 2,
			Annotations: map[string]string{"deployment.kubernetes.io/revision": "2"},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"functionName": fnName}},
		},
		Status: status,
	}
}

func podWith(cs apiv1.ContainerStatus) apiv1.Pod {
	return apiv1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "api-new", Namespace: ns,
			Labels: map[string]string{"functionName": fnName, appsv1.DefaultDeploymentUniqueLabelKey: "new"},
		},
		Status: apiv1.PodStatus{ContainerStatuses: []apiv1.ContainerStatus{cs}},
	}
}

func TestAssessRollout(t *testing.T) {
	t.Parallel()
	rolling := appsv1.DeploymentStatus{ObservedGeneration: 2, Replicas: 3, UpdatedReplicas: 1, AvailableReplicas: 2}
	tests := []struct {
		name string
		depl *appsv1.Deployment
		pods []apiv1.Pod
		want rolloutVerdict
	}{
		{"all updated and available", deployment(2, appsv1.DeploymentStatus{ObservedGeneration: 2, Replicas: 2, UpdatedReplicas: 2, AvailableReplicas: 2}), nil, rolloutReady},
		{"old controller view is not ready", deployment(2, appsv1.DeploymentStatus{ObservedGeneration: 1, Replicas: 2, UpdatedReplicas: 2, AvailableReplicas: 2}), nil, rolloutPending},
		{"new pod still starting", deployment(2, rolling), []apiv1.Pod{podWith(apiv1.ContainerStatus{Name: "fetcher"})}, rolloutPending},
		{"crash loop", deployment(2, rolling), []apiv1.Pod{podWith(apiv1.ContainerStatus{
			Name: "api", State: apiv1.ContainerState{Waiting: &apiv1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
		})}, rolloutFailed},
		{"image cannot be pulled (retried by the kubelet)", deployment(2, rolling), []apiv1.Pod{podWith(apiv1.ContainerStatus{
			Name: "api", State: apiv1.ContainerState{Waiting: &apiv1.ContainerStateWaiting{Reason: "ImagePullBackOff"}},
		})}, rolloutFailing},
		{"secret cache of a new node not synced yet", deployment(2, rolling), []apiv1.Pod{podWith(apiv1.ContainerStatus{
			Name: "fetcher", State: apiv1.ContainerState{Waiting: &apiv1.ContainerStateWaiting{
				Reason: "CreateContainerConfigError", Message: "failed to sync secret cache: timed out waiting for the condition"}},
		})}, rolloutFailing},
		{"invalid image name", deployment(2, rolling), []apiv1.Pod{podWith(apiv1.ContainerStatus{
			Name: "api", State: apiv1.ContainerState{Waiting: &apiv1.ContainerStateWaiting{Reason: "InvalidImageName"}},
		})}, rolloutFailed},
		{"restarting over and over", deployment(2, rolling), []apiv1.Pod{podWith(apiv1.ContainerStatus{Name: "api", RestartCount: crashRestarts})}, rolloutFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, _ := assessRollout(tt.depl, tt.pods)
			assert.Equal(t, tt.want, got)
		})
	}
}

func guardFixture(t *testing.T, fn *fv1.Function, pod apiv1.Pod, status appsv1.DeploymentStatus) (*NewDeploy, *fissionfake.Clientset) {
	t.Helper()
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
		Name: "api-new", Namespace: ns,
		Labels:      map[string]string{"functionName": fnName, appsv1.DefaultDeploymentUniqueLabelKey: "new"},
		Annotations: map[string]string{"deployment.kubernetes.io/revision": "2"},
	}}
	kube := k8sfake.NewClientset(deployment(2, status), rs, &pod)
	// NewSimpleClientset: NewClientset's field management has no schema for
	// Fission's CRDs and rejects the guard's merge patches (a real API
	// server applies them).
	fission := fissionfake.NewSimpleClientset(fn)
	return &NewDeploy{
		logger:           testr.New(t),
		fissionClient:    fission,
		kubernetesClient: kube,
		rolloutGuard:     rolloutGuardConfig{enabled: true, grace: time.Minute, confirm: time.Hour, poll: time.Millisecond},
	}, fission
}

func TestGuardRollsBackACrashLoopingPackage(t *testing.T) {
	t.Parallel()
	old, broken := pkgRef("api-aaaaaa"), pkgRef("api-bbbbbb")
	fn := function(broken, &old)
	deploy, fission := guardFixture(t, fn, podWith(apiv1.ContainerStatus{
		Name: "api", State: apiv1.ContainerState{Waiting: &apiv1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
	}), appsv1.DeploymentStatus{ObservedGeneration: 2, Replicas: 3, UpdatedReplicas: 1, AvailableReplicas: 2})

	deploy.guardRollout(t.Context(), ns, fnName, broken, &old, ns, deplName, time.Minute)

	got, err := fission.CoreV1().Functions(ns).Get(t.Context(), fnName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, old, got.Spec.Package.PackageRef, "back on the last ready package")
	assert.Equal(t, "api-bbbbbb", got.Annotations[AnnotationRolledBackFrom])
	assert.Contains(t, got.Annotations[AnnotationRolledBackReason], "CrashLoopBackOff")
}

func TestGuardRecordsAReadyPackage(t *testing.T) {
	t.Parallel()
	old, good := pkgRef("api-aaaaaa"), pkgRef("api-cccccc")
	fn := function(good, &old)
	deploy, fission := guardFixture(t, fn, podWith(apiv1.ContainerStatus{Name: "api", Ready: true}),
		appsv1.DeploymentStatus{ObservedGeneration: 2, Replicas: 2, UpdatedReplicas: 2, AvailableReplicas: 2})

	deploy.guardRollout(t.Context(), ns, fnName, good, &old, ns, deplName, time.Minute)

	got, err := fission.CoreV1().Functions(ns).Get(t.Context(), fnName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, good, got.Spec.Package.PackageRef)
	last := lastReadyPackage(got)
	require.NotNil(t, last)
	assert.Equal(t, good, *last)
}

func TestGuardStopsWhenTheFunctionMovedOn(t *testing.T) {
	t.Parallel()
	old, broken, newer := pkgRef("api-aaaaaa"), pkgRef("api-bbbbbb"), pkgRef("api-dddddd")
	fn := function(newer, &old) // a newer deploy already replaced the guarded one
	deploy, fission := guardFixture(t, fn, podWith(apiv1.ContainerStatus{
		Name: "api", State: apiv1.ContainerState{Waiting: &apiv1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
	}), appsv1.DeploymentStatus{ObservedGeneration: 2, Replicas: 3, UpdatedReplicas: 1, AvailableReplicas: 2})

	deploy.guardRollout(t.Context(), ns, fnName, broken, &old, ns, deplName, time.Minute)

	got, err := fission.CoreV1().Functions(ns).Get(t.Context(), fnName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, newer, got.Spec.Package.PackageRef, "never rolls back over a newer deploy")
}

func TestGuardRollsBackAfterTheGracePeriod(t *testing.T) {
	t.Parallel()
	old, slow := pkgRef("api-aaaaaa"), pkgRef("api-eeeeee")
	fn := function(slow, &old)
	deploy, fission := guardFixture(t, fn, podWith(apiv1.ContainerStatus{Name: "fetcher"}),
		appsv1.DeploymentStatus{ObservedGeneration: 2, Replicas: 3, UpdatedReplicas: 1, AvailableReplicas: 2})

	// grace already over: the first look that is still pending decides
	deploy.guardRollout(t.Context(), ns, fnName, slow, &old, ns, deplName, 0)

	got, err := fission.CoreV1().Functions(ns).Get(t.Context(), fnName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, old, got.Spec.Package.PackageRef)
	assert.Contains(t, got.Annotations[AnnotationRolledBackReason], "not available after")
}

func configErrorPod() apiv1.Pod {
	return podWith(apiv1.ContainerStatus{
		Name: "fetcher", State: apiv1.ContainerState{Waiting: &apiv1.ContainerStateWaiting{
			Reason: "CreateContainerConfigError", Message: "failed to sync secret cache: timed out waiting for the condition"}},
	})
}

// Prod 2026-11-04: one look at a new node's unsynced secret cache rolled a
// good package back. A retryable error is waited out for the confirm window.
func TestGuardWaitsOutARetryableError(t *testing.T) {
	t.Parallel()
	old, good := pkgRef("api-aaaaaa"), pkgRef("api-ffffff")
	fn := function(good, &old)
	deploy, fission := guardFixture(t, fn, configErrorPod(),
		appsv1.DeploymentStatus{ObservedGeneration: 2, Replicas: 3, UpdatedReplicas: 1, AvailableReplicas: 2})

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	deploy.guardRollout(ctx, ns, fnName, good, &old, ns, deplName, time.Hour)

	got, err := fission.CoreV1().Functions(ns).Get(t.Context(), fnName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, good, got.Spec.Package.PackageRef, "not rolled back inside the confirm window")
	assert.Empty(t, got.Annotations[AnnotationRolledBackFrom])
}

func TestGuardRollsBackARetryableErrorThatLasts(t *testing.T) {
	t.Parallel()
	old, broken := pkgRef("api-aaaaaa"), pkgRef("api-gggggg")
	fn := function(broken, &old)
	deploy, fission := guardFixture(t, fn, configErrorPod(),
		appsv1.DeploymentStatus{ObservedGeneration: 2, Replicas: 3, UpdatedReplicas: 1, AvailableReplicas: 2})
	deploy.rolloutGuard.confirm = 20 * time.Millisecond

	deploy.guardRollout(t.Context(), ns, fnName, broken, &old, ns, deplName, time.Hour)

	got, err := fission.CoreV1().Functions(ns).Get(t.Context(), fnName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, old, got.Spec.Package.PackageRef)
	assert.Contains(t, got.Annotations[AnnotationRolledBackReason], "CreateContainerConfigError")
	assert.Contains(t, got.Annotations[AnnotationRolledBackReason], "failed to sync secret cache")
}
