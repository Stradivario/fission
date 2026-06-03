/*
Copyright 2017 The Fission Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package buildermgr

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"
	apiv1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	k8sCache "k8s.io/client-go/tools/cache"

	fv1 "github.com/fission/fission/pkg/apis/core/v1"
	"github.com/fission/fission/pkg/cache"
	"github.com/fission/fission/pkg/crd"
	"github.com/fission/fission/pkg/generated/clientset/versioned"
	"github.com/fission/fission/pkg/utils"
	"github.com/fission/fission/pkg/utils/manager"
	"github.com/fission/fission/pkg/utils/metrics"
)

type (
	packageWatcher struct {
		logger        *zap.Logger
		fissionClient versioned.Interface
		nsResolver    *utils.NamespaceResolver
		k8sClient     kubernetes.Interface
		podInformer   map[string]k8sCache.SharedIndexInformer
		pkgInformer   map[string]k8sCache.SharedIndexInformer
		storageSvcUrl string
		buildCache    *cache.Cache[crd.CacheKeyUR, *fv1.Package]
		envWatcher    *environmentWatcher
	}
)

func makePackageWatcher(logger *zap.Logger, fissionClient versioned.Interface, k8sClientSet kubernetes.Interface,
	storageSvcUrl string, podInformer,
	pkgInformer map[string]k8sCache.SharedIndexInformer,
	envWatcher *environmentWatcher) *packageWatcher {
	pkgw := &packageWatcher{
		logger:        logger.Named("package_watcher"),
		fissionClient: fissionClient,
		k8sClient:     k8sClientSet,
		nsResolver:    utils.DefaultNSResolver(),
		podInformer:   podInformer,
		pkgInformer:   pkgInformer,
		storageSvcUrl: storageSvcUrl,
		buildCache:    cache.MakeCache[crd.CacheKeyUR, *fv1.Package](0, 0),
		envWatcher:    envWatcher,
	}
	return pkgw
}

func (pkgw *packageWatcher) buildCacheKey(obj metav1.ObjectMeta) crd.CacheKeyUR {
	return crd.CacheKeyURFromMeta(&obj)
}

func (pkgw *packageWatcher) buildWithCache(ctx context.Context, srcpkg *fv1.Package) {
	// Ignore duplicate build requests
	_, err := pkgw.buildCache.Set(pkgw.buildCacheKey(srcpkg.ObjectMeta), srcpkg)
	if err != nil {
		pkgw.logger.Info("package build cache set error", zap.Error(err))
		return
	}
	go pkgw.build(ctx, srcpkg)
}

// build helps to update package status, checks environment builder pod status and
// dispatches buildPackage to build source package into deployment package.
// Following is the steps build function takes to complete the whole process.
// 1. Check package status
// 2. Update package status to running state
// 3. Check environment builder pod status
// 4. Call buildPackage to build package
// 5. Update package resource in package ref of functions that share the same package
// 6. Update package status to succeed state
// *. Update package status to failed state,if any one of steps above failed/time out
func (pkgw *packageWatcher) build(ctx context.Context, srcpkg *fv1.Package) {
	key := pkgw.buildCacheKey(srcpkg.ObjectMeta)
	logger := pkgw.logger.With(zap.String("package", srcpkg.Name), zap.String("namespace", srcpkg.Namespace), zap.String("resource_version", srcpkg.ResourceVersion), zap.String("key", key.String()))

	defer func() {
		pkgw.buildCache.Delete(key)
	}()

	logger.Info("starting build for package")

	pkg, err := updatePackage(ctx, logger, pkgw.fissionClient, srcpkg, fv1.BuildStatusRunning, "", nil)
	if err != nil {
		logger.Error("error setting package pending state", zap.Error(err))
		return
	}

	env, err := pkgw.fissionClient.CoreV1().Environments(pkg.Spec.Environment.Namespace).Get(ctx, pkg.Spec.Environment.Name, metav1.GetOptions{})
	if k8serrors.IsNotFound(err) {
		e := "environment does not exist"
		logger.Error(e, zap.String("environment", pkg.Spec.Environment.Name))
		_, er := updatePackage(ctx, logger, pkgw.fissionClient, pkg,
			fv1.BuildStatusFailed, fmt.Sprintf("%s: %q", e, pkg.Spec.Environment.Name), nil)
		if er != nil {
			logger.Error(
				"error updating package",
				zap.Error(er),
			)
		}
		return
	}

	// Track this build for demand-based scaling. activeBuilds is the number of
	// in-flight builds for this environment; it drives how many builder pods we
	// provision (one pod per concurrent build, capped by the env's builder pool
	// size) and keeps the idle reaper from scaling the builder down mid-build.
	activeBuilds := pkgw.envWatcher.IncActiveBuilds(env.UID)
	defer pkgw.envWatcher.DecActiveBuilds(env.UID)

	builderNs := pkgw.nsResolver.GetBuilderNS(env.Namespace)
	logger = logger.With(zap.String("environment", env.Name), zap.String("builder_namespace", builderNs), zap.String("environment_namespace", env.Namespace))

	// Scale the builder deployment up toward the number of concurrent builds,
	// capped at the env's builder pool size. Only scales up; the idle reaper
	// returns it to zero once all builds finish (so in-flight builds are never
	// terminated by a scale-down).
	err = pkgw.scaleBuilderForDemand(ctx, env, activeBuilds)
	if err != nil {
		logger.Error("error scaling builder for demand", zap.Error(err))
		_, er := updatePackage(ctx, logger, pkgw.fissionClient, pkg, fv1.BuildStatusFailed, fmt.Sprintf("error scaling builder: %v", err), nil)
		if er != nil {
			logger.Error("error updating package", zap.Error(er))
		}
		return
	}

	// Wait for a Ready builder pod that is not already running another build and
	// claim it, so this build gets its own dedicated pod. Pinning fetch+build+
	// upload to one pod IP is required for correctness with more than one replica
	// (the fetched source lives on the pod's local volume).
	podIP, err := pkgw.acquireReadyBuilderPod(ctx, logger, env, builderNs)
	if err != nil {
		logger.Error("error acquiring a ready builder pod", zap.Error(err))
		_, er := updatePackage(ctx, logger, pkgw.fissionClient, pkg, fv1.BuildStatusFailed, fmt.Sprintf("%v", err), nil)
		if er != nil {
			logger.Error("error updating package", zap.Error(er))
		}
		return
	}
	defer pkgw.envWatcher.ReleaseBuilderPod(env.UID, podIP)
	logger = logger.With(zap.String("builder_pod_ip", podIP))

	uploadResp, buildLogs, err := buildPackage(ctx, pkgw.logger, pkgw.fissionClient, builderNs, podIP, pkgw.storageSvcUrl, pkg)
	if err != nil {
		logger.Error("error building package", zap.Error(err))
		_, er := updatePackage(ctx, logger, pkgw.fissionClient, pkg, fv1.BuildStatusFailed, buildLogs, nil)
		if er != nil {
			logger.Error("error updating package", zap.Error(er))
		}
		return
	}

	logger.Info("starting package info update")

	fnList, err := pkgw.fissionClient.CoreV1().
		Functions(pkg.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		e := "error getting function list"
		pkgw.logger.Error(e, zap.Error(err))
		buildLogs += fmt.Sprintf("%s: %v\n", e, err)
		_, er := updatePackage(ctx, pkgw.logger, pkgw.fissionClient, pkg, fv1.BuildStatusFailed, buildLogs, nil)
		if er != nil {
			pkgw.logger.Error(
				"error updating package",
				zap.Error(er),
			)
		}
	}

	// A package may be used by multiple functions. Update
	// functions with old package resource version
	for _, fn := range fnList.Items {
		if fn.Spec.Package.PackageRef.Name == pkg.Name &&
			fn.Spec.Package.PackageRef.Namespace == pkg.Namespace &&
			fn.Spec.Package.PackageRef.ResourceVersion != pkg.ResourceVersion {
			fn.Spec.Package.PackageRef.ResourceVersion = pkg.ResourceVersion
			// update CRD
			_, err = pkgw.fissionClient.CoreV1().Functions(fn.Namespace).Update(ctx, &fn, metav1.UpdateOptions{})
			if err != nil {
				e := "error updating function package resource version"
				logger.Error(e, zap.Error(err))
				buildLogs += fmt.Sprintf("%s: %v\n", e, err)
				_, er := updatePackage(ctx, logger, pkgw.fissionClient, pkg, fv1.BuildStatusFailed, buildLogs, nil)
				if er != nil {
					logger.Error("error updating package", zap.Error(er))
				}
				return
			}
		}
	}

	_, err = updatePackage(ctx, logger, pkgw.fissionClient, pkg,
		fv1.BuildStatusSucceeded, buildLogs, uploadResp)
	if err != nil {
		logger.Error("error updating package info", zap.Error(err))
		_, er := updatePackage(ctx, logger, pkgw.fissionClient, pkg, fv1.BuildStatusFailed, buildLogs, nil)
		if er != nil {
			logger.Error("error updating package", zap.Error(er))
		}
		return
	}

	logger.Info("completed package build request")
}

func (pkgw *packageWatcher) packageInformerHandler(ctx context.Context) k8sCache.ResourceEventHandlerFuncs {
	processPkg := func(ctx context.Context, pkg *fv1.Package) {
		var err error
		if len(pkg.Status.BuildStatus) == 0 {
			_, err = setInitialBuildStatus(ctx, pkgw.fissionClient, pkg)
			if err != nil {
				pkgw.logger.Error("error filling package status", zap.Error(err))
			}
			// once we update the package status, an update event
			// will arrive and handle by UpdateFunc later. So we
			// don't need to build the package at this moment.
			return
		}
		// Only build pending state packages.
		// DO NOT build packages with BuildStatusRunning - they are already building!
		// Building running packages causes infinite loops when builder pod is not ready.
		if pkg.Status.BuildStatus == fv1.BuildStatusPending {
			pkgw.buildWithCache(ctx, pkg)
		}
	}
	return k8sCache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			pkg := obj.(*fv1.Package)
			processPkg(ctx, pkg)
		},
		UpdateFunc: func(oldObj, newObj any) {
			oldPkg := oldObj.(*fv1.Package)
			pkg := newObj.(*fv1.Package)

			// TODO: Once enable "/status", check generation for spec changed instead.
			//   Before "/status" is enabled, the generation and resource version will be changed
			//   if we update the status of a package, hence we are not able to differentiate
			//   the spec change or status change. So we only build package which has status
			//   us "pending" and user have to use "kubectl replace" to update a package.
			if oldPkg.ResourceVersion == pkg.ResourceVersion &&
				pkg.Status.BuildStatus != fv1.BuildStatusPending {
				return
			}
			processPkg(ctx, pkg)
		},
	}
}

func (pkgw *packageWatcher) Run(ctx context.Context, mgr manager.Interface) error {

	mgr.Add(ctx, func(ctx context.Context) {
		metrics.ServeMetrics(ctx, "buildermgr", pkgw.logger, mgr)
	})
	mgr.AddInformers(ctx, pkgw.podInformer)
	for _, pkgInformer := range pkgw.pkgInformer {
		_, err := pkgInformer.AddEventHandler(pkgw.packageInformerHandler(ctx))
		if err != nil {
			pkgw.logger.Fatal("error adding package informer handler", zap.Error(err))
			return err
		}
	}
	mgr.AddInformers(ctx, pkgw.pkgInformer)
	return nil
}

// setInitialBuildStatus sets initial build status to a package if it is empty.
// This normally occurs when the user applies package YAML files that have no status field
// through kubectl.
func setInitialBuildStatus(ctx context.Context, fissionClient versioned.Interface, pkg *fv1.Package) (*fv1.Package, error) {
	pkg.Status = fv1.PackageStatus{
		LastUpdateTimestamp: metav1.Time{Time: time.Now().UTC()},
	}
	if !pkg.Spec.Deployment.IsEmpty() {
		// if the deployment archive is not empty,
		// we assume it's a deployable package no matter
		// the source archive is empty or not.
		pkg.Status.BuildStatus = fv1.BuildStatusNone
	} else if !pkg.Spec.Source.IsEmpty() {
		pkg.Status.BuildStatus = fv1.BuildStatusPending
	} else {
		// mark package failed since we cannot do anything with it.
		pkg.Status.BuildStatus = fv1.BuildStatusFailed
		pkg.Status.BuildLog = "Both deploy and source archive are empty"
	}

	// TODO: use UpdateStatus to update status
	return fissionClient.CoreV1().Packages(pkg.Namespace).Update(ctx, pkg, metav1.UpdateOptions{})
}

// scaleBuilderForDemand scales the builder deployment UP toward the number of
// concurrent in-flight builds, capped at the environment's builder pool size
// (spec.builder.poolsize, default 1). It never scales down — that is left to the
// idle reaper (which returns the deployment to zero once idle) so that a pod
// running a build is never terminated underneath it.
func (pkgw *packageWatcher) scaleBuilderForDemand(ctx context.Context, env *fv1.Environment, activeBuilds int32) error {
	builderNs := pkgw.nsResolver.GetBuilderNS(env.Namespace)
	builderName := fmt.Sprintf("%v-%v", env.Name, env.ResourceVersion)

	maxPods := builderPoolSize(env)
	desired := activeBuilds
	if desired < 1 {
		desired = 1
	}
	if desired > maxPods {
		desired = maxPods
	}

	scale, err := pkgw.k8sClient.AppsV1().Deployments(builderNs).GetScale(ctx, builderName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get builder deployment scale: %w", err)
	}

	if scale.Spec.Replicas < desired {
		pkgw.logger.Info("scaling builder deployment up for concurrent builds",
			zap.String("builder", builderName),
			zap.String("namespace", builderNs),
			zap.Int32("currentReplicas", scale.Spec.Replicas),
			zap.Int32("desiredReplicas", desired),
			zap.Int32("activeBuilds", activeBuilds),
			zap.Int32("maxPods", maxPods))

		scale.Spec.Replicas = desired
		_, err = pkgw.k8sClient.AppsV1().Deployments(builderNs).UpdateScale(ctx, builderName, scale, metav1.UpdateOptions{})
		if err != nil {
			return fmt.Errorf("failed to scale builder deployment: %w", err)
		}
	}

	// Refresh last build time so the idle reaper does not scale down during the build.
	pkgw.envWatcher.UpdateLastBuildTime(env.UID)

	return nil
}

// acquireReadyBuilderPod blocks (with backoff) until a builder pod for the
// environment is Ready and not already assigned to another in-flight build, then
// claims it and returns its pod IP. The caller MUST release the pod with
// ReleaseBuilderPod when the build finishes. Returns an error if no free, ready
// pod becomes available before the backoff is exhausted.
func (pkgw *packageWatcher) acquireReadyBuilderPod(ctx context.Context, logger *zap.Logger, env *fv1.Environment, builderNs string) (string, error) {
	backOff := utils.NewDefaultBackOff()
	for backOff.NextExists() {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}

		var informer k8sCache.SharedIndexInformer
		var ok bool
		if informer, ok = pkgw.podInformer[builderNs]; !ok {
			if informer, ok = pkgw.podInformer[metav1.NamespaceAll]; !ok {
				return "", fmt.Errorf("no pod informer found for namespace %s", builderNs)
			}
		}

		// Collect Ready builder pod IPs for this environment.
		var readyIPs []string
		for _, item := range informer.GetStore().List() {
			pod, ok := item.(*apiv1.Pod)
			if !ok {
				continue
			}
			if pod.Labels[LABEL_ENV_NAME] != env.Name ||
				pod.Labels[LABEL_ENV_NAMESPACE] != builderNs ||
				pod.Labels[LABEL_ENV_RESOURCEVERSION] != env.ResourceVersion {
				continue
			}
			if pod.Status.PodIP == "" {
				continue
			}
			// Pod may be "Running" but not yet pass health checks, so use
			// ContainerStatuses readiness rather than pod.Status.Phase.
			podIsReady := len(pod.Status.ContainerStatuses) > 0
			for _, cStatus := range pod.Status.ContainerStatuses {
				podIsReady = podIsReady && cStatus.Ready
			}
			if podIsReady {
				readyIPs = append(readyIPs, pod.Status.PodIP)
			}
		}

		// Claim a ready pod that no other build is using.
		if ip, claimed := pkgw.envWatcher.ClaimFreeBuilderPod(env.UID, readyIPs); claimed {
			return ip, nil
		}

		// No free ready pod yet: pods may still be starting up, or every ready
		// pod is busy with another build (we are at the pool cap and must queue).
		logger.Info("waiting for a free ready builder pod",
			zap.Int("readyPods", len(readyIPs)))
		time.Sleep(backOff.GetNext())
	}
	return "", fmt.Errorf("timed out waiting for a free builder pod")
}
