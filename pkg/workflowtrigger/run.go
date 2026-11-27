// SPDX-FileCopyrightText: The Fission Authors
//
// SPDX-License-Identifier: Apache-2.0

// Package workflowtrigger builds the WorkflowRun an HTTPTrigger or
// TimeTrigger with a workflow target (functionref.type "workflow") starts.
// The router and the timer create it with their own clients; this package
// only shapes the object, so both start runs the same way the CLI does.
package workflowtrigger

import (
	"encoding/json"
	"fmt"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"

	fv1 "github.com/fission/fission/pkg/apis/core/v1"
)

const (
	// LabelTriggerKind / LabelTriggerName say which trigger started a run
	// ("httptrigger" / "timetrigger" and its name), so a run list can show
	// where a run came from.
	LabelTriggerKind = "fission.io/trigger-kind"
	LabelTriggerName = "fission.io/trigger-name"

	KindHTTPTrigger = "httptrigger"
	KindTimeTrigger = "timetrigger"
)

// IsWorkflowTarget reports whether a trigger's reference targets a workflow.
func IsWorkflowTarget(ref fv1.FunctionReference) bool {
	return ref.Type == fv1.FunctionReferenceTypeWorkflow
}

// Input validates a run input: empty means {}; otherwise it must be one
// JSON value of at most fv1.MaxWorkflowRunInputBytes.
func Input(raw []byte) (*apiextensionsv1.JSON, error) {
	if len(raw) == 0 {
		return &apiextensionsv1.JSON{Raw: []byte("{}")}, nil
	}
	if len(raw) > fv1.MaxWorkflowRunInputBytes {
		return nil, fmt.Errorf("run input is %d bytes, the limit is %d", len(raw), fv1.MaxWorkflowRunInputBytes)
	}
	if !json.Valid(raw) {
		return nil, fmt.Errorf("run input must be JSON")
	}
	return &apiextensionsv1.JSON{Raw: raw}, nil
}

// NewRun is the WorkflowRun a trigger starts: named like the CLI's runs
// (<workflow>-<5 random>), labelled with the trigger that started it.
func NewRun(namespace, workflow string, input *apiextensionsv1.JSON, triggerKind, triggerName string) *fv1.WorkflowRun {
	return &fv1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%s", workflow, rand.String(5)),
			Namespace: namespace,
			Labels: map[string]string{
				LabelTriggerKind: triggerKind,
				LabelTriggerName: triggerName,
			},
		},
		Spec: fv1.WorkflowRunSpec{
			WorkflowRef: workflow,
			Input:       input,
		},
	}
}
