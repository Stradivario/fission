// SPDX-FileCopyrightText: The Fission Authors
//
// SPDX-License-Identifier: Apache-2.0

package httpretry

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fastOpts keeps backoff negligible so tests don't sleep for real seconds.
func fastOpts(retryMax int) Options {
	return Options{RetryMax: retryMax, RetryWaitMin: time.Millisecond, RetryWaitMax: 5 * time.Millisecond}
}

func newClient(opts Options) *http.Client {
	return &http.Client{Transport: New(http.DefaultTransport, opts)}
}

func TestRetriesThenSucceeds(t *testing.T) {
	t.Parallel()
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) < 3 {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	resp, err := newClient(fastOpts(3)).Get(srv.URL) //nolint:noctx
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, int32(3), attempts.Load(), "should have taken 3 attempts (2 retries)")
}

func TestGivesUpAfterRetryMax(t *testing.T) {
	t.Parallel()
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	resp, err := newClient(fastOpts(2)).Get(srv.URL) //nolint:noctx
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode, "final 5xx is passed through")
	assert.Equal(t, int32(3), attempts.Load(), "1 initial + 2 retries")
}

func TestNoRetryOnSuccessOr4xx(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		code int
	}{
		{"200", http.StatusOK},
		{"400", http.StatusBadRequest},
		{"404", http.StatusNotFound},
		{"501", http.StatusNotImplemented}, // 501 is explicitly non-retryable
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var attempts atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				attempts.Add(1)
				w.WriteHeader(tc.code)
			}))
			defer srv.Close()

			resp, err := newClient(fastOpts(3)).Get(srv.URL) //nolint:noctx
			require.NoError(t, err)
			defer resp.Body.Close()
			assert.Equal(t, tc.code, resp.StatusCode)
			assert.Equal(t, int32(1), attempts.Load(), "must not retry")
		})
	}
}

func TestRetriesOn429(t *testing.T) {
	t.Parallel()
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) < 2 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	resp, err := newClient(fastOpts(3)).Get(srv.URL) //nolint:noctx
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, int32(2), attempts.Load())
}

// TestNoRetryOn429SkipsRetry pins the executor client's opt-out: a 429 must
// propagate on the first attempt when NoRetryOn429 is set, so deliberate
// back-pressure (the per-function concurrency limit, the per-environment pod
// cap) fast-fails instead of being retried and eventually surfaced as a
// generic "giving up" error that erases the original 429.
func TestNoRetryOn429SkipsRetry(t *testing.T) {
	t.Parallel()
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	opts := fastOpts(3)
	opts.NoRetryOn429 = true
	resp, err := newClient(opts).Get(srv.URL) //nolint:noctx
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusTooManyRequests, resp.StatusCode)
	assert.Equal(t, int32(1), attempts.Load(), "429 must not be retried when NoRetryOn429 is set")
}

// TestNoRetryOn429StillRetries5xx confirms NoRetryOn429 is scoped to 429 only:
// 5xx responses still retry per the default policy.
func TestNoRetryOn429StillRetries5xx(t *testing.T) {
	t.Parallel()
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) < 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	opts := fastOpts(3)
	opts.NoRetryOn429 = true
	resp, err := newClient(opts).Get(srv.URL) //nolint:noctx
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, int32(2), attempts.Load(), "NoRetryOn429 must not suppress the 5xx retry")
}

// TestRewindsBodyEachAttempt is the load-bearing case: the HMAC signer reads
// the body to sign it, so every retried attempt must see the full body again.
func TestRewindsBodyEachAttempt(t *testing.T) {
	t.Parallel()
	const payload = "hello-body"
	var seen []string
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		seen = append(seen, string(b))
		if attempts.Add(1) < 3 {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	resp, err := newClient(fastOpts(3)).Post(srv.URL, "text/plain", strings.NewReader(payload)) //nolint:noctx
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	require.Len(t, seen, 3)
	for i, body := range seen {
		assert.Equal(t, payload, body, "attempt %d must receive the full body", i+1)
	}
}

func TestRetryMaxZeroMakesSingleAttempt(t *testing.T) {
	t.Parallel()
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	resp, err := newClient(fastOpts(0)).Get(srv.URL) //nolint:noctx
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, int32(1), attempts.Load(), "RetryMax=0 disables retries")
}

func TestContextCancellationStopsRetries(t *testing.T) {
	t.Parallel()
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	// Long backoff so cancellation, not RetryMax, ends the loop.
	client := &http.Client{Transport: New(http.DefaultTransport, Options{RetryMax: 5, RetryWaitMin: time.Second, RetryWaitMax: time.Second})}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, http.NoBody)
	require.NoError(t, err)
	_, err = client.Do(req)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}
