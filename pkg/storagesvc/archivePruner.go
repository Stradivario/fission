// SPDX-FileCopyrightText: The Fission Authors
//
// SPDX-License-Identifier: Apache-2.0

package storagesvc

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/go-logr/logr"
	"golang.org/x/sync/errgroup"

	"github.com/fission/fission/pkg/crd"
	"github.com/fission/fission/pkg/generated/clientset/versioned"
)

type ArchivePruner struct {
	logger             logr.Logger
	crdClient          versioned.Interface
	archiveChan        chan string
	storageClient      *StorageClient
	pruneInterval      time.Duration
	maxOrphansPerCycle int
	// dryRun: compute the plan, delete nothing (default when PRUNE_DRY_RUN
	// is unset).
	dryRun bool
	// minAge: an unreferenced archive younger than this is never a candidate
	// (an upload a package does not reference yet).
	minAge time.Duration

	planMu   sync.Mutex
	lastPlan PrunePlan
}

// PrunePlan is the result of the last prune cycle (GET /v1/prune/plan).
type PrunePlan struct {
	At            time.Time `json:"at"`
	DryRun        bool      `json:"dryRun"`
	MinAgeHours   float64   `json:"minAgeHours"`
	Referenced    int       `json:"referenced"`
	InStorage     int       `json:"inStorage"`
	Orphans       []string  `json:"orphans"`
	Deleted       int       `json:"deleted"`
	SkippedReason string    `json:"skippedReason,omitempty"`
	MaxPerCycle   int       `json:"maxPerCycle"`
}

// LastPlan returns a copy of the last cycle's plan.
func (pruner *ArchivePruner) LastPlan() PrunePlan {
	pruner.planMu.Lock()
	defer pruner.planMu.Unlock()
	p := pruner.lastPlan
	p.Orphans = append([]string(nil), p.Orphans...)
	return p
}

func (pruner *ArchivePruner) setPlan(p PrunePlan) {
	pruner.planMu.Lock()
	pruner.lastPlan = p
	pruner.planMu.Unlock()
}

// defaultMinAgeHours: 7 days.
const defaultMinAgeHours = 168

// firstCycleDelay: the first prune cycle runs this long after start.
var firstCycleDelay = 2 * time.Minute

const defaultPruneInterval int = 15 // in minutes (was 60)

// defaultMaxOrphansPerCycle bounds how many archives one sweep deletes (the
// oldest first; a larger backlog continues next interval). It is a damage
// bound for something unforeseen, not a guess about the backlog: 1000 every
// 15 minutes clears ~96k archives a day (owner 2026-11-20: 20 an hour "is
// like nothing for production"). The reference list itself is guarded
// separately (incomplete listing -> skip; no package references anything ->
// skip). 2026-07 incident: a wrong list deleted ~90 live archives - the
// all-namespaces listing (1.29.4) removed that cause.
const defaultMaxOrphansPerCycle int = 1000

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
		dryRun:             true,
		minAge:             time.Duration(defaultMinAgeHours) * time.Hour,
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

	// get all pkgs from kubernetes - in EVERY namespace. The resolver's set
	// (FissionResourceNamespaces) is not kept in sync with the FissionTenants
	// in this process: under dynamic tenancy it holds only the configured
	// default namespace, which made every tenant's archive look orphaned
	// (the 2026-07 incident; 106 archives found missing on 2026-11-20). An
	// archive any Package anywhere references is kept.
	for _, namespace := range []string{metav1.NamespaceAll} {
		pkgList, err := pruner.crdClient.CoreV1().Packages(namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			pruner.logger.Error(err, "error listing packages in all namespaces; skipping this cycle", "namespace", namespace)
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
	plan := PrunePlan{At: time.Now(), DryRun: pruner.dryRun, MinAgeHours: pruner.minAge.Hours(), MaxPerCycle: pruner.maxOrphansPerCycle, Referenced: len(archivesRefByPkgs)}
	items, err := pruner.storageClient.getItemsWithFilter(pruner.storageClient.filterItemsNewerThan(time.Now(), pruner.minAge))
	if err != nil {
		pruner.logger.Error(err, "error getting items from storage")
		plan.SkippedReason = "storage listing failed"
		pruner.setPlan(plan)
		return
	}
	archivesInStorage := make([]string, 0, len(items))
	lastMod := make(map[string]time.Time, len(items))
	for _, it := range items {
		archivesInStorage = append(archivesInStorage, it.id)
		lastMod[it.id] = it.lastMod
	}
	pruner.logger.V(1).Info("archives in storage", "count", len(archivesInStorage))
	plan.InStorage = len(archivesInStorage)

	if incomplete {
		pruner.logger.Error(nil, "skipping this prune cycle: failed to enumerate every package's referenced archives, refusing to guess which storage archives are orphaned")
		plan.SkippedReason = "package listing incomplete"
		pruner.setPlan(plan)
		return
	}

	// difference of the two lists gives us the list of orphan archives. This is just a brute force approach.
	// need to do something more optimal at scale.
	// Sanity guard: storage holds archives but no package references any.
	// That is a broken listing (RBAC, an API outage returning an empty list),
	// not "everything is orphaned" - skip the cycle.
	if len(archivesRefByPkgs) == 0 && len(archivesInStorage) > 0 {
		pruner.logger.Error(nil, "skipping this prune cycle: no package references any archive while storage is not empty",
			"storage_count", len(archivesInStorage))
		plan.SkippedReason = "no package references any archive"
		pruner.setPlan(plan)
		return
	}

	orphanedArchives := getDifferenceOfLists(archivesInStorage, archivesRefByPkgs)
	// oldest first: a cycle that hits the cap deletes the longest-abandoned
	// archives, and every cycle makes progress on a backlog.
	sort.SliceStable(orphanedArchives, func(i, j int) bool {
		return lastMod[orphanedArchives[i]].Before(lastMod[orphanedArchives[j]])
	})
	pruner.logger.Info("computed orphan archives for this cycle",
		"orphan_count", len(orphanedArchives), "referenced_count", len(archivesRefByPkgs), "storage_count", len(archivesInStorage))

	// Rate limit: a cycle deletes at most maxOrphansPerCycle archives (the
	// oldest). It used to REFUSE the whole cycle above the cap - which kept a
	// wrong reference list from deleting everything at once (2026-07) but
	// also meant a legitimate backlog (400 orphans after a cleanup,
	// 2026-11-20) was never cleared. The reference list is now complete (all
	// namespaces) and the guards above skip a broken one; the cap bounds the
	// damage of anything still unforeseen to N archives per interval.
	plan.Orphans = orphanedArchives
	if pruner.dryRun {
		pruner.logger.Info("dry run: archive pruner deletes nothing", "orphan_count", len(orphanedArchives))
		plan.SkippedReason = "dry run"
		pruner.setPlan(plan)
		return
	}
	batch := orphanedArchives
	if pruner.maxOrphansPerCycle >= 0 && len(batch) > pruner.maxOrphansPerCycle {
		batch = batch[:pruner.maxOrphansPerCycle]
		pruner.logger.Info("orphan count above the per-cycle cap; deleting the oldest only",
			"orphan_count", len(orphanedArchives), "cap", pruner.maxOrphansPerCycle)
	}

	// send each orphan archive away for deletion
	for _, archiveID := range batch {
		pruner.insertArchive(archiveID)
	}
	plan.Deleted = len(batch)
	pruner.setPlan(plan)
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
	// first cycle shortly after start (not a full interval later), so the
	// plan (GET /v1/prune/plan) is there right after an upgrade
	first := time.NewTimer(firstCycleDelay)
	defer first.Stop()
	for {
		select {
		case <-first.C:
			pruner.getOrphanArchives(ctx)
		case <-ticker.C:
			// This method fetches unused archive IDs and sends them to archiveChannel for deletion
			// silencing the errors, hoping they go away in next iteration.
			pruner.getOrphanArchives(ctx)
		case <-ctx.Done():
			return
		}
	}
}
