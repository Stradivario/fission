// SPDX-FileCopyrightText: The Fission Authors
//
// SPDX-License-Identifier: Apache-2.0

package newdeploy

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	apiv1 "k8s.io/api/core/v1"
	k8sErrs "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"

	fv1 "github.com/fission/fission/pkg/apis/core/v1"
	"github.com/fission/fission/pkg/eventhook"
)

// Automatic rollback of a failed package rollout (newdeploy).
//
// A Function whose spec.package.packageref moves to a new package gets a
// rollout guard: a background watch of the new ReplicaSet's pods. When they
// all become available the package is recorded as the Function's last ready
// package (AnnotationLastReadyPackage). When they crash-loop, cannot pull or
// create their containers, or are still not available after the grace
// period, the guard patches packageref back to the last ready package and
// fires function.deploy.rolledback.
//
// No loop: the target is always a package that was Ready, and an update that
// does not change the package (it IS the rollback, or a secret/env change)
// is not guarded. A build failure never gets here - the package must be
// built before the Deployment is touched (waitForBuild), so the old version
// keeps running. The guard only judges pod health: an app that starts and
// answers every request with an error looks Ready and is not rolled back.
const (
	// AnnotationLastReadyPackage is the JSON PackageRef of the newest
	// package whose rollout became available.
	AnnotationLastReadyPackage = "fission.io/last-ready-package"
	// AnnotationRolledBackFrom names the package a rollback moved away from.
	AnnotationRolledBackFrom = "fission.io/rolled-back-from"
	// AnnotationRolledBackReason says why.
	AnnotationRolledBackReason = "fission.io/rolled-back-reason"
	// AnnotationRolledBackAt is the RFC 3339 time of the rollback.
	AnnotationRolledBackAt = "fission.io/rolled-back-at"
)

// rolloutGuardConfig is read once at executor start.
type rolloutGuardConfig struct {
	enabled bool
	// grace is the longest a new package may take to become available
	// (raised to twice the function's specialization timeout when that is
	// longer - a slow cold start or a node being added is not a failure).
	grace time.Duration
	// confirm is how long a retryable container error (retryableReasons)
	// must last before it fails the rollout.
	confirm time.Duration
	poll    time.Duration
}

// rolloutGuardConfigFromEnv: AUTO_ROLLBACK_ENABLED (default true),
// AUTO_ROLLBACK_GRACE_SECONDS (default 180), AUTO_ROLLBACK_CONFIRM_SECONDS
// (default 60).
func rolloutGuardConfigFromEnv(logger logr.Logger) rolloutGuardConfig {
	cfg := rolloutGuardConfig{enabled: true, grace: 180 * time.Second, confirm: 60 * time.Second, poll: 5 * time.Second}
	if v := os.Getenv("AUTO_ROLLBACK_ENABLED"); v != "" {
		on, err := strconv.ParseBool(v)
		if err != nil {
			logger.Error(err, "failed to parse AUTO_ROLLBACK_ENABLED, keeping it on")
		} else {
			cfg.enabled = on
		}
	}
	if v := os.Getenv("AUTO_ROLLBACK_GRACE_SECONDS"); v != "" {
		s, err := strconv.Atoi(v)
		if err != nil || s <= 0 {
			logger.Error(err, "failed to parse AUTO_ROLLBACK_GRACE_SECONDS, keeping the default", "value", v)
		} else {
			cfg.grace = time.Duration(s) * time.Second
		}
	}
	if v := os.Getenv("AUTO_ROLLBACK_CONFIRM_SECONDS"); v != "" {
		s, err := strconv.Atoi(v)
		if err != nil || s < 0 {
			logger.Error(err, "failed to parse AUTO_ROLLBACK_CONFIRM_SECONDS, keeping the default", "value", v)
		} else {
			cfg.confirm = time.Duration(s) * time.Second
		}
	}
	return cfg
}

func (c rolloutGuardConfig) graceFor(fn *fv1.Function) time.Duration {
	twice := 2 * time.Duration(fn.Spec.InvokeStrategy.ExecutionStrategy.SpecializationTimeout) * time.Second
	return max(c.grace, twice)
}

// lastReadyPackage reads AnnotationLastReadyPackage; nil when absent or bad.
func lastReadyPackage(fn *fv1.Function) *fv1.PackageRef {
	raw := fn.Annotations[AnnotationLastReadyPackage]
	if raw == "" {
		return nil
	}
	var ref fv1.PackageRef
	if err := json.Unmarshal([]byte(raw), &ref); err != nil || ref.Name == "" {
		return nil
	}
	return &ref
}

func samePackage(a, b fv1.PackageRef) bool {
	return a.Name == b.Name && a.Namespace == b.Namespace
}

// rollbackTarget decides whether this update of fn is a package change worth
// guarding and what a failure goes back to.
//
//   - guard=false: the update does not move to a new package (it is a
//     rollback itself, or only secrets / env / environment changed).
//   - target=nil with guard=true: a new package with nothing known-good to
//     return to (first rollout ever) - the guard only records it on success.
//
// previous is the package the Function pointed at before this update (nil
// when unknown); previousServing says the Deployment was available on it,
// which makes it a valid target before any package was ever recorded.
func rollbackTarget(fn *fv1.Function, previous *fv1.PackageRef, previousServing bool) (target *fv1.PackageRef, guard bool) {
	current := fn.Spec.Package.PackageRef
	if current.Name == "" {
		return nil, false
	}
	if last := lastReadyPackage(fn); last != nil {
		if samePackage(*last, current) {
			return nil, false
		}
		return last, true
	}
	if previous != nil && previous.Name != "" && !samePackage(*previous, current) {
		if previousServing {
			p := *previous
			return &p, true
		}
		return nil, true
	}
	// No record and no known previous package: guard only to record it.
	return nil, true
}

// rolloutVerdict is one look at a rollout.
type rolloutVerdict int

const (
	rolloutPending rolloutVerdict = iota
	rolloutReady
	rolloutFailed
	// rolloutFailing: a retryable container error right now - it fails the
	// rollout only when it lasts rolloutGuardConfig.confirm.
	rolloutFailing
)

// containerFailureReasons can never fix themselves without a new spec.
var containerFailureReasons = map[string]bool{
	"CrashLoopBackOff": true,
	"InvalidImageName": true,
}

// retryableReasons are errors the kubelet retries on its own and that often
// clear: on a node that was just added the kubelet's secret / configmap cache
// is not synced yet ("failed to sync secret cache: timed out waiting for the
// condition" = CreateContainerConfigError), a registry hiccup is ErrImagePull.
// Prod 2026-11-04: a good graphql-server-lambdas package was rolled back on
// the FIRST such look, 3 s after its pod reached a new Karpenter node.
var retryableReasons = map[string]bool{
	"ErrImagePull":               true,
	"ImagePullBackOff":           true,
	"CreateContainerConfigError": true,
	"CreateContainerError":       true,
	"RunContainerError":          true,
}

// crashRestarts restarts of one container in the new pods mean a crash
// loop even before the kubelet says CrashLoopBackOff.
const crashRestarts = 3

// assessRollout judges the Deployment's current rollout from its status and
// the pods of its newest ReplicaSet.
func assessRollout(depl *appsv1.Deployment, newPods []apiv1.Pod) (rolloutVerdict, string) {
	desired := int32(1)
	if depl.Spec.Replicas != nil {
		desired = *depl.Spec.Replicas
	}
	st := depl.Status
	if st.ObservedGeneration >= depl.Generation &&
		st.UpdatedReplicas >= desired &&
		st.AvailableReplicas >= desired &&
		st.Replicas == st.UpdatedReplicas {
		return rolloutReady, ""
	}
	failing := ""
	for i := range newPods {
		pod := &newPods[i]
		statuses := append(append([]apiv1.ContainerStatus{}, pod.Status.InitContainerStatuses...), pod.Status.ContainerStatuses...)
		for _, cs := range statuses {
			if w := cs.State.Waiting; w != nil && containerFailureReasons[w.Reason] {
				return rolloutFailed, fmt.Sprintf("pod %s container %s: %s", pod.Name, cs.Name, w.Reason)
			}
			if cs.RestartCount >= crashRestarts {
				return rolloutFailed, fmt.Sprintf("pod %s container %s restarted %d times", pod.Name, cs.Name, cs.RestartCount)
			}
			if w := cs.State.Waiting; w != nil && retryableReasons[w.Reason] && failing == "" {
				failing = fmt.Sprintf("pod %s container %s: %s", pod.Name, cs.Name, w.Reason)
				if w.Message != "" {
					failing += " (" + w.Message + ")"
				}
			}
		}
	}
	if failing != "" {
		return rolloutFailing, failing
	}
	return rolloutPending, ""
}

// newRevisionPods are the pods of the Deployment's newest ReplicaSet.
func (deploy *NewDeploy) newRevisionPods(ctx context.Context, depl *appsv1.Deployment) ([]apiv1.Pod, error) {
	selector, err := metav1.LabelSelectorAsSelector(depl.Spec.Selector)
	if err != nil {
		return nil, err
	}
	rsList, err := deploy.kubernetesClient.AppsV1().ReplicaSets(depl.Namespace).List(ctx, metav1.ListOptions{LabelSelector: selector.String()})
	if err != nil {
		return nil, err
	}
	revision := depl.Annotations["deployment.kubernetes.io/revision"]
	hash := ""
	for _, rs := range rsList.Items {
		if revision != "" && rs.Annotations["deployment.kubernetes.io/revision"] == revision {
			hash = rs.Labels[appsv1.DefaultDeploymentUniqueLabelKey]
			break
		}
	}
	if hash == "" {
		return nil, nil // the controller has not created it yet
	}
	podSelector := labels.SelectorFromSet(labels.Set{appsv1.DefaultDeploymentUniqueLabelKey: hash})
	pods, err := deploy.kubernetesClient.CoreV1().Pods(depl.Namespace).List(ctx, metav1.ListOptions{LabelSelector: podSelector.String()})
	if err != nil {
		return nil, err
	}
	return pods.Items, nil
}

// startRolloutGuard runs guardRollout in the background (on the executor's
// lifetime context, not the reconcile's). A no-op when disabled, for a
// non-package change, or for a scale-to-zero Deployment (no pod to judge).
func (deploy *NewDeploy) startRolloutGuard(fn *fv1.Function, previous *fv1.PackageRef, previousServing bool, deplNs, deplName string, replicas int32) {
	if !deploy.rolloutGuard.enabled || replicas == 0 {
		return
	}
	target, guard := rollbackTarget(fn, previous, previousServing)
	if !guard {
		return
	}
	ctx := deploy.rootCtx
	if ctx == nil {
		ctx = context.Background()
	}
	guarded := fn.Spec.Package.PackageRef
	grace := deploy.rolloutGuard.graceFor(fn)
	deploy.logger.Info("guarding package rollout",
		"function", fn.Name, "namespace", fn.Namespace, "package", guarded.Name,
		"rollback_target", targetName(target), "grace", grace.String())
	go deploy.guardRollout(ctx, fn.Namespace, fn.Name, guarded, target, deplNs, deplName, grace)
}

func targetName(t *fv1.PackageRef) string {
	if t == nil {
		return ""
	}
	return t.Name
}

func (deploy *NewDeploy) guardRollout(ctx context.Context, fnNs, fnName string, guarded fv1.PackageRef, target *fv1.PackageRef,
	deplNs, deplName string, grace time.Duration) {
	logger := deploy.logger.WithValues("function", fnName, "namespace", fnNs, "package", guarded.Name)
	deadline := time.Now().Add(grace)
	var failingSince time.Time // first look of the current retryable error
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		fn, err := deploy.fissionClient.CoreV1().Functions(fnNs).Get(ctx, fnName, metav1.GetOptions{})
		if err != nil {
			if !k8sErrs.IsNotFound(err) {
				logger.Error(err, "rollout guard: reading the function failed, giving up")
			}
			return
		}
		if !samePackage(fn.Spec.Package.PackageRef, guarded) {
			logger.Info("rollout guard: the function moved to another package, stopping")
			return
		}
		depl, err := deploy.kubernetesClient.AppsV1().Deployments(deplNs).Get(ctx, deplName, metav1.GetOptions{})
		if err != nil {
			if !k8sErrs.IsNotFound(err) {
				logger.Error(err, "rollout guard: reading the deployment failed, giving up")
			}
			return
		}
		if depl.Spec.Replicas != nil && *depl.Spec.Replicas == 0 {
			return // scaled to zero meanwhile: nothing to judge
		}
		pods, err := deploy.newRevisionPods(ctx, depl)
		if err != nil {
			logger.Error(err, "rollout guard: listing the new pods failed")
		}
		verdict, reason := assessRollout(depl, pods)
		if verdict == rolloutFailing {
			if failingSince.IsZero() {
				failingSince = time.Now()
			}
			if lasted := time.Since(failingSince); lasted >= deploy.rolloutGuard.confirm {
				verdict, reason = rolloutFailed, fmt.Sprintf("%s for %s", reason, lasted.Round(time.Second))
			} else {
				verdict = rolloutPending
			}
		} else {
			failingSince = time.Time{}
		}
		if verdict == rolloutPending && time.Now().After(deadline) {
			verdict, reason = rolloutFailed, fmt.Sprintf("not available after %s", grace)
		}
		switch verdict {
		case rolloutReady:
			deploy.recordLastReady(ctx, fn, logger)
			return
		case rolloutFailed:
			deploy.rollback(ctx, fn, guarded, target, reason, logger)
			return
		}
		timer.Reset(deploy.rolloutGuard.poll)
	}
}

func (deploy *NewDeploy) recordLastReady(ctx context.Context, fn *fv1.Function, logger logr.Logger) {
	if last := lastReadyPackage(fn); last != nil && *last == fn.Spec.Package.PackageRef {
		return
	}
	ref, err := json.Marshal(fn.Spec.Package.PackageRef)
	if err != nil {
		return
	}
	patch, _ := json.Marshal(map[string]any{
		"metadata": map[string]any{"annotations": map[string]string{AnnotationLastReadyPackage: string(ref)}},
	})
	if _, err := deploy.fissionClient.CoreV1().Functions(fn.Namespace).Patch(ctx, fn.Name, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		logger.Error(err, "rollout guard: recording the last ready package failed")
		return
	}
	logger.Info("rollout guard: package is ready, recorded as last ready")
}

func (deploy *NewDeploy) rollback(ctx context.Context, fn *fv1.Function, failed fv1.PackageRef, target *fv1.PackageRef, reason string, logger logr.Logger) {
	if target == nil {
		logger.Info("rollout guard: rollout failed but there is no earlier ready package to go back to", "reason", reason)
		return
	}
	patch, _ := json.Marshal(map[string]any{
		"metadata": map[string]any{"annotations": map[string]string{
			AnnotationRolledBackFrom:   failed.Name,
			AnnotationRolledBackReason: reason,
			AnnotationRolledBackAt:     time.Now().UTC().Format(time.RFC3339),
		}},
		"spec": map[string]any{"package": map[string]any{"packageref": target}},
	})
	if _, err := deploy.fissionClient.CoreV1().Functions(fn.Namespace).Patch(ctx, fn.Name, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		logger.Error(err, "rollout guard: ROLLBACK FAILED - the function stays on the failing package", "reason", reason, "target", target.Name)
		return
	}
	logger.Info("rollout guard: rolled back to the last ready package", "reason", reason, "target", target.Name)
	deploy.deployEventHook.Send(ctx, eventhook.Event{
		Event:           eventhook.EventFunctionDeployRolledBack,
		Namespace:       fn.Namespace,
		Function:        fn.Name,
		Package:         failed.Name,
		RollbackPackage: target.Name,
		Reason:          reason,
	})
}
