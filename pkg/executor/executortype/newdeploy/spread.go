// SPDX-FileCopyrightText: The Fission Authors
//
// SPDX-License-Identifier: Apache-2.0

package newdeploy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

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
//
// A rolling update must spread the NEW pods among themselves: the old ones
// about to go must not count, or the last pods land wherever the old ones
// happened to leave a gap. Kubernetes has matchLabelKeys: [pod-template-hash]
// for that, but a virtual cluster cannot run it - its API server merges the
// key into the label selector (Kubernetes >= 1.34) and the copy of the pod on
// the host is then refused ("exists in both matchLabelKeys and
// labelSelector", seen 2026-11-17 with vCluster 0.37 on Kubernetes 1.35). So
// the executor does the same thing itself: the pod template carries a label
// with a hash of the template (LabelSpreadRevision) and the constraint
// selects on it. It changes exactly when the template changes.
const (
	AnnotationSpread       = fv1.FunctionSpreadAnnotation
	AnnotationSpreadStrict = fv1.FunctionSpreadStrictAnnotation
	// LabelSpreadRevision is the pod label a spread constraint selects on.
	LabelSpreadRevision = "lambforge.io/spread-rev"
)

// spreadTopologyKeys maps the annotation's value to the node label it spreads over.
var spreadTopologyKeys = map[string]string{
	"machines": apiv1.LabelHostname,
	"zones":    apiv1.LabelTopologyZone,
}

// spreadConstraint is the constraint the Function asks for, selecting the
// pods of one template revision, or nil (no or an unknown value: nothing
// changes for the function).
func spreadConstraint(fn *fv1.Function, revision string) *apiv1.TopologySpreadConstraint {
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
			MatchLabels: map[string]string{
				fv1.FUNCTION_UID:    string(fn.UID),
				LabelSpreadRevision: revision,
			},
		},
		NodeAffinityPolicy: &honor,
		NodeTaintsPolicy:   &honor,
	}
}

// spreadRevision is a short, stable hash of a pod template (encoding/json
// writes map keys sorted).
func spreadRevision(template *apiv1.PodTemplateSpec) (string, error) {
	raw, err := json.Marshal(template)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])[:12], nil
}

// applySpread sets the function's spread constraint on the pod template and
// labels the template with its revision. It must run LAST, when the template
// is complete, so the revision covers everything. A constraint on the same
// topology key from the environment's pod spec is replaced (the function's
// choice wins); others are kept.
func applySpread(template *apiv1.PodTemplateSpec, fn *fv1.Function) error {
	c := spreadConstraint(fn, "")
	if c == nil {
		return nil
	}
	// the revision covers the template WITHOUT what this function adds, so
	// applying it twice gives the same result
	delete(template.Labels, LabelSpreadRevision)
	spec := &template.Spec
	kept := make([]apiv1.TopologySpreadConstraint, 0, len(spec.TopologySpreadConstraints)+1)
	for _, existing := range spec.TopologySpreadConstraints {
		if existing.TopologyKey != c.TopologyKey {
			kept = append(kept, existing)
		}
	}
	spec.TopologySpreadConstraints = kept
	if len(kept) == 0 {
		spec.TopologySpreadConstraints = nil
	}
	revision, err := spreadRevision(template)
	if err != nil {
		return err
	}
	if template.Labels == nil {
		template.Labels = map[string]string{}
	}
	template.Labels[LabelSpreadRevision] = revision
	spec.TopologySpreadConstraints = append(kept, *spreadConstraint(fn, revision))
	return nil
}

// spreadChanged reports whether an update changes the spread annotations.
func spreadChanged(oldFn, newFn *fv1.Function) bool {
	return oldFn.Annotations[AnnotationSpread] != newFn.Annotations[AnnotationSpread] ||
		oldFn.Annotations[AnnotationSpreadStrict] != newFn.Annotations[AnnotationSpreadStrict]
}
