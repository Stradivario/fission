// SPDX-FileCopyrightText: The Fission Authors
//
// SPDX-License-Identifier: Apache-2.0

package poolmgr

import (
	"testing"

	k8sTypes "k8s.io/apimachinery/pkg/types"
)

func TestDeployPendingSet(t *testing.T) {
	s := newDeployPendingSet()
	uid := k8sTypes.UID("fn-1")

	if s.consume(uid) {
		t.Fatal("consume on an unmarked uid must report false")
	}

	s.mark(uid)
	if !s.consume(uid) {
		t.Fatal("consume must report true right after mark")
	}

	// consume clears the mark — a second consume (e.g. a routine respecialize
	// unrelated to any deploy) must not report pending again.
	if s.consume(uid) {
		t.Fatal("consume must clear the mark; a second consume must report false")
	}
}

func TestDeployPendingSetDistinctUIDs(t *testing.T) {
	s := newDeployPendingSet()
	a, b := k8sTypes.UID("fn-a"), k8sTypes.UID("fn-b")

	s.mark(a)
	if s.consume(b) {
		t.Fatal("consume must not report true for a uid that was never marked, even if a different uid was")
	}
	if !s.consume(a) {
		t.Fatal("the actually-marked uid must still be consumable")
	}
}
