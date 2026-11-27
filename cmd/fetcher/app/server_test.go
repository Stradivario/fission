// SPDX-FileCopyrightText: The Fission Authors
//
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
)

func TestSpecializeWithRetryKeepsTrying(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		ok := specializeWithRetry(t.Context(), logr.Discard(), func(context.Context) (int, error) {
			calls++
			if calls < 6 {
				return http.StatusInternalServerError, errors.New("subgraph graphql-server not ready")
			}
			return http.StatusAccepted, nil
		}, 2*time.Second, 30*time.Second)
		assert.True(t, ok)
		assert.Equal(t, 6, calls)
	})
}

func TestSpecializeWithRetryStopsWithContext(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		ok := specializeWithRetry(ctx, logr.Discard(), func(context.Context) (int, error) {
			return http.StatusInternalServerError, errors.New("down")
		}, 2*time.Second, 30*time.Second)
		assert.False(t, ok)
	})
}
