// SPDX-FileCopyrightText: The Fission Authors
//
// SPDX-License-Identifier: Apache-2.0

package timetrigger

import (
	"fmt"
	"os"
	"strings"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	"github.com/fission/fission/pkg/fission-cli/cliwrapper/cli"
	flagkey "github.com/fission/fission/pkg/fission-cli/flag/key"
	"github.com/fission/fission/pkg/workflowtrigger"
)

// readWorkflowInput parses --workflow-input: inline JSON or @path; empty =
// nil (the timer then starts runs with {}).
func readWorkflowInput(input cli.Input) (*apiextensionsv1.JSON, error) {
	raw := input.String(flagkey.TriggerWorkflowInput)
	if raw == "" {
		return nil, nil
	}
	data := []byte(raw)
	if strings.HasPrefix(raw, "@") {
		var err error
		data, err = os.ReadFile(strings.TrimPrefix(raw, "@"))
		if err != nil {
			return nil, fmt.Errorf("reading --workflow-input file: %w", err)
		}
	}
	value, err := workflowtrigger.Input(data)
	if err != nil {
		return nil, fmt.Errorf("--workflow-input: %w", err)
	}
	return value, nil
}
