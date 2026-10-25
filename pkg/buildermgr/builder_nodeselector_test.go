// SPDX-FileCopyrightText: The Fission Authors
//
// SPDX-License-Identifier: Apache-2.0

package buildermgr

import (
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	apiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// Existing builders get the pod spec patch's nodeSelector (they are otherwise
// only rebuilt on an Environment change); keys the builder sets are kept.
func TestSyncBuilderNodeSelector(t *testing.T) {
	t.Parallel()
	builder := func(sel map[string]string) *appsv1.Deployment {
		return &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "nodejs-123", Namespace: "tenant"},
			Spec: appsv1.DeploymentSpec{Template: apiv1.PodTemplateSpec{Spec: apiv1.PodSpec{
				NodeSelector: sel,
				Containers:   []apiv1.Container{{Name: "builder"}},
			}}},
		}
	}
	patch := &apiv1.PodSpec{NodeSelector: map[string]string{"kubernetes.io/arch": "amd64"}}

	cases := []struct {
		name  string
		patch *apiv1.PodSpec
		have  map[string]string
		want  map[string]string
	}{
		{"adds the missing pin", patch, nil, map[string]string{"kubernetes.io/arch": "amd64"}},
		{"keeps other keys", patch, map[string]string{"pool": "build"}, map[string]string{"pool": "build", "kubernetes.io/arch": "amd64"}},
		{"builder's own value wins", patch, map[string]string{"kubernetes.io/arch": "arm64"}, map[string]string{"kubernetes.io/arch": "arm64"}},
		{"no patch is a no-op", nil, map[string]string{"pool": "build"}, map[string]string{"pool": "build"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			d := builder(c.have)
			kc := fake.NewClientset(d)
			r := &EnvironmentReconciler{logger: logr.Discard(), kubernetesClient: kc, podSpecPatch: c.patch}
			require.NoError(t, r.syncBuilderNodeSelector(t.Context(), d))
			got, err := kc.AppsV1().Deployments("tenant").Get(t.Context(), "nodejs-123", metav1.GetOptions{})
			require.NoError(t, err)
			assert.Equal(t, c.want, got.Spec.Template.Spec.NodeSelector)
		})
	}
}
