// SPDX-FileCopyrightText: The Fission Authors
//
// SPDX-License-Identifier: Apache-2.0

package fetcher

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fission/fission/pkg/utils"
)

func TestEnvSpecializeRetryDelay(t *testing.T) {
	t.Parallel()
	want := []time.Duration{
		25 * time.Millisecond, 50 * time.Millisecond, 100 * time.Millisecond,
		200 * time.Millisecond, 400 * time.Millisecond, 500 * time.Millisecond,
		500 * time.Millisecond,
	}
	for i, w := range want {
		assert.Equalf(t, w, envSpecializeRetryDelay(i), "attempt %d", i)
	}
	// Large attempt indices must stay capped (no overflow back below the cap).
	assert.Equal(t, 500*time.Millisecond, envSpecializeRetryDelay(1000))
	// The wait budget must cover the previous schedule's worst case
	// (sum of 500*2i ms for i in [0,29) ≈ 406s) so no environment that
	// previously had time to start is now cut off.
	assert.GreaterOrEqual(t, envSpecializeWaitBudget, 406*time.Second)
}

func TestPkgRetrySchedules(t *testing.T) {
	t.Parallel()
	var notFoundTotal time.Duration
	for _, d := range pkgNotFoundRetrySchedule {
		notFoundTotal += d
	}
	// The summed not-found window guards the apiserver create-then-get race;
	// it must cover the previous schedule's 500ms (50+100+150+200).
	assert.GreaterOrEqual(t, notFoundTotal, 500*time.Millisecond)
	// First retry must stay cheap — that's the point of the reshape.
	assert.LessOrEqual(t, pkgNotFoundRetrySchedule[0], 25*time.Millisecond)

	var dialTotal time.Duration
	for _, d := range pkgDialRetrySchedule {
		dialTotal += d
	}
	// The dial schedule exists for istio/envoy warm-up; keep it coarse
	// (≥ the previous 5s total).
	assert.GreaterOrEqual(t, dialTotal, 5*time.Second)
}

func TestArchiveDownloadRetrySchedule(t *testing.T) {
	t.Parallel()
	var total time.Duration
	for _, d := range archiveDownloadRetrySchedule {
		total += d
	}
	// Long enough to ride out a fresh node's DNS/CNI start-up, short enough
	// to stay well inside the executor's specialization timeouts.
	assert.GreaterOrEqual(t, total, 20*time.Second)
	assert.LessOrEqual(t, total, 60*time.Second)
}

func TestIsTransientDownloadError(t *testing.T) {
	t.Parallel()
	dnsFail := &url.Error{Op: "Get", URL: "http://storagesvc.fission/v1/archive", Err: &net.OpError{
		Op: "dial", Net: "tcp", Err: &net.DNSError{Err: "dial udp 10.96.0.10:53: connect: no route to host", Name: "storagesvc.fission"},
	}}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"dns lookup on a cold node", dnsFail, true},
		{"connection refused", &url.Error{Op: "Get", URL: "http://x", Err: &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}}, true},
		{"bare errno, not a network error", fmt.Errorf("copy: %w", syscall.ECONNRESET), false},
		{"connection reset (also mid-body)", &url.Error{Op: "Get", URL: "http://x", Err: &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}}, true},
		{"503", &utils.HTTPStatusError{StatusCode: 503, Status: "503 Service Unavailable"}, true},
		{"429", &utils.HTTPStatusError{StatusCode: 429, Status: "429 Too Many Requests"}, true},
		{"404 missing archive", &utils.HTTPStatusError{StatusCode: 404, Status: "404 Not Found"}, false},
		{"403", &utils.HTTPStatusError{StatusCode: 403, Status: "403 Forbidden"}, false},
		{"cancelled", fmt.Errorf("get: %w", context.Canceled), false},
		{"local file error", errors.New("open /packages/x: permission denied"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, c.want, isTransientDownloadError(c.err))
		})
	}
}

func TestDownloadWithRetry(t *testing.T) {
	t.Parallel()
	schedule := []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}

	serve := func(t *testing.T, statuses ...int) (string, *atomic.Int32) {
		t.Helper()
		var calls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			i := int(calls.Add(1)) - 1
			code := statuses[min(i, len(statuses)-1)]
			w.WriteHeader(code)
			if code == http.StatusOK {
				_, _ = w.Write([]byte("archive"))
			}
		}))
		t.Cleanup(srv.Close)
		return srv.URL, &calls
	}
	download := func(t *testing.T, target string) (error, int) {
		t.Helper()
		dir := t.TempDir()
		retries := 0
		err := downloadWithRetry(t.Context(), schedule, func(int, error) { retries++ }, func() error {
			return utils.DownloadUrlToRoot(t.Context(), http.DefaultClient, target, dir, "pkg")
		})
		return err, retries
	}

	t.Run("recovers from transient 5xx", func(t *testing.T) {
		t.Parallel()
		target, calls := serve(t, http.StatusServiceUnavailable, http.StatusServiceUnavailable, http.StatusOK)
		err, retries := download(t, target)
		require.NoError(t, err)
		assert.Equal(t, int32(3), calls.Load())
		assert.Equal(t, 2, retries)
	})

	t.Run("does not retry a missing archive", func(t *testing.T) {
		t.Parallel()
		target, calls := serve(t, http.StatusNotFound)
		err, retries := download(t, target)
		require.Error(t, err)
		assert.Equal(t, "404 Not Found", err.Error(), "message unchanged for callers and logs")
		assert.Equal(t, int32(1), calls.Load())
		assert.Zero(t, retries)
	})

	t.Run("gives up after the schedule", func(t *testing.T) {
		t.Parallel()
		target, calls := serve(t, http.StatusBadGateway)
		err, retries := download(t, target)
		require.Error(t, err)
		assert.Equal(t, int32(len(schedule)+1), calls.Load())
		assert.Equal(t, len(schedule), retries)
	})

	t.Run("retries connection refused", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.NotFoundHandler())
		target := srv.URL
		srv.Close() // nothing listens on the port any more
		err, retries := download(t, target)
		require.Error(t, err)
		assert.Equal(t, len(schedule), retries)
	})
}
