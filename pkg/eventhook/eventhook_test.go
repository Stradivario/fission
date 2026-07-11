// SPDX-FileCopyrightText: The Fission Authors
//
// SPDX-License-Identifier: Apache-2.0

package eventhook

import (
	"testing"

	"github.com/fission/fission/pkg/utils/loggerfactory"
)

// A nil *Dispatcher is a real, common construction path: any struct that
// embeds/holds a *Dispatcher field and is built via a Go struct literal
// (rather than through NewPackageBuildDispatcher/NewFunctionDeployDispatcher)
// leaves it nil. Both methods must treat that exactly like an unconfigured
// Dispatcher — this regression-guards the buildermgr panic this package
// caused before Enabled/Send were made nil-safe (PackageReconciler.eventHook
// left nil by a struct-literal test construction).
func TestNilDispatcherIsSafe(t *testing.T) {
	var d *Dispatcher

	if d.Enabled() {
		t.Fatal("nil *Dispatcher must report Enabled() == false")
	}

	// Must not panic.
	d.Send(t.Context(), Event{Event: EventPackageBuildSucceeded})
}

// An unconfigured Dispatcher (EVENTHOOK_ENABLED unset, the default) must
// behave identically to a nil one: Enabled() == false, Send() a no-op.
func TestUnconfiguredDispatcherIsNoop(t *testing.T) {
	t.Setenv("EVENTHOOK_ENABLED", "")

	d := NewPackageBuildDispatcher(loggerfactory.GetLogger())
	if d == nil {
		t.Fatal("constructors must never return nil")
	}
	if d.Enabled() {
		t.Fatal("Dispatcher with EVENTHOOK_ENABLED unset must report Enabled() == false")
	}

	// Must not panic and must not attempt any network call (no publisher constructed).
	d.Send(t.Context(), Event{Event: EventPackageBuildSucceeded})
}

// EVENTHOOK_ENABLED=true with no target configured for this event family is
// still a no-op — the master switch alone isn't enough, a namespace+function
// (or url) must also be set.
func TestEnabledWithoutTargetIsNoop(t *testing.T) {
	t.Setenv("EVENTHOOK_ENABLED", "true")
	t.Setenv("EVENTHOOK_PACKAGE_BUILD_NAMESPACE", "")
	t.Setenv("EVENTHOOK_PACKAGE_BUILD_FUNCTION", "")
	t.Setenv("EVENTHOOK_PACKAGE_BUILD_URL", "")

	d := NewPackageBuildDispatcher(loggerfactory.GetLogger())
	if d.Enabled() {
		t.Fatal("Dispatcher with no target configured must report Enabled() == false even when EVENTHOOK_ENABLED=true")
	}
}

// A fully configured Dispatcher (function-target mode) reports Enabled() ==
// true. Send's actual HTTP dispatch goes through publisher.WebhookPublisher
// (pkg/publisher), which owns its own test coverage — this package's
// responsibility is only reaching a configured state and not blocking/erroring.
func TestConfiguredDispatcherIsEnabled(t *testing.T) {
	t.Setenv("EVENTHOOK_ENABLED", "true")
	t.Setenv("EVENTHOOK_FUNCTION_DEPLOY_NAMESPACE", "platform")
	t.Setenv("EVENTHOOK_FUNCTION_DEPLOY_FUNCTION", "lifecycle-events")

	d := NewFunctionDeployDispatcher(loggerfactory.GetLogger())
	if !d.Enabled() {
		t.Fatal("Dispatcher with namespace+function configured must report Enabled() == true")
	}

	// Must not block: WebhookPublisher.Publish only queues onto a buffered
	// channel and returns.
	d.Send(t.Context(), Event{
		Event:                EventFunctionDeployReady,
		Namespace:            "some-project",
		Function:             "some-lambda",
		DeploymentGeneration: "3",
	})
}
