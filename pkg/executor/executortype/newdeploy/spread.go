// SPDX-FileCopyrightText: The Fission Authors
//
// SPDX-License-Identifier: Apache-2.0

package newdeploy

import (
	apiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fv1 "github.com/fission/fission/pkg/apis/core/v1"
)

// Spreading a function's replicas across failure domains (Lambforge plan 33).
//
// The platform sets the annotations on the Function; the executor turns them
// into one topology spread constraint on the Deployment's pod template. The
// platform says WHAT to spread over (machines, later zones), Kubernetes
// decides WHERE - no node names anywhere.
//
//	lambforge.io/spread: machines        -> one replica per machine, as even as possible
//	lambforge.io/spread-strict: "true"   -> never two on one machine while another
//	                                        machine that can take it has fewer
//
// Soft (default) is ScheduleAnyway: it never blocks a scale-up; with more
// replicas than machines they share machines evenly. Strict is DoNotSchedule:
// a replica stays Pending rather than doubling up. Tainted / unreachable
// machines and machines the pod's affinity excludes do not count as places to
// spread to (nodeTaintsPolicy / nodeAffinityPolicy Honor), so a machine that
// is down does not keep a strict replica Pending.
const (
	AnnotationSpread       = fv1.FunctionSpreadAnnotation
	AnnotationSpreadStrict = fv1.FunctionSpreadStrictAnnotation
)

// spreadTopologyKeys maps the annotation's value to the node label it spreads over.
var spreadTopologyKeys = map[string]string{
	"machines": apiv1.LabelHostname,
	"zones":    apiv1.LabelTopologyZone,
}

// spreadConstraint is the constraint the Function asks for, or nil (no or an
// unknown value: nothing changes for the function).
func spreadConstraint(fn *fv1.Function) *apiv1.TopologySpreadConstraint {
	key, ok := spreadTopologyKeys[fn.Annotations[AnnotationSpread]]
	if !ok {
		return nil
	}
	when := apiv1.ScheduleAnyway
	if fn.Annotations[AnnotationSpreadStrict] == "true" {
		when = apiv1.DoNotSchedule
	}
	honor := apiv1.NodeInclusionPolicyHonor
	return &apiv1.TopologySpreadConstraint{
		MaxSkew:           1,
		TopologyKey:       key,
		WhenUnsatisfiable: when,
		LabelSelector: &metav1.LabelSelector{
			MatchLabels: map[string]string{fv1.FUNCTION_UID: string(fn.UID)},
		},
		// a rolling update spreads the new pods among themselves; the old
		// ones about to go do not count
		MatchLabelKeys:     []string{"pod-template-hash"},
		NodeAffinityPolicy: &honor,
		NodeTaintsPolicy:   &honor,
	}
}

// applySpread sets the function's spread constraint on the pod spec. A
// constraint on the same topology key from the environment's pod spec is
// replaced (the function's choice wins); others are kept.
func applySpread(spec *apiv1.PodSpec, fn *fv1.Function) {
	c := spreadConstraint(fn)
	if c == nil {
		return
	}
	kept := make([]apiv1.TopologySpreadConstraint, 0, len(spec.TopologySpreadConstraints)+1)
	for _, existing := range spec.TopologySpreadConstraints {
		if existing.TopologyKey != c.TopologyKey {
			kept = append(kept, existing)
		}
	}
	spec.TopologySpreadConstraints = append(kept, *c)
}

// spreadChanged reports whether an update changes the spread annotations.
func spreadChanged(oldFn, newFn *fv1.Function) bool {
	return oldFn.Annotations[AnnotationSpread] != newFn.Annotations[AnnotationSpread] ||
		oldFn.Annotations[AnnotationSpreadStrict] != newFn.Annotations[AnnotationSpreadStrict]
}
