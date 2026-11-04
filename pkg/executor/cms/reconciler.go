// SPDX-FileCopyrightText: The Fission Authors
//
// SPDX-License-Identifier: Apache-2.0

package cms

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	apiv1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/metadata"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/source"

	fv1 "github.com/fission/fission/pkg/apis/core/v1"
	"github.com/fission/fission/pkg/controller"
	"github.com/fission/fission/pkg/executor/executortype"
	"github.com/fission/fission/pkg/executor/tenantwatch"
	"github.com/fission/fission/pkg/generated/clientset/versioned"
	"github.com/fission/fission/pkg/utils"
)

// ConfigMapReconciler and SecretReconciler replace the executor's ConfigMap +
// Secret informer event handlers. When a referenced ConfigMap/Secret's content
// changes, every Function that mounts it has its pods recycled (RefreshFuncPods)
// so the new data takes effect. They are registered with contentChangedPredicate
// so only a real content change triggers a reconcile — matching the old handlers,
// whose Add/Delete were no-ops and whose Update acted only on a ResourceVersion
// change.

// ConfigMapReconciler recycles pods of functions that reference a changed ConfigMap.
type ConfigMapReconciler struct {
	logger        logr.Logger
	client        client.Reader
	fissionClient versioned.Interface
	types         map[fv1.ExecutorType]executortype.ExecutorType
}

func (r *ConfigMapReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	cm := &apiv1.ConfigMap{}
	if err := r.client.Get(ctx, req.NamespacedName, cm); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	funcs, err := getConfigmapRelatedFuncs(ctx, &cm.ObjectMeta, r.fissionClient)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to get functions related to configmap %s/%s: %w", cm.Namespace, cm.Name, err)
	}
	if len(funcs) == 0 {
		return ctrl.Result{}, nil
	}
	r.logger.V(1).Info("configmap changed", "configmap_name", cm.Name, "configmap_namespace", cm.Namespace)
	refreshPods(ctx, r.logger, funcs, r.types)
	return ctrl.Result{}, nil
}

// SecretReconciler recycles pods of functions that reference a changed Secret.
type SecretReconciler struct {
	logger        logr.Logger
	client        client.Reader
	fissionClient versioned.Interface
	types         map[fv1.ExecutorType]executortype.ExecutorType
}

func (r *SecretReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	s := &apiv1.Secret{}
	if err := r.client.Get(ctx, req.NamespacedName, s); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	funcs, err := getSecretRelatedFuncs(ctx, r.logger, &s.ObjectMeta, r.fissionClient)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to get functions related to secret %s/%s: %w", s.Namespace, s.Name, err)
	}
	if len(funcs) == 0 {
		return ctrl.Result{}, nil
	}
	r.logger.V(1).Info("secret changed", "secret_name", s.Name, "secret_namespace", s.Namespace)
	refreshPods(ctx, r.logger, funcs, r.types)
	return ctrl.Result{}, nil
}

// RegisterReconcilers wires the ConfigMap + Secret reconcilers onto the executor
// Manager. They watch through the Manager's (namespace-scoped) cache and, like
// the rest of the executor's mutating controllers, run on the elected leader
// only. Under dynamic tenancy that cache is fixed at startup, so the reconcilers
// are fed by a tenantwatch.Watcher instead (see registerTenantScoped).
func RegisterReconcilers(mgr ctrl.Manager, logger logr.Logger, fissionClient versioned.Interface,
	types map[fv1.ExecutorType]executortype.ExecutorType) error {
	if utils.DynamicNamespacesEnabled() {
		return registerTenantScoped(mgr, logger, fissionClient, types)
	}
	cmReconciler := &ConfigMapReconciler{
		logger:        logger.WithName("configmap_reconciler"),
		client:        mgr.GetClient(),
		fissionClient: fissionClient,
		types:         types,
	}
	if err := controller.RegisterWithPredicates(mgr, &apiv1.ConfigMap{}, cmReconciler, "executor-configmap", 0, contentChangedPredicate()); err != nil {
		return fmt.Errorf("error registering configmap reconciler: %w", err)
	}
	secretReconciler := &SecretReconciler{
		logger:        logger.WithName("secret_reconciler"),
		client:        mgr.GetClient(),
		fissionClient: fissionClient,
		types:         types,
	}
	if err := controller.RegisterWithPredicates(mgr, &apiv1.Secret{}, secretReconciler, "executor-secret", 0, contentChangedPredicate()); err != nil {
		return fmt.Errorf("error registering secret reconciler: %w", err)
	}
	return nil
}

// registerTenantScoped is RegisterReconcilers for dynamic tenancy: a tenantwatch
// Watcher runs per-tenant metadata-only Secret/ConfigMap informers that follow the live
// tenant set, and feeds ResourceVersion changes to the same reconcilers through
// channel sources. The reconcilers read through the API reader (one GET per
// change) because the Manager cache cannot see runtime-onboarded namespaces.
func registerTenantScoped(mgr ctrl.Manager, logger logr.Logger, fissionClient versioned.Interface,
	types map[fv1.ExecutorType]executortype.ExecutorType) error {
	metaClient, err := metadata.NewForConfig(mgr.GetConfig())
	if err != nil {
		return fmt.Errorf("error creating metadata client: %w", err)
	}
	secrets := tenantwatch.MetadataSource(metaClient, apiv1.SchemeGroupVersion.WithResource("secrets"))
	configMaps := tenantwatch.MetadataSource(metaClient, apiv1.SchemeGroupVersion.WithResource("configmaps"))
	w := tenantwatch.New(logger.WithName("secret_configmap_tenant_watch"), mgr.GetClient(), mgr.GetCache(), secrets, configMaps)
	if err := mgr.Add(w); err != nil {
		return fmt.Errorf("error adding tenant secret/configmap watcher: %w", err)
	}

	cmReconciler := &ConfigMapReconciler{
		logger:        logger.WithName("configmap_reconciler"),
		client:        mgr.GetAPIReader(),
		fissionClient: fissionClient,
		types:         types,
	}
	if err := builder.ControllerManagedBy(mgr).
		Named("executor-configmap").
		WatchesRawSource(source.Channel(configMaps.Events, &handler.EnqueueRequestForObject{})).
		Complete(cmReconciler); err != nil {
		return fmt.Errorf("error registering configmap reconciler: %w", err)
	}
	secretReconciler := &SecretReconciler{
		logger:        logger.WithName("secret_reconciler"),
		client:        mgr.GetAPIReader(),
		fissionClient: fissionClient,
		types:         types,
	}
	if err := builder.ControllerManagedBy(mgr).
		Named("executor-secret").
		WatchesRawSource(source.Channel(secrets.Events, &handler.EnqueueRequestForObject{})).
		Complete(secretReconciler); err != nil {
		return fmt.Errorf("error registering secret reconciler: %w", err)
	}
	return nil
}

// contentChangedPredicate enqueues a reconcile only on an Update whose
// ResourceVersion changed. Creates and Deletes are dropped, reproducing the old
// informer handlers (Add/Delete were no-ops; Update acted only on a real
// content change). This also avoids a refresh storm from the initial list of
// every ConfigMap/Secret in the watched namespaces on startup.
func contentChangedPredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc:  func(event.CreateEvent) bool { return false },
		DeleteFunc:  func(event.DeleteEvent) bool { return false },
		GenericFunc: func(event.GenericEvent) bool { return false },
		UpdateFunc: func(e event.UpdateEvent) bool {
			return e.ObjectOld.GetResourceVersion() != e.ObjectNew.GetResourceVersion()
		},
	}
}

func refreshPods(ctx context.Context, logger logr.Logger, funcs []fv1.Function, types map[fv1.ExecutorType]executortype.ExecutorType) {
	for _, f := range funcs {
		var err error

		et, exists := types[f.Spec.InvokeStrategy.ExecutionStrategy.ExecutorType]
		if exists {
			err = et.RefreshFuncPods(ctx, logger, f)
		} else {
			err = fmt.Errorf("unknown executor type '%s'", f.Spec.InvokeStrategy.ExecutionStrategy.ExecutorType)
		}

		if err != nil {
			logger.Error(err, "Failed to recycle pods for function after configmap/secret changed", "function", f)
		}
	}
}
