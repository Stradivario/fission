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
			c := spreadConstraint(fn)
			if tc.wantKey == "" {
				assert.Nil(t, c)
				return
			}
			require.NotNil(t, c)
			assert.Equal(t, tc.wantKey, c.TopologyKey)
			assert.Equal(t, tc.wantWhen, c.WhenUnsatisfiable)
			assert.Equal(t, int32(1), c.MaxSkew)
			assert.Equal(t, map[string]string{fv1.FUNCTION_UID: "uid-1"}, c.LabelSelector.MatchLabels)
			assert.Equal(t, []string{"pod-template-hash"}, c.MatchLabelKeys)
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
	spec := &apiv1.PodSpec{TopologySpreadConstraints: []apiv1.TopologySpreadConstraint{
		{TopologyKey: apiv1.LabelHostname, MaxSkew: 5, WhenUnsatisfiable: apiv1.ScheduleAnyway},
		{TopologyKey: apiv1.LabelTopologyZone, MaxSkew: 2},
	}}
	applySpread(spec, fn)
	require.Len(t, spec.TopologySpreadConstraints, 2)
	assert.Equal(t, apiv1.LabelTopologyZone, spec.TopologySpreadConstraints[0].TopologyKey)
	assert.Equal(t, int32(1), spec.TopologySpreadConstraints[1].MaxSkew)
	assert.Equal(t, apiv1.DoNotSchedule, spec.TopologySpreadConstraints[1].WhenUnsatisfiable)

	untouched := &apiv1.PodSpec{}
	applySpread(untouched, &fv1.Function{})
	assert.Nil(t, untouched.TopologySpreadConstraints)
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
}
