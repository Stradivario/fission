// SPDX-FileCopyrightText: The Fission Authors
//
// SPDX-License-Identifier: Apache-2.0

package utils

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDrainLifecycle(t *testing.T) {
	t.Parallel()

	t.Run("positive grace returns native sleep preStop", func(t *testing.T) {
		t.Parallel()
		lc := DrainLifecycle(360)
		require.NotNil(t, lc)
		require.NotNil(t, lc.PreStop)
		require.NotNil(t, lc.PreStop.Sleep)
		assert.Nil(t, lc.PreStop.Exec)
		assert.Equal(t, int64(360), lc.PreStop.Sleep.Seconds)
	})

	t.Run("zero grace returns nil", func(t *testing.T) {
		t.Parallel()
		assert.Nil(t, DrainLifecycle(0))
	})

	t.Run("negative grace returns nil", func(t *testing.T) {
		t.Parallel()
		assert.Nil(t, DrainLifecycle(-1))
	})
}

func TestRuntimeDrain(t *testing.T) {
	t.Parallel()

	t.Run("drain path from the annotation, absolute paths only", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, "/__drain", RuntimeDrainPath(map[string]string{DrainPathAnnotation: " /__drain "}))
		assert.Empty(t, RuntimeDrainPath(map[string]string{DrainPathAnnotation: "__drain"}))
		assert.Empty(t, RuntimeDrainPath(nil))
	})

	t.Run("opted in: HTTP GET to the runtime port", func(t *testing.T) {
		t.Parallel()
		lc := RuntimeLifecycle(15, "/__drain", 8888)
		require.NotNil(t, lc)
		require.NotNil(t, lc.PreStop.HTTPGet)
		assert.Nil(t, lc.PreStop.Sleep)
		assert.Equal(t, "/__drain", lc.PreStop.HTTPGet.Path)
		assert.Equal(t, int32(8888), lc.PreStop.HTTPGet.Port.IntVal)
	})

	t.Run("not opted in or no window: today's sleep (or none)", func(t *testing.T) {
		t.Parallel()
		lc := RuntimeLifecycle(15, "", 8888)
		require.NotNil(t, lc)
		assert.Equal(t, int64(15), lc.PreStop.Sleep.Seconds)
		assert.Nil(t, RuntimeLifecycle(0, "/__drain", 8888))
	})

	t.Run("grace: function timeout + 10 s, never below the env window", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, int64(70), DrainGrace(15, 60))
		assert.Equal(t, int64(90), DrainGrace(90, 60))
		assert.Equal(t, int64(15), DrainGrace(15, 0))
		assert.Equal(t, int64(310), DrainGrace(15, 300))
	})
}
