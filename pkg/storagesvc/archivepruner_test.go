// SPDX-FileCopyrightText: The Fission Authors
//
// SPDX-License-Identifier: Apache-2.0

package storagesvc

import (
	"bytes"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fv1 "github.com/fission/fission/pkg/apis/core/v1"
	"github.com/fission/fission/pkg/generated/clientset/versioned/fake"
)

// pruneFixture: three archives on a local store - one referenced by a
// Package in a TENANT namespace (not the configured default), one old and
// unreferenced, one fresh and unreferenced.
type pruneFixture struct {
	pruner                 *ArchivePruner
	referenced, old, fresh string
}

func newPruneFixture(t *testing.T, dryRun bool) pruneFixture {
	t.Helper()
	sc, err := MakeStorageClient(logr.Discard(), NewLocalStorage(t.TempDir()))
	require.NoError(t, err)
	put := func(name string, age time.Duration) string {
		id, err := sc.backend.put(name, bytes.NewReader([]byte(name)), int64(len(name)))
		require.NoError(t, err)
		ts := time.Now().Add(-age)
		require.NoError(t, os.Chtimes(id, ts, ts))
		return id
	}
	f := pruneFixture{
		referenced: put("referenced", 30*24*time.Hour),
		old:        put("old-orphan", 30*24*time.Hour),
		fresh:      put("fresh-orphan", time.Hour),
	}
	pkg := &fv1.Package{
		ObjectMeta: metav1.ObjectMeta{Name: "app-1a2b3c", Namespace: "68ac38641fbf0b1a1e53e6ee"},
		Spec: fv1.PackageSpec{Deployment: fv1.Archive{
			Type: fv1.ArchiveTypeUrl,
			URL:  "http://storagesvc.fission/v1/archive?id=" + url.QueryEscape(f.referenced),
		}},
	}
	f.pruner = &ArchivePruner{
		logger:             logr.Discard(),
		crdClient:          fake.NewSimpleClientset(pkg),
		archiveChan:        make(chan string, 10),
		storageClient:      sc,
		maxOrphansPerCycle: 20,
		dryRun:             dryRun,
		minAge:             7 * 24 * time.Hour,
	}
	return f
}

func drained(ch chan string) []string {
	out := []string{}
	for {
		select {
		case id := <-ch:
			out = append(out, id)
		default:
			return out
		}
	}
}

// The 2026-07 / 2026-11 incident: a package in a tenant namespace must keep
// its archive; only the old unreferenced archive is deleted.
func TestArchivePrunerKeepsArchivesOfEveryNamespace(t *testing.T) {
	t.Parallel()
	f := newPruneFixture(t, false)
	f.pruner.getOrphanArchives(t.Context())

	assert.Equal(t, []string{f.old}, drained(f.pruner.archiveChan))
	plan := f.pruner.LastPlan()
	assert.Equal(t, 1, plan.Deleted)
	assert.Empty(t, plan.SkippedReason)
}

func TestArchivePrunerDryRunDeletesNothing(t *testing.T) {
	t.Parallel()
	f := newPruneFixture(t, true)
	f.pruner.getOrphanArchives(t.Context())

	assert.Empty(t, drained(f.pruner.archiveChan))
	plan := f.pruner.LastPlan()
	assert.True(t, plan.DryRun)
	assert.Equal(t, []string{f.old}, plan.Orphans, "the plan lists what a real run would delete")
	assert.Equal(t, "dry run", plan.SkippedReason)
	assert.Zero(t, plan.Deleted)
}

// An archive younger than the minimum age (an upload no package references
// yet) is never a candidate.
func TestArchivePrunerKeepsYoungArchives(t *testing.T) {
	t.Parallel()
	f := newPruneFixture(t, true)
	f.pruner.getOrphanArchives(t.Context())

	assert.NotContains(t, f.pruner.LastPlan().Orphans, f.fresh)
}

func TestArchivePrunerRespectsTheCap(t *testing.T) {
	t.Parallel()
	f := newPruneFixture(t, false)
	f.pruner.maxOrphansPerCycle = -1 // any orphan is above the cap
	f.pruner.getOrphanArchives(t.Context())

	assert.Empty(t, drained(f.pruner.archiveChan))
	assert.Equal(t, "orphan count above the per-cycle cap", f.pruner.LastPlan().SkippedReason)
}
