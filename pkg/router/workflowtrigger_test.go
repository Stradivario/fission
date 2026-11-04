// SPDX-FileCopyrightText: The Fission Authors
//
// SPDX-License-Identifier: Apache-2.0

package router

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	fv1 "github.com/fission/fission/pkg/apis/core/v1"
	fissionfake "github.com/fission/fission/pkg/generated/clientset/versioned/fake"
	"github.com/fission/fission/pkg/router/routetable"
	"github.com/fission/fission/pkg/workflowtrigger"
)

func workflowTrigger(name, ns, url, workflow string) *fv1.HTTPTrigger {
	return &fv1.HTTPTrigger{
		Name: name, Namespace: ns, Generation: 1, UID: types.UID("trig-" + name),
		Spec: fv1.HTTPTriggerSpec{
			RelativeURL: url,
			Methods:     []string{http.MethodPost},
			FunctionReference: fv1.FunctionReference{
				Type: fv1.FunctionReferenceTypeWorkflow, Name: workflow,
			},
		},
	}
}

// A workflow-targeting trigger is admitted without any function and every
// request creates a labelled WorkflowRun with the body as input, answered 202.
func TestWorkflowTriggerStartsRuns(t *testing.T) {
	trigger := workflowTrigger("orders", "myns", "/orders", "order-flow")
	ts, _ := newIncrementalTS(t, trigger)
	fc := fissionfake.NewSimpleClientset(trigger)
	ts.fissionClient = fc

	res, err := ts.applyTriggerIncremental(t.Context(), trigger)
	require.NoError(t, err)
	require.Equal(t, routetable.ShapeChanged, res, "a workflow trigger resolves without a function")
	requireSignal(t, ts)
	ts.materialize(t.Context())
	public, _ := muxes(ts)
	require.True(t, muxMatches(public, http.MethodPost, "/orders"))
	handler := public.Handler()

	t.Run("json body starts a run", func(t *testing.T) {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/orders", strings.NewReader(`{"orderId":42}`)))
		require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
		var body map[string]string
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		assert.Equal(t, "order-flow", body["workflow"])
		assert.True(t, strings.HasPrefix(body["run"], "order-flow-"))

		run, err := fc.CoreV1().WorkflowRuns("myns").Get(t.Context(), body["run"], metav1.GetOptions{})
		require.NoError(t, err)
		assert.Equal(t, "order-flow", run.Spec.WorkflowRef)
		assert.JSONEq(t, `{"orderId":42}`, string(run.Spec.Input.Raw))
		assert.Equal(t, workflowtrigger.KindHTTPTrigger, run.Labels[workflowtrigger.LabelTriggerKind])
		assert.Equal(t, "orders", run.Labels[workflowtrigger.LabelTriggerName])
	})

	t.Run("empty body is {}", func(t *testing.T) {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/orders", nil))
		require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
		var body map[string]string
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		run, err := fc.CoreV1().WorkflowRuns("myns").Get(t.Context(), body["run"], metav1.GetOptions{})
		require.NoError(t, err)
		assert.JSONEq(t, `{}`, string(run.Spec.Input.Raw))
	})

	t.Run("non-JSON body is a 400 and starts nothing", func(t *testing.T) {
		before, err := fc.CoreV1().WorkflowRuns("myns").List(t.Context(), metav1.ListOptions{})
		require.NoError(t, err)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/orders", strings.NewReader("not json")))
		assert.Equal(t, http.StatusBadRequest, rec.Code)
		after, err := fc.CoreV1().WorkflowRuns("myns").List(t.Context(), metav1.ListOptions{})
		require.NoError(t, err)
		assert.Len(t, after.Items, len(before.Items))
	})

	t.Run("too large is a 413", func(t *testing.T) {
		big := `"` + strings.Repeat("x", fv1.MaxWorkflowRunInputBytes) + `"`
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/orders", strings.NewReader(big)))
		assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
	})
}

// Without a Fission client (the informer-less test/router mode) the route
// answers 503 instead of panicking.
func TestWorkflowTriggerWithoutClient(t *testing.T) {
	trigger := workflowTrigger("orders", "myns", "/orders", "order-flow")
	ts, _ := newIncrementalTS(t, trigger)
	rec := httptest.NewRecorder()
	ts.workflowTriggerHandler(trigger).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/orders", nil))
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}
