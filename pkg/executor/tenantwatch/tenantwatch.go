// SPDX-FileCopyrightText: The Fission Authors
//
// SPDX-License-Identifier: Apache-2.0

// Package tenantwatch runs per-namespace informers that follow the live tenant
// set under dynamic tenancy.
//
// The executor Manager's cache keeps Tier-B types (Secret, ConfigMap,
// ReplicaSet) namespace-scoped to the env-seeded namespaces fixed at startup
// (executorCacheOptions), so watches on them never saw a namespace onboarded at
// runtime. A Watcher closes that gap without a cluster-wide read: for every live
// tenant namespace it runs one informer per Source, starting them on onboarding
// and stopping them on offboarding, and delivers the updates a Source selects as
// GenericEvents for a controller's source.Channel. The per-namespace list/watch
// is already granted by the tenant controller's executor workload RoleBinding.
package tenantwatch

import (
	"context"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/metadata/metadatainformer"
	k8sCache "k8s.io/client-go/tools/cache"
	crcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"

	fv1 "github.com/fission/fission/pkg/apis/core/v1"
	"github.com/fission/fission/pkg/utils"
)

// resyncInterval is the drift guard: the FissionTenant informer triggers a
// sync on every tenant change, this re-converges anything missed.
const resyncInterval = 5 * time.Minute

// Source is one per-namespace watch. OnUpdate selects which updates reach
// Events; adds (the initial list) and deletes are dropped, so a (re)start does
// not fire for every existing object.
type Source struct {
	Resource    string
	NewInformer func(ns string) k8sCache.SharedIndexInformer
	OnUpdate    func(oldObj, newObj any) (client.Object, bool)
	Events      chan event.GenericEvent
}

// MetadataSource watches gvr metadata-only (no object data, e.g. no Secret
// values, is held in memory) and delivers ResourceVersion-changing updates.
func MetadataSource(metaClient metadata.Interface, gvr schema.GroupVersionResource) *Source {
	return &Source{
		Resource: gvr.Resource,
		NewInformer: func(ns string) k8sCache.SharedIndexInformer {
			return metadatainformer.NewFilteredMetadataInformer(metaClient, gvr, ns, 0, k8sCache.Indexers{}, nil).Informer()
		},
		OnUpdate: func(oldObj, newObj any) (client.Object, bool) {
			o, ok1 := oldObj.(*metav1.PartialObjectMetadata)
			n, ok2 := newObj.(*metav1.PartialObjectMetadata)
			if !ok1 || !ok2 || o.ResourceVersion == n.ResourceVersion {
				return nil, false
			}
			return n, true
		},
		Events: make(chan event.GenericEvent, 1024),
	}
}

// Watcher runs every Source's informer in each live tenant namespace. It is a
// leader-only manager.Runnable.
type Watcher struct {
	logger    logr.Logger
	reader    client.Reader
	informers crcache.Informers
	sources   []*Source
	trigger   chan struct{}

	mu      sync.Mutex
	watches map[string]context.CancelFunc
}

// New returns a Watcher for sources. reader lists FissionTenants; informers
// provides the FissionTenant informer whose events trigger a sync (both are the
// executor Manager's, whose Fission-CRD cache is cluster-wide in dynamic mode).
func New(logger logr.Logger, reader client.Reader, informers crcache.Informers, sources ...*Source) *Watcher {
	return &Watcher{
		logger:    logger,
		reader:    reader,
		informers: informers,
		sources:   sources,
		trigger:   make(chan struct{}, 1),
		watches:   map[string]context.CancelFunc{},
	}
}

// NeedLeaderElection keeps the watcher on the elected leader, alongside the
// reconcilers it feeds.
func (w *Watcher) NeedLeaderElection() bool { return true }

// Start implements manager.Runnable. It blocks until ctx is done, then stops
// every per-namespace watch.
func (w *Watcher) Start(ctx context.Context) error {
	inf, err := w.informers.GetInformer(ctx, &fv1.FissionTenant{})
	if err != nil {
		return err
	}
	if _, err := inf.AddEventHandler(k8sCache.ResourceEventHandlerFuncs{
		AddFunc:    func(any) { w.kick() },
		UpdateFunc: func(any, any) { w.kick() },
		DeleteFunc: func(any) { w.kick() },
	}); err != nil {
		return err
	}

	ticker := time.NewTicker(resyncInterval)
	defer ticker.Stop()
	w.kick()
	for {
		select {
		case <-ctx.Done():
			w.Sync(ctx, nil)
			return nil
		case <-w.trigger:
		case <-ticker.C:
		}
		desired, err := w.DesiredNamespaces(ctx)
		if err != nil {
			w.logger.Error(err, "failed to list tenant namespaces; keeping current watches")
			continue
		}
		w.Sync(ctx, desired)
	}
}

func (w *Watcher) kick() {
	select {
	case w.trigger <- struct{}{}:
	default:
	}
}

// DesiredNamespaces is the live resolver set (env seed + tenants, kept current
// by the resolver-sync controller) plus the builder/function/default namespaces
// — the same set executorCacheOptions scopes to — unioned with the
// FissionTenants read directly, so a tenant event that races ahead of
// resolver-sync is not missed.
func (w *Watcher) DesiredNamespaces(ctx context.Context) (map[string]string, error) {
	desired := utils.DefaultNSResolver().FissionNSWithOptions(utils.WithBuilderNs(), utils.WithFunctionNs(), utils.WithDefaultNs())
	list := &fv1.FissionTenantList{}
	if err := w.reader.List(ctx, list); err != nil {
		return nil, err
	}
	for i := range list.Items {
		if ns := list.Items[i].Spec.Namespace; ns != "" {
			desired[ns] = ns
		}
	}
	return desired, nil
}

// Sync starts a watch for every desired namespace not yet watched and stops
// every watched namespace no longer desired. A nil desired set stops everything.
func (w *Watcher) Sync(ctx context.Context, desired map[string]string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, ns := range slices.Sorted(maps.Keys(w.watches)) {
		if _, ok := desired[ns]; !ok {
			w.watches[ns]()
			delete(w.watches, ns)
			w.logger.Info("stopped tenant watch", "namespace", ns)
		}
	}
	for _, ns := range slices.Sorted(maps.Keys(desired)) {
		if _, ok := w.watches[ns]; ok {
			continue
		}
		nsCtx, cancel := context.WithCancel(ctx)
		w.watches[ns] = cancel
		for _, s := range w.sources {
			w.watch(nsCtx, ns, s)
		}
		w.logger.Info("started tenant watch", "namespace", ns)
	}
}

// Watched returns the namespaces currently watched.
func (w *Watcher) Watched() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Sorted(maps.Keys(w.watches))
}

func (w *Watcher) watch(ctx context.Context, ns string, s *Source) {
	send := func(obj client.Object) {
		select {
		case s.Events <- event.GenericEvent{Object: obj}:
		case <-ctx.Done():
		}
	}
	inf := s.NewInformer(ns)
	if _, err := inf.AddEventHandler(k8sCache.ResourceEventHandlerFuncs{
		UpdateFunc: func(oldObj, newObj any) {
			if o, ok := s.OnUpdate(oldObj, newObj); ok {
				send(o)
			}
		},
	}); err != nil {
		w.logger.Error(err, "failed to add event handler", "namespace", ns, "resource", s.Resource)
		return
	}
	go inf.Run(ctx.Done())
}
