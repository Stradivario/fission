// SPDX-FileCopyrightText: The Fission Authors
//
// SPDX-License-Identifier: Apache-2.0

// Package eventhook dispatches best-effort build/deploy lifecycle events to a
// single configured target (a Fission function, resolved through the router's
// internal listener, or a raw URL escape hatch). See
// docs/features/build-deploy-webhooks.md for the design.
//
// Delivery reuses pkg/publisher.WebhookPublisher — the same async, retrying,
// (optionally) HMAC-signed dispatcher timer/mqtrigger/kubewatcher already use
// to invoke /fission-function/... on the router. That gives this package
// queued, non-blocking delivery with backoff for free: Send only ever queues
// onto the publisher's internal channel and returns, so a slow or unreachable
// target can never stall the buildermgr/executor reconcile loop that calls it.
package eventhook

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"time"

	"github.com/go-logr/logr"

	"github.com/fission/fission/pkg/publisher"
	"github.com/fission/fission/pkg/utils"
)

// Event lifecycle identifiers. See docs/features/build-deploy-webhooks.md#events
// for the payload shape each fires with.
const (
	EventPackageBuildSucceeded = "package.build.succeeded"
	EventPackageBuildFailed    = "package.build.failed"
	EventFunctionDeployReady   = "function.deploy.ready"
	EventFunctionDeployFailed  = "function.deploy.failed"
)

// Event is the JSON payload POSTed to the configured target. Fields are
// omitted (via omitempty) when not applicable to the firing event, rather than
// sent empty, so a receiver's shape doesn't have to special-case zero values
// per event family.
type Event struct {
	Event     string `json:"event"`
	Namespace string `json:"namespace"`

	// Package build events.
	Package     string `json:"package,omitempty"`
	BuildStatus string `json:"buildStatus,omitempty"`

	// Function deploy events. DeploymentGeneration lets the receiver tell a
	// genuinely new rollout apart from a stale one still sitting at a
	// generation it already observed, without a before/after snapshot dance
	// of its own (see the "gap" this closes in newdeploymgr.go).
	Function             string `json:"function,omitempty"`
	DeploymentGeneration string `json:"deploymentGeneration,omitempty"`

	Timestamp string `json:"timestamp"`
}

// target is where one event family (package build, or function deploy)
// should be dispatched. Exactly one of (namespace+function) or url is set in
// practice — namespace+function (resolved through the router) is the
// recommended mode; url is an escape hatch for a non-Fission receiver. See
// "Delivery target" in docs/features/build-deploy-webhooks.md for why the
// function-target mode is preferred.
//
// subpath is appended after the resolved /fission-function/<ns>/<fn> target,
// same as TimeTrigger.Spec.Subpath in timer.go: the router only strips the
// /fission-function/<ns>/<fn> prefix and forwards the rest, so the receiving
// function sees subpath as its own request path (e.g. a function that's a
// full app with its own router, like graphql-server-lambdas, needs this to
// land on a specific route rather than always hitting its root). Empty means
// root. Only meaningful in function-target mode; the url escape hatch is
// already a complete address.
type target struct {
	namespace string
	function  string
	subpath   string
	url       string
}

func (t target) configured() bool {
	return t.url != "" || (t.namespace != "" && t.function != "")
}

// Dispatcher sends Events for a single event family to its configured
// target. The zero value (and one built from an unconfigured environment) is
// a valid, inert no-op: Send returns immediately without touching the
// network, so callers never need to guard a Send call behind their own
// "is this enabled" check.
type Dispatcher struct {
	logger    logr.Logger
	target    target
	publisher publisher.Publisher
}

// NewPackageBuildDispatcher and NewFunctionDeployDispatcher construct a
// Dispatcher for their event family from EVENTHOOK_* environment variables
// (see docs/features/build-deploy-webhooks.md#configuration). Both are gated
// by the shared EVENTHOOK_ENABLED master switch; unset (the default) makes
// every Dispatcher a no-op, matching this fork's off-by-default convention
// for opt-in features (e.g. internalAuth.enabled).
//
// Construct once per process (one per event family) and reuse — matching how
// every other Fission internal publisher (timer, mqtrigger, kubewatcher) is
// constructed once and reused across many dispatches, not rebuilt per event.
func NewPackageBuildDispatcher(logger logr.Logger) *Dispatcher {
	return newDispatcher(logger, "EVENTHOOK_PACKAGE_BUILD")
}

func NewFunctionDeployDispatcher(logger logr.Logger) *Dispatcher {
	return newDispatcher(logger, "EVENTHOOK_FUNCTION_DEPLOY")
}

func newDispatcher(logger logr.Logger, envPrefix string) *Dispatcher {
	d := &Dispatcher{logger: logger.WithName("eventhook")}
	if os.Getenv("EVENTHOOK_ENABLED") != "true" {
		return d
	}
	d.target = target{
		namespace: os.Getenv(envPrefix + "_NAMESPACE"),
		function:  os.Getenv(envPrefix + "_FUNCTION"),
		subpath:   os.Getenv(envPrefix + "_SUBPATH"),
		url:       os.Getenv(envPrefix + "_URL"),
	}
	if !d.target.configured() {
		return d
	}

	baseURL := d.target.url
	if baseURL == "" {
		baseURL = routerInternalURL()
	}
	d.publisher = publisher.MakeWebhookPublisher(logger, baseURL)
	return d
}

// routerInternalURL mirrors the resolution in cmd/fission-bundle/main.go used
// for every other internal publisher (timer/mqtrigger/kubewatcher): prefer
// ROUTER_INTERNAL_URL — the router's internal, HMAC-verifying listener (see
// docs/internal-auth/00-design.md) — falling back to the legacy default for
// installs that don't set it.
func routerInternalURL() string {
	if internal := os.Getenv("ROUTER_INTERNAL_URL"); internal != "" {
		return internal
	}
	return "http://router.fission"
}

// Enabled reports whether this Dispatcher has a configured target. Callers
// that would otherwise do extra work solely to produce a value for Send (e.g.
// waiting for a Deployment to become available before reporting it ready)
// should check this first and skip that work when false, so an unconfigured
// Dispatcher — the default — costs nothing beyond the Send no-op itself.
//
// Nil-safe: a nil *Dispatcher reports false, same as one built from an
// unconfigured environment. Production code always gets a non-nil Dispatcher
// from NewPackageBuildDispatcher/NewFunctionDeployDispatcher, but callers
// (and their tests) that construct the owning struct as a literal — bypassing
// the constructor, a common Go test pattern — leave this field nil. Panicking
// on that would make an entirely unrelated struct literal fail the moment it
// touches a code path this package instruments, which defeats the point of an
// opt-in feature that is supposed to cost nothing unless configured.
func (d *Dispatcher) Enabled() bool {
	return d != nil && d.publisher != nil
}

// Send dispatches ev to this Dispatcher's configured target. A no-op — no
// allocation beyond the nil check, no network call — when unconfigured or nil
// (see Enabled).
//
// ev.Timestamp is set here (callers do not need to). Delivery is best-effort
// and asynchronous: the underlying WebhookPublisher queues the request and
// retries with backoff on its own goroutine, so Send never blocks the
// caller's reconcile loop. When FISSION_INTERNAL_AUTH_SECRET is set, the
// request is HMAC-signed automatically by that same publisher, exactly like
// every other internal call to the router's /fission-function/... listener —
// there is no separate signing path here to keep in sync.
func (d *Dispatcher) Send(ctx context.Context, ev Event) {
	if !d.Enabled() {
		return
	}
	ev.Timestamp = time.Now().UTC().Format(time.RFC3339)

	body, err := json.Marshal(ev)
	if err != nil {
		d.logger.Error(err, "failed to marshal event", "event", ev.Event)
		return
	}

	requestTarget := "/"
	if d.target.url == "" {
		requestTarget = utils.UrlForFunction(d.target.function, d.target.namespace) + d.target.subpath
	}

	headers := map[string]string{
		"Content-Type":    "application/json",
		"X-Fission-Event": ev.Event,
	}
	d.publisher.Publish(ctx, string(body), headers, http.MethodPost, requestTarget)
}
