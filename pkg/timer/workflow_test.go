// SPDX-FileCopyrightText: The Fission Authors
//
// SPDX-License-Identifier: Apache-2.0

package timer

import (
	"context"
	"errors"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	fv1 "github.com/fission/fission/pkg/apis/core/v1"
	"github.com/fission/fission/pkg/workflowtrigger"
)

func workflowTimeTrigger(input string) fv1.TimeTrigger {
	tt := fv1.TimeTrigger{}
	tt.Name, tt.Namespace = "nightly", "myns"
	tt.Spec.Cron = "@every 1h"
	tt.Spec.FunctionReference = fv1.FunctionReference{Type: fv1.FunctionReferenceTypeWorkflow, Name: "report"}
	if input != "" {
		tt.Spec.WorkflowInput = &apiextensionsv1.JSON{Raw: []byte(input)}
	}
	return tt
}

func TestStartWorkflowRun(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		input     string
		wantInput string
		wantRun   bool
	}{
		{name: "workflowInput is the run input", input: `{"day":"mon"}`, wantInput: `{"day":"mon"}`, wantRun: true},
		{name: "no workflowInput starts with {}", wantInput: `{}`, wantRun: true},
		{name: "invalid workflowInput starts nothing", input: `{nope`, wantRun: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			timer := MakeTimer(logr.Discard(), "")
			var got []*fv1.WorkflowRun
			timer.createRun = func(_ context.Context, run *fv1.WorkflowRun) error {
				got = append(got, run)
				return nil
			}
			timer.startWorkflowRun(workflowTimeTrigger(tc.input))
			if !tc.wantRun {
				assert.Empty(t, got)
				return
			}
			require.Len(t, got, 1)
			run := got[0]
			assert.Equal(t, "myns", run.Namespace)
			assert.Equal(t, "report", run.Spec.WorkflowRef)
			assert.JSONEq(t, tc.wantInput, string(run.Spec.Input.Raw))
			assert.Equal(t, workflowtrigger.KindTimeTrigger, run.Labels[workflowtrigger.LabelTriggerKind])
			assert.Equal(t, "nightly", run.Labels[workflowtrigger.LabelTriggerName])
		})
	}
}

// A failing create (RBAC, a missing namespace) is logged, never a panic in
// the cron goroutine; a timer without a creator only logs.
func TestStartWorkflowRunErrors(t *testing.T) {
	t.Parallel()
	timer := MakeTimer(logr.Discard(), "")
	timer.startWorkflowRun(workflowTimeTrigger(""))
	timer.createRun = func(context.Context, *fv1.WorkflowRun) error { return errors.New("forbidden") }
	timer.startWorkflowRun(workflowTimeTrigger(""))
}
