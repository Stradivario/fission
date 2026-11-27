// SPDX-FileCopyrightText: The Fission Authors
//
// SPDX-License-Identifier: Apache-2.0

package utils

import (
	"strings"

	apiv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// DrainPathAnnotation opts an Environment into drain-until-idle (Lambforge,
// 2026-11-27): its runtime image serves this path and answers once no
// request is in flight, so the runtime container's preStop is an HTTP GET to
// it instead of a fixed sleep. Runtimes without the annotation keep the
// sleep - a failing HTTP preStop would kill the pod at once, without even
// the endpoint-propagation window.
const DrainPathAnnotation = "lambforge.io/drain-path"

// RuntimeDrainPath is the Environment's drain path, or "" (not opted in /
// not an absolute path).
func RuntimeDrainPath(annotations map[string]string) string {
	p := strings.TrimSpace(annotations[DrainPathAnnotation])
	if !strings.HasPrefix(p, "/") {
		return ""
	}
	return p
}

// RuntimeLifecycle is the runtime container's preStop: an HTTP GET to the
// drain path on the runtime port when the Environment opted in, otherwise
// DrainLifecycle's sleep.
func RuntimeLifecycle(gracePeriodSeconds int64, drainPath string, port int) *apiv1.Lifecycle {
	if drainPath == "" || gracePeriodSeconds <= 0 {
		return DrainLifecycle(gracePeriodSeconds)
	}
	return &apiv1.Lifecycle{
		PreStop: &apiv1.LifecycleHandler{
			HTTPGet: &apiv1.HTTPGetAction{
				Path: drainPath,
				Port: intstr.FromInt32(int32(port)),
			},
		},
	}
}

// DrainGrace is a draining newdeploy pod's grace period: long enough for a
// request that started just before termination to finish - the function
// timeout (the router cuts a call there anyway) plus 10 s - and never
// shorter than the environment's own window.
func DrainGrace(envGraceSeconds int64, functionTimeoutSeconds int) int64 {
	g := int64(functionTimeoutSeconds) + 10
	if functionTimeoutSeconds <= 0 || g < envGraceSeconds {
		return envGraceSeconds
	}
	return g
}

// DrainLifecycle returns a preStop lifecycle that keeps a terminating pod
// alive for the full grace period so the router/endpoints controller can
// drop it from rotation before the process is killed (connection draining,
// see https://github.com/kubernetes/kubernetes/issues/47576#issuecomment-308900172).
//
// It uses the kubelet-native sleep action (GA since Kubernetes 1.30) instead
// of `exec /bin/sleep`: distroless images (e.g. the fetcher) have no sleep
// binary, and an exec'd sleep spanning the whole grace window is always
// SIGKILLed at expiry, failing the hook on every termination.
//
// A non-positive grace returns nil: there is no drain window to hold open,
// so the hook is omitted entirely.
func DrainLifecycle(gracePeriodSeconds int64) *apiv1.Lifecycle {
	if gracePeriodSeconds <= 0 {
		return nil
	}
	return &apiv1.Lifecycle{
		PreStop: &apiv1.LifecycleHandler{
			Sleep: &apiv1.SleepAction{Seconds: gracePeriodSeconds},
		},
	}
}
