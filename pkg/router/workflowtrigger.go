// SPDX-FileCopyrightText: The Fission Authors
//
// SPDX-License-Identifier: Apache-2.0

package router

import (
	"encoding/json"
	"io"
	"net/http"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fv1 "github.com/fission/fission/pkg/apis/core/v1"
	"github.com/fission/fission/pkg/workflowtrigger"
)

// workflowTriggerHandler serves an HTTPTrigger whose functionref.type is
// "workflow": every request starts a WorkflowRun of the named Workflow in
// the trigger's namespace, with the request body (JSON, empty = {}) as the
// run input, and answers 202 {"run": ..., "workflow": ..., "namespace": ...}
// without waiting for the run. The run carries the trigger in its labels.
//
// Nothing is resolved at route-build time: a missing Workflow is reported
// by the run itself (the engine fails a run whose workflow never appears).
func (ts *HTTPTriggerSet) workflowTriggerHandler(trigger *fv1.HTTPTrigger) http.Handler {
	namespace := trigger.Namespace
	workflow := trigger.Spec.FunctionReference.Name
	name := trigger.Name
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ts.fissionClient == nil {
			writeWorkflowTriggerError(w, http.StatusServiceUnavailable, "workflow triggers need the router's Fission client")
			return
		}
		raw, err := io.ReadAll(io.LimitReader(r.Body, int64(fv1.MaxWorkflowRunInputBytes)+1))
		if err != nil {
			writeWorkflowTriggerError(w, http.StatusBadRequest, "reading the request body: "+err.Error())
			return
		}
		input, err := workflowtrigger.Input(raw)
		if err != nil {
			status := http.StatusBadRequest
			if len(raw) > fv1.MaxWorkflowRunInputBytes {
				status = http.StatusRequestEntityTooLarge
			}
			writeWorkflowTriggerError(w, status, err.Error())
			return
		}
		run := workflowtrigger.NewRun(namespace, workflow, input, workflowtrigger.KindHTTPTrigger, name)
		created, err := ts.fissionClient.CoreV1().WorkflowRuns(namespace).Create(r.Context(), run, metav1.CreateOptions{})
		if err != nil {
			status := http.StatusInternalServerError
			if apierrors.IsInvalid(err) || apierrors.IsBadRequest(err) {
				status = http.StatusBadRequest
			}
			ts.logger.Error(err, "starting workflow run", "trigger", name, "namespace", namespace, "workflow", workflow)
			writeWorkflowTriggerError(w, status, "starting the workflow run failed: "+err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"run":       created.Name,
			"workflow":  workflow,
			"namespace": namespace,
		})
	})
}

func writeWorkflowTriggerError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}
