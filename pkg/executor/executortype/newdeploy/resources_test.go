// SPDX-FileCopyrightText: The Fission Authors
//
// SPDX-License-Identifier: Apache-2.0

package newdeploy

import (
	"testing"

	"github.com/stretchr/testify/assert"
	apiv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	fv1 "github.com/fission/fission/pkg/apis/core/v1"
)

// getResources must not write a function's overrides into the shared (cached)
// Environment: that raced concurrent function creations ("concurrent map read
// and map write" in the executor) and leaked one function's limits into the
// next function's Deployment.
func TestGetResourcesDoesNotMutateEnvironment(t *testing.T) {
	t.Parallel()
	env := &fv1.Environment{}
	env.Spec.Resources = apiv1.ResourceRequirements{
		Limits:   apiv1.ResourceList{apiv1.ResourceMemory: resource.MustParse("256Mi")},
		Requests: apiv1.ResourceList{apiv1.ResourceCPU: resource.MustParse("100m")},
	}
	want := env.Spec.Resources.DeepCopy()

	big := &fv1.Function{}
	big.Spec.Resources.Limits = apiv1.ResourceList{apiv1.ResourceMemory: resource.MustParse("2Gi")}
	plain := &fv1.Function{}

	deploy := &NewDeploy{}
	gotBig := deploy.getResources(env, big)
	gotPlain := deploy.getResources(env, plain)

	assert.Equal(t, *want, env.Spec.Resources, "environment resources mutated")
	assert.True(t, gotBig.Limits.Memory().Equal(resource.MustParse("2Gi")), "function override applied")
	assert.True(t, gotPlain.Limits.Memory().Equal(resource.MustParse("256Mi")), "override leaked into another function")
}
