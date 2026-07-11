// SPDX-FileCopyrightText: The Fission Authors
//
// SPDX-License-Identifier: Apache-2.0

package router

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/fission/fission/pkg/utils/loggerfactory"
)

// testUnresolvedRetries/testUnresolvedRetryInterval keep the unresolved-caller
// test cases fast and deterministic instead of eating the real ~250ms default
// retry budget (defaultUnresolvedRetries * defaultUnresolvedRetryInterval).
const (
	testUnresolvedRetries       = 1
	testUnresolvedRetryInterval = time.Millisecond
)

// mapLookup is a static callerNamespaceLookup for tests: ip -> namespace.
type mapLookup map[string]string

func (m mapLookup) lookup(ip string) (string, bool) {
	ns, ok := m[ip]
	return ns, ok
}

func TestSameNamespaceGuard(t *testing.T) {
	const installNS = "fission"

	cases := []struct {
		name              string
		callerIP          string
		lookup            mapLookup
		targetNS          string
		allowedNamespaces []string
		wantStatus        int
		wantInner         bool
	}{
		{"same namespace allowed", "10.0.0.1:5000", mapLookup{"10.0.0.1": "tenant-a"}, "tenant-a", nil, http.StatusOK, true},
		{"internal component (install ns) may invoke any namespace", "10.0.0.2:5000", mapLookup{"10.0.0.2": installNS}, "tenant-b", nil, http.StatusOK, true},
		{"cross-namespace forbidden", "10.0.0.3:5000", mapLookup{"10.0.0.3": "tenant-a"}, "tenant-b", nil, http.StatusForbidden, false},
		{"unresolved caller IP forbidden (fail closed)", "10.0.0.9:5000", mapLookup{}, "tenant-a", nil, http.StatusForbidden, false},
		{"allowed namespace bypasses guard", "10.0.0.4:5000", mapLookup{"10.0.0.4": "monitoring"}, "tenant-b", []string{"monitoring"}, http.StatusOK, true},
		{"allowed namespace not in list still forbidden", "10.0.0.5:5000", mapLookup{"10.0.0.5": "monitoring"}, "tenant-b", []string{"logging"}, http.StatusForbidden, false},
		{"allowed namespace + same namespace still allowed", "10.0.0.6:5000", mapLookup{"10.0.0.6": "tenant-a"}, "tenant-a", []string{"monitoring"}, http.StatusOK, true},
		{"install namespace takes precedence over allowed list", "10.0.0.7:5000", mapLookup{"10.0.0.7": installNS}, "tenant-c", []string{"other"}, http.StatusOK, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			innerCalled := false
			inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				innerCalled = true
				w.WriteHeader(http.StatusOK)
			})
			allowed := make(map[string]struct{})
			for _, ns := range tc.allowedNamespaces {
				allowed[ns] = struct{}{}
			}
			g := &sameNamespaceGuard{
				lookup:                  tc.lookup,
				installNamespace:        installNS,
				allowedNamespaces:       allowed,
				logger:                  loggerfactory.GetLogger(),
				unresolvedRetries:       testUnresolvedRetries,
				unresolvedRetryInterval: testUnresolvedRetryInterval,
			}
			h := g.wrap(inner, tc.targetNS)

			req := httptest.NewRequest(http.MethodPost, "/fission-function/"+tc.targetNS+"/fn", nil)
			req.RemoteAddr = tc.callerIP
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			assert.Equal(t, tc.wantStatus, rec.Code)
			assert.Equal(t, tc.wantInner, innerCalled, "inner handler reached?")
		})
	}
}

// eventuallyResolvingLookup reports unresolved for the first missBeforeHit
// lookups of a given IP, then resolves to ns thereafter -- simulating a
// brand-new caller pod whose IP becomes observable (via the informer or the
// API fallback) only after a short delay, confirmed live against a real
// cluster (see resolveCallerNamespace's doc comment).
type eventuallyResolvingLookup struct {
	ip            string
	ns            string
	missBeforeHit int
	calls         int
}

func (e *eventuallyResolvingLookup) lookup(ip string) (string, bool) {
	if ip != e.ip {
		return "", false
	}
	e.calls++
	if e.calls <= e.missBeforeHit {
		return "", false
	}
	return e.ns, true
}

// TestResolveCallerNamespaceRetriesBeforeFailingClosed pins the fix for a
// 100%-reproducible race confirmed live: a pod's very first request to the
// internal listener, fired the instant its container starts, was rejected
// every time because neither the informer cache nor the direct API-list
// fallback had observed the pod's own status.podIP yet. resolveCallerNamespace
// must retry briefly instead of failing closed on the first miss.
func TestResolveCallerNamespaceRetriesBeforeFailingClosed(t *testing.T) {
	t.Run("resolves within the retry budget", func(t *testing.T) {
		lookup := &eventuallyResolvingLookup{ip: "10.0.0.1", ns: "tenant-a", missBeforeHit: 2}
		g := &sameNamespaceGuard{
			lookup:                  lookup,
			logger:                  loggerfactory.GetLogger(),
			unresolvedRetries:       testUnresolvedRetries + 2,
			unresolvedRetryInterval: testUnresolvedRetryInterval,
		}
		ns, found := g.resolveCallerNamespace("10.0.0.1")
		require.True(t, found, "must resolve once the lookup starts succeeding, not fail closed on the first miss")
		assert.Equal(t, "tenant-a", ns)
		assert.Greater(t, lookup.calls, 1, "must have actually retried, not just the first bare lookup")
	})

	t.Run("still fails closed once the retry budget is exhausted", func(t *testing.T) {
		lookup := &eventuallyResolvingLookup{ip: "10.0.0.1", ns: "tenant-a", missBeforeHit: 1000}
		g := &sameNamespaceGuard{
			lookup:                  lookup,
			logger:                  loggerfactory.GetLogger(),
			unresolvedRetries:       testUnresolvedRetries,
			unresolvedRetryInterval: testUnresolvedRetryInterval,
		}
		_, found := g.resolveCallerNamespace("10.0.0.1")
		assert.False(t, found, "a caller that never resolves must still fail closed, not retry forever")
	})

	t.Run("wrap() end-to-end: a request from an initially-unresolved caller succeeds once it resolves", func(t *testing.T) {
		lookup := &eventuallyResolvingLookup{ip: "10.0.0.1", ns: "tenant-a", missBeforeHit: 2}
		innerCalled := false
		inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			innerCalled = true
			w.WriteHeader(http.StatusOK)
		})
		g := &sameNamespaceGuard{
			lookup:                  lookup,
			logger:                  loggerfactory.GetLogger(),
			unresolvedRetries:       testUnresolvedRetries + 2,
			unresolvedRetryInterval: testUnresolvedRetryInterval,
		}
		h := g.wrap(inner, "tenant-a")

		req := httptest.NewRequest(http.MethodPost, "/fission-function/tenant-a/fn", nil)
		req.RemoteAddr = "10.0.0.1:5000"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code, "must not 403 a same-namespace caller just because its first lookup missed")
		assert.True(t, innerCalled)
	})
}

func TestParseAllowedNamespaces(t *testing.T) {
	tests := []struct {
		name  string
		raw   string
		want  map[string]struct{}
		count int
	}{
		{"empty string", "", map[string]struct{}{}, 0},
		{"single namespace", "monitoring", map[string]struct{}{"monitoring": {}}, 1},
		{"multiple namespaces", "monitoring,logging", map[string]struct{}{"monitoring": {}, "logging": {}}, 2},
		{"with spaces", "  monitoring , logging ", map[string]struct{}{"monitoring": {}, "logging": {}}, 2},
		{"trailing comma", "monitoring,", map[string]struct{}{"monitoring": {}}, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseAllowedNamespaces(tt.raw)
			assert.Equal(t, tt.count, len(got))
			for k := range tt.want {
				_, ok := got[k]
				assert.True(t, ok, "expected key %q not found", k)
			}
		})
	}
}

func TestClientIP(t *testing.T) {
	assert.Equal(t, "10.0.0.1", clientIP("10.0.0.1:5678"))
	assert.Equal(t, "10.0.0.1", clientIP("10.0.0.1"))     // no port
	assert.Equal(t, "fe80::1", clientIP("[fe80::1]:443")) // ipv6
}

func TestPodIPCache(t *testing.T) {
	c := &podIPCache{ipToPod: map[string]podRef{}}

	c.set("10.0.0.1", podRef{namespace: "ns1", name: "pod-a"})
	ns, ok := c.lookup("10.0.0.1")
	assert.True(t, ok)
	assert.Equal(t, "ns1", ns)

	_, ok = c.lookup("10.0.0.99")
	assert.False(t, ok, "unknown IP must not resolve")

	// IP recycling: pod-b takes over 10.0.0.1 in ns2; a late delete for pod-a must
	// NOT evict pod-b's mapping.
	c.set("10.0.0.1", podRef{namespace: "ns2", name: "pod-b"})
	c.del("10.0.0.1", "pod-a")
	ns, ok = c.lookup("10.0.0.1")
	assert.True(t, ok)
	assert.Equal(t, "ns2", ns, "stale delete for the previous owner must not evict the recycled IP")

	// The matching delete removes it.
	c.del("10.0.0.1", "pod-b")
	_, ok = c.lookup("10.0.0.1")
	assert.False(t, ok)

	// Empty IP is ignored on both set and lookup.
	c.set("", podRef{namespace: "x", name: "y"})
	_, ok = c.lookup("")
	assert.False(t, ok)
}

// TestPodIPCacheAPIFallback covers the cache-miss path: a caller whose pod the
// informer has not observed yet (a fresh pod racing the watch) must still resolve
// via a direct API lookup, so the guard does not wrongly reject it.
func TestPodIPCacheAPIFallback(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "caller", Namespace: "tenant-x"},
		Status:     corev1.PodStatus{PodIP: "10.1.2.3"},
	}
	c := &podIPCache{
		ipToPod:    map[string]podRef{}, // empty warm cache forces the API fallback
		kubeClient: k8sfake.NewClientset(pod),
		logger:     loggerfactory.GetLogger(),
	}

	ns, ok := c.lookup("10.1.2.3")
	assert.True(t, ok, "cache miss must resolve via the API fallback")
	assert.Equal(t, "tenant-x", ns)

	c.mu.RLock()
	_, warm := c.ipToPod["10.1.2.3"]
	c.mu.RUnlock()
	assert.True(t, warm, "an API-resolved IP should be warmed into the cache")

	_, ok = c.lookup("10.9.9.9")
	assert.False(t, ok, "an IP with no pod stays unresolved")
}
