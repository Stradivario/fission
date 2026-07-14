// SPDX-FileCopyrightText: The Fission Authors
//
// SPDX-License-Identifier: Apache-2.0

package storagesvc

import (
	"context"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/go-logr/logr"
	"golang.org/x/sync/errgroup"

	"github.com/fission/fission/pkg/crd"
	"github.com/fission/fission/pkg/generated/clientset/versioned"
	"github.com/fission/fission/pkg/utils"
)

type ArchivePruner struct {
	logger             logr.Logger
	crdClient          versioned.Interface
	archiveChan        chan string
	storageClient      *StorageClient
	pruneInterval      time.Duration
	maxOrphansPerCycle int
}

const defaultPruneInterval int = 60 // in minutes

// defaultMaxOrphansPerCycle bounds how many archives a single sweep may
// delete. A healthy steady state prunes a handful of genuinely abandoned
// archives per cycle (leftovers from `fission package delete` or similar); a
// sweep that wants to delete far more than that is a much stronger signal
// that the "still referenced" list itself is wrong (an incomplete namespace
// list, a resolver returning too few tenants, a listing that raced a bulk
// update, ...) than that this many archives are truly orphaned at once. See
// the 2026-07 incident where a bad reference list caused ~90 live archives
// across multiple tenant namespaces to be deleted in one sweep.
const defaultMaxOrphansPerCycle int = 20

func MakeArchivePruner(logger logr.Logger, clientGen crd.ClientGeneratorInterface, storageClient *StorageClient, pruneInterval time.Duration, maxOrphansPerCycle int) (*ArchivePruner, error) {
	fissionClient, err := clientGen.GetFissionClient()
	if err != nil {
		return nil, fmt.Errorf("failed to get fission client: %w", err)
	}

	if maxOrphansPerCycle <= 0 {
		maxOrphansPerCycle = defaultMaxOrphansPerCycle
	}

	return &ArchivePruner{
		logger:             logger.WithName("archive_pruner"),
		crdClient:          fissionClient,
		archiveChan:        make(chan string),
		storageClient:      storageClient,
		pruneInterval:      pruneInterval,
		maxOrphansPerCycle: maxOrphansPerCycle,
	}, nil
}

// pruneArchives listens to archiveChannel for archive ids that need to be deleted
func (pruner *ArchivePruner) pruneArchives(ctx context.Context) {
	pruner.logger.V(1).Info("listening to archiveChannel to prune archives")
	for {
		select {
		case archiveID := <-pruner.archiveChan:
			pruner.logger.Info("sending delete request for archive",
				"archive_id", archiveID)
			if err := pruner.storageClient.removeFileByID(archiveID); err != nil {
				// logging the error and continuing with other deletions.
				// hopefully this archive will be deleted in the next iteration.
				pruner.logger.Error(err, "ignoring error while deleting archive", "archive_id", archiveID)
			}
		case <-ctx.Done():
			close(pruner.archiveChan)
			pruner.logger.Info("stopped listening to archiveChannel to prune archives, context cancelled")
			return
		}
	}
}

// insertArchive method just writes the archive ID into the channel.
func (pruner *ArchivePruner) insertArchive(archiveID string) {
	pruner.archiveChan <- archiveID
}

// A user may have deleted pkgs with kubectl or fission cli. That only deletes crd.Package objects from kubernetes
// and not the archives that are referenced by them, leaving the archives as orphans.
// getOrphanArchives reaps the orphaned archives.
func (pruner *ArchivePruner) getOrphanArchives(ctx context.Context) {
	pruner.logger.V(1).Info("getting orphan archives")
	archivesRefByPkgs := make([]string, 0)

	// incomplete tracks whether we failed to enumerate every package's
	// referenced archives this cycle (a namespace List() failed, or a
	// package's URL didn't parse). A single bad namespace/package must not
	// blind us to every OTHER namespace/package's references, so we log and
	// keep scanning rather than aborting immediately — but we must also never
	// let an incomplete reference list feed the deletion phase below, since
	// that would make every archive we simply failed to see look orphaned.
	incomplete := false

	// get all pkgs from kubernetes
	for _, namespace := range utils.DefaultNSResolver().FissionResourceNamespaces() {
		pkgList, err := pruner.crdClient.CoreV1().Packages(namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			pruner.logger.Error(err, "error getting package list from kubernetes; skipping this namespace for this cycle", "namespace", namespace)
			incomplete = true
			continue
		}

		// extract archives referenced by these pkgs
		for _, pkg := range pkgList.Items {
			if pkg.Spec.Deployment.URL != "" {
				archiveID, err := getQueryParamValue(pkg.Spec.Deployment.URL, "id")
				if err != nil {
					pruner.logger.Error(err, "error extracting value of archiveID from deployment url",
						"package", pkg.Name, "namespace", pkg.Namespace, "url", pkg.Spec.Deployment.URL)
					incomplete = true
					continue
				}
				archivesRefByPkgs = append(archivesRefByPkgs, archiveID)
			}
			if pkg.Spec.Source.URL != "" {
				archiveID, err := getQueryParamValue(pkg.Spec.Source.URL, "id")
				if err != nil {
					pruner.logger.Error(err, "error extracting value of archiveID from source url",
						"package", pkg.Name, "namespace", pkg.Namespace, "url", pkg.Spec.Source.URL)
					incomplete = true
					continue
				}
				archivesRefByPkgs = append(archivesRefByPkgs, archiveID)
			}
		}
	}

	pruner.logger.V(1).Info("archives referenced by packages", "count", len(archivesRefByPkgs))

	// get all archives on storage
	// out of them, there may be some just created but not referenced by packages yet.
	// need to filter them out.
	archivesInStorage, err := pruner.storageClient.getItemIDsWithFilter(pruner.storageClient.filterItemCreatedAMinuteAgo, time.Now())
	if err != nil {
		pruner.logger.Error(err, "error getting items from storage")
		return
	}
	pruner.logger.V(1).Info("archives in storage", "count", len(archivesInStorage))

	if incomplete {
		pruner.logger.Error(nil, "skipping this prune cycle: failed to enumerate every package's referenced archives, refusing to guess which storage archives are orphaned")
		return
	}

	// difference of the two lists gives us the list of orphan archives. This is just a brute force approach.
	// need to do something more optimal at scale.
	orphanedArchives := getDifferenceOfLists(archivesInStorage, archivesRefByPkgs)
	pruner.logger.Info("computed orphan archives for this cycle",
		"orphan_count", len(orphanedArchives), "referenced_count", len(archivesRefByPkgs), "storage_count", len(archivesInStorage))

	// Safety valve: refuse a sweep that wants to delete an implausibly large
	// number of archives at once instead of silently carrying it out. See
	// defaultMaxOrphansPerCycle for why — this is the guardrail that would
	// have contained the 2026-07 incident regardless of what exactly made
	// the reference list wrong that cycle. A genuinely large legitimate
	// backlog can be cleared by raising PRUNE_MAX_ORPHANS_PER_CYCLE once the
	// operator has confirmed the orphan list is correct.
	if len(orphanedArchives) > pruner.maxOrphansPerCycle {
		pruner.logger.Error(nil, "refusing to prune: orphan count exceeds the per-cycle safety cap; this usually means the reference list is wrong, not that this many archives are truly abandoned",
			"orphan_count", len(orphanedArchives), "cap", pruner.maxOrphansPerCycle)
		return
	}

	// send each orphan archive away for deletion
	for _, archiveID := range orphanedArchives {
		pruner.insertArchive(archiveID)
	}
}

// Start starts a go routine that listens to a channel for archive IDs that need to deleted.
// Also wakes up at regular intervals to make a list of archive IDs that need to be reaped
// and sends them over to the channel for deletion
func (pruner *ArchivePruner) Start(ctx context.Context, mgr *errgroup.Group) {
	ticker := time.NewTicker(pruner.pruneInterval * time.Minute)
	defer ticker.Stop()
	mgr.Go(func() error {
		pruner.pruneArchives(ctx)
		return nil
	})
	for {
		select {
		case <-ticker.C:
			// This method fetches unused archive IDs and sends them to archiveChannel for deletion
			// silencing the errors, hoping they go away in next iteration.
			pruner.getOrphanArchives(ctx)
		case <-ctx.Done():
			return
		}
	}
}
