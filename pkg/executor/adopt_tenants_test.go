// SPDX-FileCopyrightText: The Fission Authors
//
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
)

// The adopt + cleanup pass must not run against a partially loaded tenant set:
// the reaper would delete every Deployment adopt did not re-stamp.
func TestSyncTenantsBeforeAdopt(t *testing.T) {
	t.Parallel()

	t.Run("no tenancy sync configured runs the pass", func(t *testing.T) {
		t.Parallel()
		assert.True(t, syncTenantsBeforeAdopt(t.Context(), logr.Discard(), nil, time.Second, time.Millisecond))
	})

	t.Run("transient failures are retried", func(t *testing.T) {
		t.Parallel()
		calls := 0
		sync := func(context.Context) error {
			calls++
			if calls < 3 {
				return errors.New("apiserver unavailable")
			}
			return nil
		}
		assert.True(t, syncTenantsBeforeAdopt(t.Context(), logr.Discard(), sync, 5*time.Second, time.Millisecond))
		assert.Equal(t, 3, calls)
	})

	t.Run("persistent failure skips the pass", func(t *testing.T) {
		t.Parallel()
		sync := func(context.Context) error { return errors.New("forbidden") }
		assert.False(t, syncTenantsBeforeAdopt(t.Context(), logr.Discard(), sync, 50*time.Millisecond, 5*time.Millisecond))
	})
}
