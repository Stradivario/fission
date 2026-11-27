// SPDX-FileCopyrightText: The Fission Authors
//
// SPDX-License-Identifier: Apache-2.0

package util

import (
	"errors"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	fv1 "github.com/fission/fission/pkg/apis/core/v1"
)

func TestRecordCreateFailure(t *testing.T) {
	t.Parallel()
	quota := errors.New(`services "newdeploy-dadadaddada" is forbidden: exceeded quota: compute-resources, requested: services=1, used: services=3, limited: services=3`)
	newFn := func() *fv1.Function {
		return &fv1.Function{ObjectMeta: metav1.ObjectMeta{
			Name: "dadadaddada", Namespace: "6ae62802e7551c6f257ab152", UID: "uid-1",
		}}
	}

	t.Run("a repeated failure updates one Warning Event on the Function", func(t *testing.T) {
		t.Parallel()
		client := fake.NewSimpleClientset()
		fn := newFn()
		RecordCreateFailure(t.Context(), client, logr.Discard(), fn, "service", quota)
		RecordCreateFailure(t.Context(), client, logr.Discard(), fn, "service", quota)

		list, err := client.CoreV1().Events(fn.Namespace).List(t.Context(), metav1.ListOptions{})
		require.NoError(t, err)
		require.Len(t, list.Items, 1)
		ev := list.Items[0]
		assert.Equal(t, int32(2), ev.Count)
		assert.Equal(t, corev1.EventTypeWarning, ev.Type)
		assert.Equal(t, CreateFailureReason, ev.Reason)
		assert.Equal(t, "Function", ev.InvolvedObject.Kind)
		assert.Equal(t, fn.Name, ev.InvolvedObject.Name)
		assert.Equal(t, fn.UID, ev.InvolvedObject.UID)
		assert.True(t, strings.HasPrefix(ev.Message, "could not create the function's service:"), ev.Message)
		assert.Contains(t, ev.Message, "exceeded quota")
	})

	t.Run("each object kind is its own Event", func(t *testing.T) {
		t.Parallel()
		client := fake.NewSimpleClientset()
		fn := newFn()
		RecordCreateFailure(t.Context(), client, logr.Discard(), fn, "service", quota)
		RecordCreateFailure(t.Context(), client, logr.Discard(), fn, "deployment", errors.New("boom"))
		list, err := client.CoreV1().Events(fn.Namespace).List(t.Context(), metav1.ListOptions{})
		require.NoError(t, err)
		assert.Len(t, list.Items, 2)
	})

	t.Run("nothing to report records nothing", func(t *testing.T) {
		t.Parallel()
		client := fake.NewSimpleClientset()
		fn := newFn()
		RecordCreateFailure(t.Context(), client, logr.Discard(), fn, "service", nil)
		RecordCreateFailure(t.Context(), nil, logr.Discard(), fn, "service", quota)
		list, err := client.CoreV1().Events(fn.Namespace).List(t.Context(), metav1.ListOptions{})
		require.NoError(t, err)
		assert.Empty(t, list.Items)
	})

	t.Run("a very long message is cut to the Event limit", func(t *testing.T) {
		t.Parallel()
		client := fake.NewSimpleClientset()
		fn := newFn()
		RecordCreateFailure(t.Context(), client, logr.Discard(), fn, "service", errors.New(strings.Repeat("x", 5000)))
		list, err := client.CoreV1().Events(fn.Namespace).List(t.Context(), metav1.ListOptions{})
		require.NoError(t, err)
		require.Len(t, list.Items, 1)
		assert.LessOrEqual(t, len(list.Items[0].Message), maxEventMessage)
	})
}

func TestCreateFailureEventName(t *testing.T) {
	t.Parallel()
	fn := &fv1.Function{ObjectMeta: metav1.ObjectMeta{Name: strings.Repeat("a", 300), UID: "u"}}
	a := createFailureEventName(fn, "service")
	assert.Equal(t, a, createFailureEventName(fn, "service"), "stable across retries")
	assert.LessOrEqual(t, len(a), 253, "a valid object name")
	assert.NotEqual(t, a, createFailureEventName(fn, "deployment"), "object kinds do not share an Event")
}
