// SPDX-FileCopyrightText: The Fission Authors
//
// SPDX-License-Identifier: Apache-2.0

package fetcher

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiv1 "k8s.io/api/core/v1"
)

// fakeRuntime is the runtime container as the watchdog sees it.
type fakeRuntime struct {
	mu          sync.Mutex
	restarts    int32
	down        bool
	failLoads   int // next N re-specializations fail
	loads       int
	readiness   []bool
	podReadErrs int
}

func (r *fakeRuntime) watchdog() *runtimeWatchdog {
	return &runtimeWatchdog{
		logger: logr.Discard(),
		restarts: func(context.Context) (int32, error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			if r.podReadErrs > 0 {
				r.podReadErrs--
				return 0, errors.New("apiserver unavailable")
			}
			return r.restarts, nil
		},
		alive: func(context.Context) bool {
			r.mu.Lock()
			defer r.mu.Unlock()
			return !r.down
		},
		respecialize: func(context.Context) error {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.loads++
			if r.failLoads > 0 {
				r.failLoads--
				return errors.New("subgraph not ready")
			}
			return nil
		},
		setReady: func(ready bool) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.readiness = append(r.readiness, ready)
		},
		probeEvery:   time.Second,
		confirmEvery: 2 * time.Second,
		suspectFor:   time.Minute,
		backstop:     time.Minute,
		retryMax:     30 * time.Second,
	}
}

func (r *fakeRuntime) set(f func(r *fakeRuntime)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f(r)
}

func (r *fakeRuntime) snapshot() (loads int, readiness []bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.loads, append([]bool(nil), r.readiness...)
}

// run starts the watchdog in the bubble and returns a stop func.
func (r *fakeRuntime) run(t *testing.T) func() {
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		r.watchdog().run(ctx, 0)
		close(done)
	}()
	return func() {
		cancel()
		<-done
	}
}

func TestRuntimeWatchdogRespecializesAfterRestart(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := &fakeRuntime{}
		stop := r.run(t)
		defer stop()

		// OOM kill: port refuses, then the container is back with a new count.
		time.Sleep(3 * time.Second)
		r.set(func(r *fakeRuntime) { r.down = true })
		time.Sleep(2 * time.Second)
		r.set(func(r *fakeRuntime) { r.down = false; r.restarts = 1 })
		time.Sleep(5 * time.Second)
		synctest.Wait()

		loads, readiness := r.snapshot()
		assert.Equal(t, 1, loads)
		assert.Equal(t, []bool{false, true}, readiness)

		// Nothing else happens while the runtime stays up.
		time.Sleep(5 * time.Minute)
		synctest.Wait()
		loads, _ = r.snapshot()
		assert.Equal(t, 1, loads)
	})
}

func TestRuntimeWatchdogIgnoresPortBlipWithoutRestart(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := &fakeRuntime{}
		stop := r.run(t)
		defer stop()

		r.set(func(r *fakeRuntime) { r.down = true })
		time.Sleep(2 * time.Second)
		r.set(func(r *fakeRuntime) { r.down = false })
		time.Sleep(3 * time.Minute)
		synctest.Wait()

		loads, readiness := r.snapshot()
		assert.Zero(t, loads)
		assert.Empty(t, readiness)
	})
}

func TestRuntimeWatchdogBackstopCatchesUnseenRestart(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := &fakeRuntime{}
		stop := r.run(t)
		defer stop()

		// Restarted between two port probes: never seen down.
		time.Sleep(10 * time.Second)
		r.set(func(r *fakeRuntime) { r.restarts = 1 })
		time.Sleep(30 * time.Second)
		synctest.Wait()
		loads, _ := r.snapshot()
		require.Zero(t, loads, "only the backstop can see it")

		time.Sleep(40 * time.Second)
		synctest.Wait()
		loads, readiness := r.snapshot()
		assert.Equal(t, 1, loads)
		assert.Equal(t, []bool{false, true}, readiness)
	})
}

func TestRuntimeWatchdogRetriesUntilSpecialized(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := &fakeRuntime{failLoads: 3, podReadErrs: 0}
		stop := r.run(t)
		defer stop()

		r.set(func(r *fakeRuntime) { r.down = true })
		time.Sleep(2 * time.Second)
		r.set(func(r *fakeRuntime) { r.down = false; r.restarts = 1 })

		// 1s + 2s + 4s of backoff, then the 4th load works.
		time.Sleep(4 * time.Second)
		synctest.Wait()
		_, readiness := r.snapshot()
		assert.Equal(t, []bool{false}, readiness, "unready while failing")

		time.Sleep(10 * time.Second)
		synctest.Wait()
		loads, readiness := r.snapshot()
		assert.Equal(t, 4, loads)
		assert.Equal(t, []bool{false, true}, readiness)
	})
}

func TestRuntimeWatchdogSurvivesPodReadErrors(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := &fakeRuntime{podReadErrs: 2}
		stop := r.run(t)
		defer stop()

		r.set(func(r *fakeRuntime) { r.down = true })
		time.Sleep(2 * time.Second)
		r.set(func(r *fakeRuntime) { r.down = false; r.restarts = 1 })
		time.Sleep(15 * time.Second)
		synctest.Wait()

		loads, readiness := r.snapshot()
		assert.Equal(t, 1, loads)
		assert.Equal(t, []bool{false, true}, readiness)
	})
}

func TestRuntimeRestartsIgnoresFetcher(t *testing.T) {
	t.Parallel()
	pod := &apiv1.Pod{Status: apiv1.PodStatus{ContainerStatuses: []apiv1.ContainerStatus{
		{Name: fetcherContainerName, RestartCount: 5},
		{Name: "nodejs-graphql", RestartCount: 1},
	}}}
	assert.Equal(t, int32(1), runtimeRestarts(pod))
}
