// SPDX-FileCopyrightText: The Fission Authors
//
// SPDX-License-Identifier: Apache-2.0

package newdeploy

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiv1 "k8s.io/api/core/v1"

	fv1 "github.com/fission/fission/pkg/apis/core/v1"
)

func TestSpreadConstraint(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		annotations map[string]string
		wantKey     string
		wantWhen    apiv1.UnsatisfiableConstraintAction
	}{
		{"off", nil, "", ""},
		{"unknown value is ignored", map[string]string{AnnotationSpread: "racks"}, "", ""},
		{"machines, soft", map[string]string{AnnotationSpread: "machines"}, apiv1.LabelHostname, apiv1.ScheduleAnyway},
		{"machines, strict", map[string]string{AnnotationSpread: "machines", AnnotationSpreadStrict: "true"},
			apiv1.LabelHostname, apiv1.DoNotSchedule},
		{"zones", map[string]string{AnnotationSpread: "zones"}, apiv1.LabelTopologyZone, apiv1.ScheduleAnyway},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fn := &fv1.Function{}
			fn.UID = "uid-1"
			fn.Annotations = tc.annotations
			c := spreadConstraint(fn, "rev-1")
			if tc.wantKey == "" {
				assert.Nil(t, c)
				return
			}
			require.NotNil(t, c)
			assert.Equal(t, tc.wantKey, c.TopologyKey)
			assert.Equal(t, tc.wantWhen, c.WhenUnsatisfiable)
			assert.Equal(t, int32(1), c.MaxSkew)
			assert.Equal(t, map[string]string{fv1.FUNCTION_UID: "uid-1", LabelSpreadRevision: "rev-1"},
				c.LabelSelector.MatchLabels)
			// a virtual cluster cannot copy a pod with matchLabelKeys to its host
			assert.Empty(t, c.MatchLabelKeys)
			assert.Equal(t, apiv1.NodeInclusionPolicyHonor, *c.NodeTaintsPolicy)
			assert.Equal(t, apiv1.NodeInclusionPolicyHonor, *c.NodeAffinityPolicy)
		})
	}
}

// The function's constraint replaces an environment-wide one on the same key
// and keeps the others.
func TestApplySpreadReplacesSameKey(t *testing.T) {
	t.Parallel()
	fn := &fv1.Function{}
	fn.Annotations = map[string]string{AnnotationSpread: "machines", AnnotationSpreadStrict: "true"}
	template := &apiv1.PodTemplateSpec{Spec: apiv1.PodSpec{TopologySpreadConstraints: []apiv1.TopologySpreadConstraint{
		{TopologyKey: apiv1.LabelHostname, MaxSkew: 5, WhenUnsatisfiable: apiv1.ScheduleAnyway},
		{TopologyKey: apiv1.LabelTopologyZone, MaxSkew: 2},
	}}}
	require.NoError(t, applySpread(template, fn))
	spec := &template.Spec
	require.Len(t, spec.TopologySpreadConstraints, 2)
	assert.Equal(t, apiv1.LabelTopologyZone, spec.TopologySpreadConstraints[0].TopologyKey)
	assert.Equal(t, int32(1), spec.TopologySpreadConstraints[1].MaxSkew)
	assert.Equal(t, apiv1.DoNotSchedule, spec.TopologySpreadConstraints[1].WhenUnsatisfiable)

	untouched := &apiv1.PodTemplateSpec{}
	require.NoError(t, applySpread(untouched, &fv1.Function{}))
	assert.Nil(t, untouched.Spec.TopologySpreadConstraints)
	assert.Empty(t, untouched.Labels)
}

// The pods carry the revision the constraint selects on; it is stable for one
// template and changes with it, so a rollout spreads the new pods among
// themselves.
func TestApplySpreadRevision(t *testing.T) {
	t.Parallel()
	fn := &fv1.Function{}
	fn.UID = "uid-1"
	fn.Annotations = map[string]string{AnnotationSpread: "machines"}
	build := func(image string) *apiv1.PodTemplateSpec {
		template := &apiv1.PodTemplateSpec{Spec: apiv1.PodSpec{Containers: []apiv1.Container{{Name: "fn", Image: image}}}}
		template.Labels = map[string]string{"b": "2", "a": "1"}
		require.NoError(t, applySpread(template, fn))
		return template
	}
	first := build("img:1")
	rev := first.Labels[LabelSpreadRevision]
	require.NotEmpty(t, rev)
	assert.Equal(t, rev, first.Spec.TopologySpreadConstraints[0].LabelSelector.MatchLabels[LabelSpreadRevision])

	assert.Equal(t, rev, build("img:1").Labels[LabelSpreadRevision], "same template, same revision")
	assert.NotEqual(t, rev, build("img:2").Labels[LabelSpreadRevision], "new template, new revision")

	// applying it again (an update of an already built template) keeps it
	require.NoError(t, applySpread(first, fn))
	assert.Equal(t, rev, first.Labels[LabelSpreadRevision])
	assert.Len(t, first.Spec.TopologySpreadConstraints, 1)
}

// The built Deployment carries the constraint; without the annotation it has none.
func TestNewDeployPodSpecSpread(t *testing.T) {
	deploy := newTestNewDeploy(t)
	env := newTestNewDeployEnv()
	replicas := int32(3)

	build := func(fn *fv1.Function) []apiv1.TopologySpreadConstraint {
		d, err := deploy.getDeploymentSpec(t.Context(), fn, env, &replicas,
			"newdeploy-test-fn", "default", map[string]string{"app": "newdeploy-test"}, map[string]string{})
		require.NoError(t, err)
		return d.Spec.Template.Spec.TopologySpreadConstraints
	}

	assert.Empty(t, build(newTestNewDeployFunction()))

	fn := newTestNewDeployFunction()
	fn.Annotations = map[string]string{AnnotationSpread: "machines"}
	got := build(fn)
	require.Len(t, got, 1)
	assert.Equal(t, apiv1.LabelHostname, got[0].TopologyKey)
	assert.Equal(t, apiv1.ScheduleAnyway, got[0].WhenUnsatisfiable)
	assert.NotEmpty(t, got[0].LabelSelector.MatchLabels[LabelSpreadRevision])
}
