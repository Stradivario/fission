// SPDX-FileCopyrightText: The Fission Authors
//
// SPDX-License-Identifier: Apache-2.0

package buildermgr

import (
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"

	fv1 "github.com/fission/fission/pkg/apis/core/v1"
)

// An annotation-only patch (Lambforge's fission.io/force-update on resume)
// bumps ResourceVersion, which the builder object names embed - the
// reconciler must see it, or the package reconciler waits for a builder
// named after the new ResourceVersion forever.
func TestEnvironmentPredicate(t *testing.T) {
	t.Parallel()
	env := func(generation int64, rv string, annotations map[string]string) *fv1.Environment {
		return &fv1.Environment{ObjectMeta: metav1.ObjectMeta{
			Name: "nodejs", Namespace: "p1", Generation: generation,
			ResourceVersion: rv, Annotations: annotations,
		}}
	}
	cases := []struct {
		name     string
		old, new *fv1.Environment
		want     bool
	}{
		{"spec change", env(1, "10", nil), env(2, "11", nil), true},
		{"force-update annotation", env(2, "11", nil), env(2, "12", map[string]string{"fission.io/force-update": "1"}), true},
		{"label-only edit", env(2, "12", nil), func() *fv1.Environment {
			e := env(2, "13", nil)
			e.Labels = map[string]string{"team": "a"}
			return e
		}(), true},
		{"status-only write", env(2, "12", nil), env(2, "13", nil), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := environmentPredicate().Update(event.UpdateEvent{ObjectOld: c.old, ObjectNew: c.new})
			assert.Equal(t, c.want, got)
		})
	}
}
