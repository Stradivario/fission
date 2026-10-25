// SPDX-FileCopyrightText: The Fission Authors
//
// SPDX-License-Identifier: Apache-2.0

package newdeploy

import (
	"testing"

	"github.com/stretchr/testify/assert"
	appsv1 "k8s.io/api/apps/v1"
	apiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Adopting a previous executor's Deployment must carry the scheduling of the
// current pod spec (runtime pod spec patch), or a newly added
// kubernetes.io/arch pin never reaches already-running functions.
func TestAdoptDeploymentCarriesScheduling(t *testing.T) {
	t.Parallel()
	existing := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "newdeploy-fn", Annotations: map[string]string{"executorInstanceId": "old"}},
		Spec: appsv1.DeploymentSpec{Template: apiv1.PodTemplateSpec{Spec: apiv1.PodSpec{
			Containers: []apiv1.Container{{Name: "nodejs", Image: "old"}},
			Volumes:    []apiv1.Volume{{Name: "kept"}},
		}}},
	}
	desired := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{"executorInstanceId": "new"}},
		Spec: appsv1.DeploymentSpec{Template: apiv1.PodTemplateSpec{Spec: apiv1.PodSpec{
			Containers:   []apiv1.Container{{Name: "nodejs", Image: "new"}},
			NodeSelector: map[string]string{"kubernetes.io/arch": "amd64"},
			Tolerations:  []apiv1.Toleration{{Key: "dedicated", Operator: apiv1.TolerationOpExists}},
		}}},
	}

	adoptDeployment(existing, desired)

	spec := existing.Spec.Template.Spec
	assert.Equal(t, "new", existing.Annotations["executorInstanceId"])
	assert.Equal(t, "new", spec.Containers[0].Image)
	assert.Equal(t, map[string]string{"kubernetes.io/arch": "amd64"}, spec.NodeSelector)
	assert.Equal(t, desired.Spec.Template.Spec.Tolerations, spec.Tolerations)
	assert.Equal(t, "kept", spec.Volumes[0].Name, "fields the executor does not own are untouched")
}
