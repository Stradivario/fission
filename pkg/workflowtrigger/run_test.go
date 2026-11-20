// SPDX-FileCopyrightText: The Fission Authors
//
// SPDX-License-Identifier: Apache-2.0

package workflowtrigger

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	fv1 "github.com/fission/fission/pkg/apis/core/v1"
)

func TestInput(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		raw     string
		want    string
		wantErr bool
	}{
		{name: "empty is an empty object", raw: "", want: "{}"},
		{name: "object", raw: `{"a":1}`, want: `{"a":1}`},
		{name: "any JSON value", raw: `[1,2]`, want: `[1,2]`},
		{name: "not JSON", raw: "hello", wantErr: true},
		{name: "over the limit", raw: `"` + strings.Repeat("x", fv1.MaxWorkflowRunInputBytes) + `"`, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := Input([]byte(tc.raw))
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.JSONEq(t, tc.want, string(got.Raw))
		})
	}
}

func TestNewRun(t *testing.T) {
	t.Parallel()
	input, err := Input([]byte(`{"x":1}`))
	require.NoError(t, err)
	a := NewRun("ns", "flow", input, KindHTTPTrigger, "hook")
	b := NewRun("ns", "flow", input, KindHTTPTrigger, "hook")
	assert.True(t, strings.HasPrefix(a.Name, "flow-"))
	assert.NotEqual(t, a.Name, b.Name, "every firing is its own run")
	assert.Equal(t, "flow", a.Spec.WorkflowRef)
	assert.Equal(t, map[string]string{LabelTriggerKind: KindHTTPTrigger, LabelTriggerName: "hook"}, a.Labels)
	assert.True(t, IsWorkflowTarget(fv1.FunctionReference{Type: fv1.FunctionReferenceTypeWorkflow, Name: "flow"}))
	assert.False(t, IsWorkflowTarget(fv1.FunctionReference{Type: fv1.FunctionReferenceTypeFunctionName, Name: "fn"}))
}
