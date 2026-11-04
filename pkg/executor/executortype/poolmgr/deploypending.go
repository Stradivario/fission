// SPDX-FileCopyrightText: The Fission Authors
//
// SPDX-License-Identifier: Apache-2.0

package poolmgr

import (
	"sync"

	k8sTypes "k8s.io/apimachinery/pkg/types"
)

// deployPendingSet tracks functions whose next specialize should report a
// function.deploy.ready/failed event (see docs/features/build-deploy-webhooks.md).
//
// poolmgr has no per-function Deployment to wait on the way the newdeploy
// executor does — pods are lazily specialized on demand from a shared
// per-environment pool, and a pod gets re-specialized constantly during
// normal operation (idle recycle, health-check failure, ...) for reasons
// having nothing to do with a Function create/update. Firing a deploy event
// on every specialize would be noise, not signal. So: ReconcileFunction marks
// a function's UID here on create/update; the specialize choke point in
// GenericPool.getFuncSvc consumes (checks-and-clears) the mark on its next
// outcome — success or failure — and only fires an event when it was
// actually pending. A function that specializes for any other reason (no
// pending mark) fires nothing, same as before this feature existed.
//
// Shared by pointer between GenericPoolManager (which marks, on reconcile)
// and every GenericPool it creates (which consumes, on specialize) — the same
// sharing pattern already used for fsCache between these two types.
type deployPendingSet struct {
	mu sync.Mutex
	m  map[k8sTypes.UID]struct{}
}

func newDeployPendingSet() *deployPendingSet {
	return &deployPendingSet{m: make(map[k8sTypes.UID]struct{})}
}

func (s *deployPendingSet) mark(uid k8sTypes.UID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[uid] = struct{}{}
}

// consume reports whether uid was pending, clearing it either way so a
// specialize outcome is only ever reported once per create/update — matching
// the newdeploy executor's "observe once" behavior.
func (s *deployPendingSet) consume(uid k8sTypes.UID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.m[uid]
	delete(s.m, uid)
	return ok
}
