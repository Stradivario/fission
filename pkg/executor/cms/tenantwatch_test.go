// SPDX-FileCopyrightText: The Fission Authors
//
// SPDX-License-Identifier: Apache-2.0

package cms

import (
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	metafake "k8s.io/client-go/metadata/fake"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"

	fv1 "github.com/fission/fission/pkg/apis/core/v1"
	fscheme "github.com/fission/fission/pkg/generated/clientset/versioned/scheme"
)

func metaObj(kind, ns, name, rv string) *metav1.PartialObjectMetadata {
	return &metav1.PartialObjectMetadata{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: kind},
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, ResourceVersion: rv},
	}
}

func newMetaClient(t *testing.T, objs ...runtime.Object) *metafake.FakeMetadataClient {
	t.Helper()
	s := metafake.NewTestScheme()
	require.NoError(t, metav1.AddMetaToScheme(s))
	return metafake.NewSimpleMetadataClient(s, objs...)
}

// update bumps an object's ResourceVersion through the tracker, which feeds
// the informer's watch like a real content change would.
func update(t *testing.T, c *metafake.FakeMetadataClient, gvr schema.GroupVersionResource, obj *metav1.PartialObjectMetadata) {
	t.Helper()
	require.NoError(t, c.Tracker().Update(gvr, obj, obj.Namespace))
}

func expectEvent(t *testing.T, ch <-chan event.GenericEvent, ns, name string) {
	t.Helper()
	select {
	case e := <-ch:
		assert.Equal(t, ns, e.Object.GetNamespace())
		assert.Equal(t, name, e.Object.GetName())
	case <-time.After(5 * time.Second):
		t.Fatalf("no event for %s/%s", ns, name)
	}
}

func expectNoEvent(t *testing.T, ch <-chan event.GenericEvent) {
	t.Helper()
	select {
	case e := <-ch:
		t.Fatalf("unexpected event for %s/%s", e.Object.GetNamespace(), e.Object.GetName())
	case <-time.After(300 * time.Millisecond):
	}
}

// waitWatching sends updates for obj until the informer delivers one, so the
// test doesn't race the informer's initial list/watch.
func waitWatching(t *testing.T, c *metafake.FakeMetadataClient, gvr schema.GroupVersionResource,
	obj *metav1.PartialObjectMetadata, ch <-chan event.GenericEvent) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for i := 0; time.Now().Before(deadline); i++ {
		o := obj.DeepCopy()
		o.ResourceVersion = obj.ResourceVersion + "-warm" + string(rune('a'+i%26))
		update(t, c, gvr, o)
		select {
		case <-ch:
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
	t.Fatalf("watch on %s/%s never became live", obj.Namespace, obj.Name)
}

func TestTenantWatcherFollowsTenantSet(t *testing.T) {
	sec := metaObj("Secret", "tenant-a", "environment", "1")
	cm := metaObj("ConfigMap", "tenant-a", "settings", "1")
	other := metaObj("Secret", "not-a-tenant", "environment", "1")
	c := newMetaClient(t, sec, cm, other)
	w := newTenantWatcher(logr.Discard(), c, nil, nil)
	ctx := t.Context()

	w.sync(ctx, map[string]string{"tenant-a": "tenant-a"})
	assert.Equal(t, []string{"tenant-a"}, w.watchedNamespaces())
	waitWatching(t, c, secretGVR, sec, w.secretEvents)
	waitWatching(t, c, configMapGVR, cm, w.cmEvents)

	t.Run("secret content change in a tenant is delivered", func(t *testing.T) {
		update(t, c, secretGVR, metaObj("Secret", "tenant-a", "environment", "10"))
		expectEvent(t, w.secretEvents, "tenant-a", "environment")
	})

	t.Run("configmap content change in a tenant is delivered", func(t *testing.T) {
		update(t, c, configMapGVR, metaObj("ConfigMap", "tenant-a", "settings", "10"))
		expectEvent(t, w.cmEvents, "tenant-a", "settings")
	})

	t.Run("same resourceVersion is dropped", func(t *testing.T) {
		update(t, c, secretGVR, metaObj("Secret", "tenant-a", "environment", "10"))
		expectNoEvent(t, w.secretEvents)
	})

	t.Run("non-tenant namespace is not watched", func(t *testing.T) {
		update(t, c, secretGVR, metaObj("Secret", "not-a-tenant", "environment", "2"))
		expectNoEvent(t, w.secretEvents)
	})

	t.Run("offboarded tenant stops delivering", func(t *testing.T) {
		w.sync(ctx, map[string]string{})
		assert.Empty(t, w.watchedNamespaces())
		update(t, c, secretGVR, metaObj("Secret", "tenant-a", "environment", "11"))
		expectNoEvent(t, w.secretEvents)
	})

	t.Run("re-onboarded tenant delivers again", func(t *testing.T) {
		w.sync(ctx, map[string]string{"tenant-a": "tenant-a"})
		waitWatching(t, c, secretGVR, metaObj("Secret", "tenant-a", "environment", "12"), w.secretEvents)
	})

	w.sync(ctx, nil)
	assert.Empty(t, w.watchedNamespaces())
}

func TestTenantWatcherDesiredNamespaces(t *testing.T) {
	s := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(s))
	require.NoError(t, fscheme.AddToScheme(s))
	reader := crfake.NewClientBuilder().WithScheme(s).WithObjects(
		&fv1.FissionTenant{
			ObjectMeta: metav1.ObjectMeta{Name: "team-a"},
			Spec:       fv1.FissionTenantSpec{Namespace: "team-a"},
		},
		&fv1.FissionTenant{ObjectMeta: metav1.ObjectMeta{Name: "empty"}},
	).Build()
	w := newTenantWatcher(logr.Discard(), nil, reader, nil)

	desired, err := w.desiredNamespaces(t.Context())
	require.NoError(t, err)
	assert.Contains(t, desired, "team-a", "a FissionTenant is watched even before resolver-sync catches up")
	assert.NotContains(t, desired, "", "a tenant without a namespace is skipped")
}
