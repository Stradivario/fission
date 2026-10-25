// SPDX-FileCopyrightText: The Fission Authors
//
// SPDX-License-Identifier: Apache-2.0

package cms

import (
	"context"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/go-logr/logr"
	apiv1 "k8s.io/api/core/v1"
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

// Under dynamic tenancy the executor Manager's Secret/ConfigMap cache is scoped to
// the env-seeded namespaces fixed at startup (executorCacheOptions), so a change
// to a Secret/ConfigMap in a namespace onboarded at runtime was never seen and
// the functions mounting it were never recycled. tenantWatcher closes that gap
// without a cluster-wide Secret read: it runs one metadata-only Secret and
// ConfigMap informer per live tenant namespace, starting them on onboarding and
// stopping them on offboarding. Metadata-only means no Secret data is held in
// executor memory; the per-namespace list/watch is already granted by the
// tenant controller's executor workload RoleBinding.

var (
	secretGVR    = apiv1.SchemeGroupVersion.WithResource("secrets")
	configMapGVR = apiv1.SchemeGroupVersion.WithResource("configmaps")
)

// tenantWatchResync is the drift guard: the FissionTenant informer triggers a
// sync on every tenant change, this re-converges anything missed.
const tenantWatchResync = 5 * time.Minute

type tenantWatcher struct {
	logger       logr.Logger
	metaClient   metadata.Interface
	reader       client.Reader
	informers    crcache.Informers
	secretEvents chan event.GenericEvent
	cmEvents     chan event.GenericEvent
	trigger      chan struct{}

	mu      sync.Mutex
	watches map[string]context.CancelFunc
}

func newTenantWatcher(logger logr.Logger, metaClient metadata.Interface, reader client.Reader, informers crcache.Informers) *tenantWatcher {
	return &tenantWatcher{
		logger:       logger.WithName("tenant_watcher"),
		metaClient:   metaClient,
		reader:       reader,
		informers:    informers,
		secretEvents: make(chan event.GenericEvent, 1024),
		cmEvents:     make(chan event.GenericEvent, 1024),
		trigger:      make(chan struct{}, 1),
		watches:      map[string]context.CancelFunc{},
	}
}

// NeedLeaderElection keeps the watcher on the elected leader, alongside the
// Secret/ConfigMap reconcilers it feeds.
func (w *tenantWatcher) NeedLeaderElection() bool { return true }

// Start implements manager.Runnable. It blocks until ctx is done, then stops
// every per-namespace watch.
func (w *tenantWatcher) Start(ctx context.Context) error {
	inf, err := w.informers.GetInformer(ctx, &fv1.FissionTenant{})
	if err != nil {
		return err
	}
	kick := func(any) { w.kick() }
	if _, err := inf.AddEventHandler(k8sCache.ResourceEventHandlerFuncs{
		AddFunc:    kick,
		UpdateFunc: func(any, any) { w.kick() },
		DeleteFunc: kick,
	}); err != nil {
		return err
	}

	ticker := time.NewTicker(tenantWatchResync)
	defer ticker.Stop()
	w.kick()
	for {
		select {
		case <-ctx.Done():
			w.sync(ctx, nil)
			return nil
		case <-w.trigger:
		case <-ticker.C:
		}
		desired, err := w.desiredNamespaces(ctx)
		if err != nil {
			w.logger.Error(err, "failed to list tenant namespaces; keeping current secret/configmap watches")
			continue
		}
		w.sync(ctx, desired)
	}
}

func (w *tenantWatcher) kick() {
	select {
	case w.trigger <- struct{}{}:
	default:
	}
}

// desiredNamespaces is the live resolver set (env seed + tenants, kept current by
// the resolver-sync controller) plus the builder/function/default namespaces —
// the same set executorCacheOptions scopes to — unioned with the FissionTenants
// read directly, so a tenant event that races ahead of resolver-sync is not
// missed.
func (w *tenantWatcher) desiredNamespaces(ctx context.Context) (map[string]string, error) {
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

// sync starts a watch for every desired namespace not yet watched and stops every
// watched namespace no longer desired. A nil desired set stops everything.
func (w *tenantWatcher) sync(ctx context.Context, desired map[string]string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, ns := range slices.Sorted(maps.Keys(w.watches)) {
		if _, ok := desired[ns]; !ok {
			w.watches[ns]()
			delete(w.watches, ns)
			w.logger.Info("stopped secret/configmap watch", "namespace", ns)
		}
	}
	for _, ns := range slices.Sorted(maps.Keys(desired)) {
		if _, ok := w.watches[ns]; ok {
			continue
		}
		nsCtx, cancel := context.WithCancel(ctx)
		w.watches[ns] = cancel
		w.watch(nsCtx, ns, secretGVR, w.secretEvents)
		w.watch(nsCtx, ns, configMapGVR, w.cmEvents)
		w.logger.Info("started secret/configmap watch", "namespace", ns)
	}
}

func (w *tenantWatcher) watchedNamespaces() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Sorted(maps.Keys(w.watches))
}

// watch runs a metadata-only informer for gvr in ns until ctx is done, sending a
// GenericEvent for every ResourceVersion-changing update. Adds (the initial list)
// and deletes are dropped, matching contentChangedPredicate.
func (w *tenantWatcher) watch(ctx context.Context, ns string, gvr schema.GroupVersionResource, out chan<- event.GenericEvent) {
	inf := metadatainformer.NewFilteredMetadataInformer(w.metaClient, gvr, ns, 0, k8sCache.Indexers{}, nil).Informer()
	_, err := inf.AddEventHandler(k8sCache.ResourceEventHandlerFuncs{
		UpdateFunc: func(oldObj, newObj any) {
			o, ok1 := oldObj.(*metav1.PartialObjectMetadata)
			n, ok2 := newObj.(*metav1.PartialObjectMetadata)
			if !ok1 || !ok2 || o.ResourceVersion == n.ResourceVersion {
				return
			}
			select {
			case out <- event.GenericEvent{Object: n}:
			case <-ctx.Done():
			}
		},
	})
	if err != nil {
		w.logger.Error(err, "failed to add event handler", "namespace", ns, "resource", gvr.Resource)
		return
	}
	go inf.Run(ctx.Done())
}
