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
	"os"
	"strconv"
	"sync"
	"time"

	"go.uber.org/zap"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	apiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	k8sCache "k8s.io/client-go/tools/cache"

	fv1 "github.com/fission/fission/pkg/apis/core/v1"
	"github.com/fission/fission/pkg/crd"
	"github.com/fission/fission/pkg/executor/util"
	fetcherConfig "github.com/fission/fission/pkg/fetcher/config"
	"github.com/fission/fission/pkg/generated/clientset/versioned"
	"github.com/fission/fission/pkg/utils"
	"github.com/fission/fission/pkg/utils/manager"
)

const (
	LABEL_ENV_NAME            = "envName"
	LABEL_ENV_NAMESPACE       = "envNamespace"
	LABEL_ENV_RESOURCEVERSION = "envResourceVersion"
	LABEL_DEPLOYMENT_OWNER    = "owner"
	BUILDER_MGR               = "buildermgr"

	// DefaultBuilderIdleTimeout is the default idle timeout in seconds for builders
	DefaultBuilderIdleTimeout = 600

	// DefaultBuilderPoolSize is the default maximum number of builder pods per environment
	DefaultBuilderPoolSize int32 = 1
)

// builderPoolSize returns the MAXIMUM number of builder pods allowed for an
// environment. Builder pods are provisioned on demand (one per concurrent
// build) up to this cap; it is not a fixed replica count. Defaults to
// DefaultBuilderPoolSize (1) when unset or < 1, which preserves the original
// single-builder, one-build-at-a-time behaviour.
func builderPoolSize(env *fv1.Environment) int32 {
	if env.Spec.Builder.PoolSize != nil && *env.Spec.Builder.PoolSize >= 1 {
		return *env.Spec.Builder.PoolSize
	}
	return DefaultBuilderPoolSize
}

var (
	deletePropagation = metav1.DeletePropagationBackground
	delOpt            = metav1.DeleteOptions{PropagationPolicy: &deletePropagation}
)

type (
	builderInfo struct {
		envMetadata   *metav1.ObjectMeta
		deployment    *appsv1.Deployment
		service       *apiv1.Service
		idleTimeout   int64
		lastBuildTime time.Time
		// activeBuilds is the number of in-flight builds for this environment.
		// It drives demand-based scaling (one builder pod per concurrent build,
		// capped by the env's builder pool size) and keeps the idle reaper from
		// scaling the builder down while builds are running.
		activeBuilds int
		// busyPodIPs is the set of builder pod IPs currently assigned to a build,
		// so concurrent builds each get their own dedicated pod.
		busyPodIPs map[string]bool
		mu         sync.Mutex
	}

	environmentWatcher struct {
		logger                 *zap.Logger
		cache                  map[types.UID]*builderInfo
		cacheMu                sync.RWMutex
		kubernetesClient       kubernetes.Interface
		nsResolver             *utils.NamespaceResolver
		fetcherConfig          *fetcherConfig.Config
		builderImagePullPolicy apiv1.PullPolicy
		useIstio               bool
		podSpecPatch           *apiv1.PodSpec
		envWatchInformer       map[string]k8sCache.SharedIndexInformer
		enableOwnerReferences  bool
		builderReaperInterval  time.Duration
	}
)

func makeEnvironmentWatcher(
	ctx context.Context,
	logger *zap.Logger,
	fissionClient versioned.Interface,
	kubernetesClient kubernetes.Interface,
	fetcherConfig *fetcherConfig.Config,
	podSpecPatch *apiv1.PodSpec,
	builderReaperInterval time.Duration) (*environmentWatcher, error) {

	useIstio := false
	enableIstio := os.Getenv("ENABLE_ISTIO")
	if len(enableIstio) > 0 {
		istio, err := strconv.ParseBool(enableIstio)
		if err != nil {
			logger.Error("Failed to parse ENABLE_ISTIO, defaults to false")
		}
		useIstio = istio
	}

	builderImagePullPolicy := utils.GetImagePullPolicy(os.Getenv("BUILDER_IMAGE_PULL_POLICY"))

	envWatcher := &environmentWatcher{
		logger:                 logger.Named("environment_watcher"),
		cache:                  make(map[types.UID]*builderInfo),
		kubernetesClient:       kubernetesClient,
		nsResolver:             utils.DefaultNSResolver(),
		builderImagePullPolicy: builderImagePullPolicy,
		useIstio:               useIstio,
		fetcherConfig:          fetcherConfig,
		podSpecPatch:           podSpecPatch,
		envWatchInformer:       utils.GetInformersForNamespaces(fissionClient, time.Minute*30, fv1.EnvironmentResource),
		enableOwnerReferences:  utils.IsOwnerReferencesEnabled(),
		builderReaperInterval:  builderReaperInterval,
	}

	err := envWatcher.EnvWatchEventHandlers(ctx)
	if err != nil {
		return nil, err
	}
	return envWatcher, nil
}

func (env *environmentWatcher) getDeploymentLabels(envName string) map[string]string {
	return map[string]string{
		LABEL_DEPLOYMENT_OWNER: BUILDER_MGR,
		LABEL_ENV_NAME:         envName,
	}
}

func (envw *environmentWatcher) getLabels(envName string, envNamespace string, envResourceVersion string) map[string]string {
	return map[string]string{
		LABEL_ENV_NAME:            envName,
		LABEL_ENV_NAMESPACE:       envNamespace,
		LABEL_ENV_RESOURCEVERSION: envResourceVersion,
		LABEL_DEPLOYMENT_OWNER:    BUILDER_MGR,
	}
}

func (envw *environmentWatcher) Run(ctx context.Context, mgr manager.Interface) {
	mgr.AddInformers(ctx, envw.envWatchInformer)
	go envw.idleBuilderReaper(ctx)
}

func (envw *environmentWatcher) EnvWatchEventHandlers(ctx context.Context) error {
	for _, informer := range envw.envWatchInformer {
		_, err := informer.AddEventHandler(k8sCache.ResourceEventHandlerFuncs{
			AddFunc: func(obj any) {
				envObj := obj.(*fv1.Environment)
				envw.AddUpdateBuilder(ctx, envObj)
			},
			UpdateFunc: func(oldObj any, newObj any) {
				oldEnvObj := oldObj.(*fv1.Environment)
				newEnvObj := newObj.(*fv1.Environment)
				if oldEnvObj.ResourceVersion != newEnvObj.ResourceVersion {
					envw.AddUpdateBuilder(ctx, newEnvObj)
				}
			},
			DeleteFunc: func(obj any) {
				envObj := obj.(*fv1.Environment)
				envw.DeleteBuilder(ctx, envObj)
			},
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func (envw *environmentWatcher) AddUpdateBuilder(ctx context.Context, env *fv1.Environment) {
	// builder is not supported with v1 interface and ignore env without builder image
	if env.Spec.Version == 1 || len(env.Spec.Builder.Image) == 0 {
		return
	}
	key := crd.CacheKeyUIDFromMeta(&env.ObjectMeta)
	// On update the older builder is deleted before the new one is created.
	if _, ok := envw.getBuilderInfo(key); ok {
		envw.DeleteBuilder(ctx, env)
	}
	// createBuilder performs API calls, so it runs without holding cacheMu.
	builderInfo, err := envw.createBuilder(ctx, env, envw.nsResolver.GetBuilderNS(env.Namespace))
	if err != nil {
		envw.logger.Error("error creating/updating builder service", zap.Error(err))
		return
	}
	envw.setBuilderInfo(key, builderInfo)
}

func (envw *environmentWatcher) DeleteBuilder(ctx context.Context, env *fv1.Environment) {
	key := crd.CacheKeyUIDFromMeta(&env.ObjectMeta)
	if _, ok := envw.getBuilderInfo(key); !ok {
		envw.logger.Debug("builder service not found", zap.String("env_name", env.Name), zap.String("namespace", envw.nsResolver.GetBuilderNS(env.Namespace)))
		return
	}
	envw.DeleteBuilderService(ctx, env)
	envw.DeleteBuilderDeployment(ctx, env)
	envw.deleteBuilderInfo(key)
	envw.logger.Info("builder service deleted", zap.String("env_name", env.Name), zap.String("namespace", envw.nsResolver.GetBuilderNS(env.Namespace)))
}

func (envw *environmentWatcher) DeleteBuilderService(ctx context.Context, env *fv1.Environment) {
	ns := envw.nsResolver.GetBuilderNS(env.Namespace)
	svcList, err := envw.getBuilderServiceList(ctx, envw.getDeploymentLabels(env.Name), ns)
	if err != nil {
		envw.logger.Error("error getting the builder service list", zap.Error(err))
	}
	for _, svc := range svcList {
		err := envw.deleteBuilderServiceByName(ctx, svc.Name, svc.Namespace)
		if err != nil {
			envw.logger.Error("error removing builder service", zap.Error(err),
				zap.String("service_name", svc.Name),
				zap.String("service_namespace", svc.Namespace),
				zap.String("env_name", svc.Labels[LABEL_ENV_NAME]))
		}
		break
	}
}

func (envw *environmentWatcher) DeleteBuilderDeployment(ctx context.Context, env *fv1.Environment) {
	ns := envw.nsResolver.GetBuilderNS(env.Namespace)
	deployList, err := envw.getBuilderDeploymentList(ctx, envw.getDeploymentLabels(env.Name), ns)
	if err != nil {
		envw.logger.Error("error getting the builder deployment list", zap.Error(err))
	}
	for _, deploy := range deployList {
		err := envw.deleteBuilderDeploymentByName(ctx, deploy.Name, deploy.Namespace)
		if err != nil {
			envw.logger.Error("error removing builder deployment", zap.Error(err),
				zap.String("deployment_name", deploy.Name),
				zap.String("deployment_namespace", deploy.Namespace))
		}
		break
	}
}

// getBuilderInfo returns the cached builderInfo for an environment UID.
func (envw *environmentWatcher) getBuilderInfo(uid types.UID) (*builderInfo, bool) {
	envw.cacheMu.RLock()
	defer envw.cacheMu.RUnlock()
	bi, ok := envw.cache[uid]
	return bi, ok
}

// setBuilderInfo stores the builderInfo for an environment UID.
func (envw *environmentWatcher) setBuilderInfo(uid types.UID, bi *builderInfo) {
	envw.cacheMu.Lock()
	defer envw.cacheMu.Unlock()
	envw.cache[uid] = bi
}

// deleteBuilderInfo removes the cached builderInfo for an environment UID.
func (envw *environmentWatcher) deleteBuilderInfo(uid types.UID) {
	envw.cacheMu.Lock()
	defer envw.cacheMu.Unlock()
	delete(envw.cache, uid)
}

// listBuilderInfo returns a snapshot of all cached builders so callers can
// iterate without holding cacheMu across slow operations.
func (envw *environmentWatcher) listBuilderInfo() []*builderInfo {
	envw.cacheMu.RLock()
	defer envw.cacheMu.RUnlock()
	out := make([]*builderInfo, 0, len(envw.cache))
	for _, bi := range envw.cache {
		out = append(out, bi)
	}
	return out
}

func (envw *environmentWatcher) createBuilder(ctx context.Context, env *fv1.Environment, ns string) (*builderInfo, error) {
	// Ensure builder service account exists
	utils.EnsureBuilderSA(ctx, envw.kubernetesClient, envw.logger, ns)

	var svc *apiv1.Service
	var deploy *appsv1.Deployment

	sel := envw.getLabels(env.Name, ns, env.ResourceVersion)

	svcList, err := envw.getBuilderServiceList(ctx, sel, ns)
	if err != nil {
		return nil, err
	}
	// there should be only one service in svcList
	if len(svcList) == 0 {
		svc, err = envw.createBuilderService(ctx, env, ns)
		if err != nil {
			return nil, fmt.Errorf("error creating builder service for environment in namespace %s %s: %w", env.Name, ns, err)
		}
	} else if len(svcList) == 1 {
		svc = &svcList[0]
	} else {
		return nil, fmt.Errorf("found more than one builder service for environment in namespace %s %s", env.Name, ns)
	}

	deployList, err := envw.getBuilderDeploymentList(ctx, sel, ns)
	if err != nil {
		return nil, err
	}
	// there should be only one deploy in deployList
	if len(deployList) == 0 {
		deploy, err = envw.createBuilderDeployment(ctx, env, ns)
		if err != nil {
			return nil, fmt.Errorf("error creating builder deployment for environment in namespace %s %s: %w", env.Name, ns, err)
		}
	} else if len(deployList) == 1 {
		deploy = &deployList[0]
	} else {
		return nil, fmt.Errorf("found more than one builder deployment for environment in namespace %s %s", env.Name, ns)
	}

	idleTimeout := int64(DefaultBuilderIdleTimeout)
	if env.Spec.Builder.IdleTimeout != nil {
		idleTimeout = *env.Spec.Builder.IdleTimeout
	}

	return &builderInfo{
		envMetadata:   &env.ObjectMeta,
		service:       svc,
		deployment:    deploy,
		idleTimeout:   idleTimeout,
		lastBuildTime: time.Now(),
		busyPodIPs:    make(map[string]bool),
	}, nil
}

func (envw *environmentWatcher) deleteBuilderServiceByName(ctx context.Context, name, namespace string) error {
	err := envw.kubernetesClient.CoreV1().
		Services(namespace).
		Delete(ctx, name, delOpt)
	if err != nil {
		return fmt.Errorf("error deleting builder service %s.%s: %w", name, namespace, err)
	}
	return nil
}

func (envw *environmentWatcher) deleteBuilderDeploymentByName(ctx context.Context, name, namespace string) error {
	err := envw.kubernetesClient.AppsV1().
		Deployments(namespace).
		Delete(ctx, name, delOpt)
	if err != nil {
		return fmt.Errorf("error deleting builder deployment %s.%s: %w", name, namespace, err)
	}
	return nil
}

func (envw *environmentWatcher) getBuilderServiceList(ctx context.Context, sel map[string]string, ns string) ([]apiv1.Service, error) {
	svcList, err := envw.kubernetesClient.CoreV1().Services(ns).List(
		ctx,
		metav1.ListOptions{
			LabelSelector: labels.Set(sel).AsSelector().String(),
		})
	if err != nil {
		return nil, fmt.Errorf("error getting builder service list for namespace %s: %w", ns, err)
	}
	return svcList.Items, nil
}

func (envw *environmentWatcher) createBuilderService(ctx context.Context, env *fv1.Environment, ns string) (*apiv1.Service, error) {
	name := fmt.Sprintf("%v-%v", env.Name, env.ResourceVersion)
	sel := envw.getLabels(env.Name, ns, env.ResourceVersion)
	var ownerReferences []metav1.OwnerReference
	if envw.enableOwnerReferences {
		ownerReferences = []metav1.OwnerReference{
			*metav1.NewControllerRef(env, schema.GroupVersionKind{
				Group:   "fission.io",
				Version: "v1",
				Kind:    "Environment",
			}),
		}
	}
	service := apiv1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       ns,
			Name:            name,
			Labels:          sel,
			OwnerReferences: ownerReferences,
		},
		Spec: apiv1.ServiceSpec{
			Selector: sel,
			Type:     apiv1.ServiceTypeClusterIP,
			Ports: []apiv1.ServicePort{
				{
					Name:     "fetcher-port",
					Protocol: apiv1.ProtocolTCP,
					Port:     8000,
					TargetPort: intstr.IntOrString{
						Type:   intstr.Int,
						IntVal: 8000,
					},
				},
				{
					Name:     "builder-port",
					Protocol: apiv1.ProtocolTCP,
					Port:     8001,
					TargetPort: intstr.IntOrString{
						Type:   intstr.Int,
						IntVal: 8001,
					},
				},
			},
		},
	}
	envw.logger.Info("creating builder service", zap.String("service_name", name))
	_, err := envw.kubernetesClient.CoreV1().Services(ns).Create(ctx, &service, metav1.CreateOptions{})
	if err != nil {
		return nil, err
	}
	return &service, nil
}

func (envw *environmentWatcher) getBuilderDeploymentList(ctx context.Context, sel map[string]string, ns string) ([]appsv1.Deployment, error) {
	deployList, err := envw.kubernetesClient.AppsV1().Deployments(ns).List(
		ctx,
		metav1.ListOptions{
			LabelSelector: labels.Set(sel).AsSelector().String(),
		})
	if err != nil {
		return nil, fmt.Errorf("error getting builder deployment list for namespace %s: %w", ns, err)
	}
	return deployList.Items, nil
}

func (envw *environmentWatcher) createBuilderDeployment(ctx context.Context, env *fv1.Environment, ns string) (*appsv1.Deployment, error) {
	name := fmt.Sprintf("%v-%v", env.Name, env.ResourceVersion)
	sel := envw.getLabels(env.Name, ns, env.ResourceVersion)
	// Start with a single warm builder pod. Additional pods are provisioned on
	// demand (up to spec.builder.poolsize) when concurrent builds arrive, and
	// the idle reaper scales back to zero once builds stop.
	var replicas int32 = 1

	podAnnotations := env.Annotations
	if podAnnotations == nil {
		podAnnotations = make(map[string]string)
	}
	if envw.useIstio && env.Spec.AllowAccessToExternalNetwork {
		podAnnotations["sidecar.istio.io/inject"] = "false"
	}

	container, err := util.MergeContainer(&apiv1.Container{
		Name:                   fv1.BuilderContainerName,
		Image:                  env.Spec.Builder.Image,
		ImagePullPolicy:        envw.builderImagePullPolicy,
		TerminationMessagePath: "/dev/termination-log",
		Command:                []string{"/builder", envw.fetcherConfig.SharedMountPath()},
		ReadinessProbe: &apiv1.Probe{
			InitialDelaySeconds: 5,
			PeriodSeconds:       2,
			ProbeHandler: apiv1.ProbeHandler{
				HTTPGet: &apiv1.HTTPGetAction{
					Path: "/healthz",
					Port: intstr.IntOrString{
						Type:   intstr.Int,
						IntVal: 8001,
					},
				},
			},
		},
	}, env.Spec.Builder.Container)
	if err != nil {
		return nil, err
	}

	pod := apiv1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Labels:      sel,
			Annotations: podAnnotations,
		},
		Spec: apiv1.PodSpec{
			Containers:         []apiv1.Container{*container},
			ServiceAccountName: fv1.FissionBuilderSA,
		},
	}

	if envw.podSpecPatch != nil {

		updatedPodSpec, err := util.MergePodSpec(&pod.Spec, envw.podSpecPatch)
		if err == nil {
			pod.Spec = *updatedPodSpec
		} else {
			envw.logger.Warn("Failed to merge the specs: %v", zap.Error(err))
		}
	}

	pod.Spec = *(util.ApplyImagePullSecret(env.Spec.ImagePullSecret, pod.Spec))

	var ownerReferences []metav1.OwnerReference
	if envw.enableOwnerReferences {
		ownerReferences = []metav1.OwnerReference{
			*metav1.NewControllerRef(env, schema.GroupVersionKind{
				Group:   "fission.io",
				Version: "v1",
				Kind:    "Environment",
			}),
		}
	}

	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       ns,
			Name:            name,
			Labels:          sel,
			OwnerReferences: ownerReferences,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: sel,
			},
			Template: pod,
		},
	}

	err = envw.fetcherConfig.AddFetcherToPodSpec(&deployment.Spec.Template.Spec, "builder")
	if err != nil {
		return nil, err
	}

	if env.Spec.Builder.PodSpec != nil {
		newPodSpec, err := util.MergePodSpec(&deployment.Spec.Template.Spec, env.Spec.Builder.PodSpec)
		if err != nil {
			return nil, err
		}
		deployment.Spec.Template.Spec = *newPodSpec
	}

	_, err = envw.kubernetesClient.AppsV1().Deployments(ns).Create(ctx, deployment, metav1.CreateOptions{})
	if err != nil {
		return nil, err
	}

	envw.logger.Info("creating builder deployment", zap.String("deployment", name))

	return deployment, nil
}

// ScaleBuilderDeployment scales a builder deployment to the specified number of replicas
func (envw *environmentWatcher) ScaleBuilderDeployment(ctx context.Context, ns, name string, replicas int32) error {
	scale := &autoscalingv1.Scale{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
		},
		Spec: autoscalingv1.ScaleSpec{
			Replicas: replicas,
		},
	}
	_, err := envw.kubernetesClient.AppsV1().Deployments(ns).UpdateScale(ctx, name, scale, metav1.UpdateOptions{})
	if err != nil {
		envw.logger.Error("error scaling builder deployment",
			zap.String("deployment", name),
			zap.String("namespace", ns),
			zap.Int32("replicas", replicas),
			zap.Error(err))
		return err
	}
	envw.logger.Info("scaled builder deployment",
		zap.String("deployment", name),
		zap.String("namespace", ns),
		zap.Int32("replicas", replicas))
	return nil
}

// idleBuilderReaper runs the builder reaper loop to scale down idle builders
func (envw *environmentWatcher) idleBuilderReaper(ctx context.Context) {
	envw.logger.Info("starting idle builder reaper",
		zap.Duration("interval", envw.builderReaperInterval))
	wait.UntilWithContext(ctx, envw.doIdleBuilderReaper, envw.builderReaperInterval)
}

// doIdleBuilderReaper checks all cached builders and scales idle ones to zero.
// It snapshots the cache under cacheMu and then performs the scale calls without
// holding the lock, so env add/update/delete events are not blocked by the sweep.
func (envw *environmentWatcher) doIdleBuilderReaper(ctx context.Context) {
	for _, bi := range envw.listBuilderInfo() {
		// Skip if any build is in progress for this environment.
		if bi.isBuilding() {
			continue
		}

		// idleTimeout of 0 means "never scale to zero".
		if bi.idleTimeout == 0 {
			continue
		}

		envMeta := bi.envMetadata
		if envMeta == nil {
			continue
		}

		bi.mu.Lock()
		lastBuildTime := bi.lastBuildTime
		bi.mu.Unlock()

		if time.Since(lastBuildTime) < time.Duration(bi.idleTimeout)*time.Second {
			continue
		}

		// Re-check just before scaling to narrow the race with a build that
		// started after the snapshot was taken.
		if bi.isBuilding() {
			continue
		}

		builderNS := envw.nsResolver.GetBuilderNS(envMeta.Namespace)
		builderName := fmt.Sprintf("%v-%v", envMeta.Name, envMeta.ResourceVersion)
		if err := envw.ScaleBuilderDeployment(ctx, builderNS, builderName, 0); err != nil {
			envw.logger.Error("failed to scale builder to zero",
				zap.String("builder", builderName),
				zap.String("namespace", builderNS),
				zap.Error(err))
		}
	}
}

// GetLastBuildTime returns the last build time for a builder
func (envw *environmentWatcher) GetLastBuildTime(envUID types.UID) time.Time {
	bi, ok := envw.getBuilderInfo(envUID)
	if !ok {
		return time.Time{}
	}
	bi.mu.Lock()
	defer bi.mu.Unlock()
	return bi.lastBuildTime
}

// UpdateLastBuildTime updates the last build time for a builder
func (envw *environmentWatcher) UpdateLastBuildTime(envUID types.UID) {
	bi, ok := envw.getBuilderInfo(envUID)
	if !ok {
		return
	}
	bi.mu.Lock()
	defer bi.mu.Unlock()
	bi.lastBuildTime = time.Now()
}

// isBuilding reports whether any build is currently in progress for this builder.
func (bi *builderInfo) isBuilding() bool {
	bi.mu.Lock()
	defer bi.mu.Unlock()
	return bi.activeBuilds > 0
}

// IncActiveBuilds records that a build has started for an environment and
// returns the new in-flight build count. It also refreshes lastBuildTime so the
// idle reaper does not scale the builder down underneath an active build.
func (envw *environmentWatcher) IncActiveBuilds(envUID types.UID) int32 {
	bi, ok := envw.getBuilderInfo(envUID)
	if !ok {
		return 1
	}
	bi.mu.Lock()
	defer bi.mu.Unlock()
	bi.activeBuilds++
	bi.lastBuildTime = time.Now()
	return int32(bi.activeBuilds)
}

// DecActiveBuilds records that a build has finished for an environment and
// refreshes lastBuildTime so the idle timer starts from the last completed build.
func (envw *environmentWatcher) DecActiveBuilds(envUID types.UID) {
	bi, ok := envw.getBuilderInfo(envUID)
	if !ok {
		return
	}
	bi.mu.Lock()
	defer bi.mu.Unlock()
	if bi.activeBuilds > 0 {
		bi.activeBuilds--
	}
	bi.lastBuildTime = time.Now()
}

// ClaimFreeBuilderPod picks the first candidate builder pod IP that is not
// already assigned to another in-flight build, marks it busy, and returns it.
// Returns false if every candidate is already busy (caller should wait/retry).
func (envw *environmentWatcher) ClaimFreeBuilderPod(envUID types.UID, candidateIPs []string) (string, bool) {
	bi, ok := envw.getBuilderInfo(envUID)
	if !ok {
		return "", false
	}
	bi.mu.Lock()
	defer bi.mu.Unlock()
	if bi.busyPodIPs == nil {
		bi.busyPodIPs = make(map[string]bool)
	}
	for _, ip := range candidateIPs {
		if ip == "" || bi.busyPodIPs[ip] {
			continue
		}
		bi.busyPodIPs[ip] = true
		return ip, true
	}
	return "", false
}

// ReleaseBuilderPod frees a builder pod IP previously claimed via ClaimFreeBuilderPod.
func (envw *environmentWatcher) ReleaseBuilderPod(envUID types.UID, podIP string) {
	bi, ok := envw.getBuilderInfo(envUID)
	if !ok {
		return
	}
	bi.mu.Lock()
	defer bi.mu.Unlock()
	delete(bi.busyPodIPs, podIP)
}
