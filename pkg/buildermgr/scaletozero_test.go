/*
Copyright 2024 The Fission Authors.

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
	"sync"
	"testing"
	"time"

	autoscalingv1 "k8s.io/api/autoscaling/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	fv1 "github.com/fission/fission/pkg/apis/core/v1"
	"github.com/fission/fission/pkg/utils"
	"github.com/fission/fission/pkg/utils/loggerfactory"
)

// scaleTracker keeps an in-memory replica count per "namespace/name" and serves
// the scale subresource via reactors, since the fake clientset does not persist
// scale subresource state on its own.
type scaleTracker struct {
	mu       sync.Mutex
	replicas map[string]int32
	updates  map[string]int
}

func (s *scaleTracker) get(key string) (int32, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.replicas[key]
	return r, ok
}

func (s *scaleTracker) updateCount(key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.updates[key]
}

// newScaleFake returns a fake clientset wired so that GetScale/UpdateScale on
// deployments read and write the provided in-memory replica counts.
func newScaleFake(initial map[string]int32) (*fake.Clientset, *scaleTracker) {
	st := &scaleTracker{replicas: map[string]int32{}, updates: map[string]int{}}
	for k, v := range initial {
		st.replicas[k] = v
	}
	cs := fake.NewClientset()

	cs.PrependReactor("get", "deployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
		ga, ok := action.(k8stesting.GetActionImpl)
		if !ok || ga.GetSubresource() != "scale" {
			return false, nil, nil
		}
		key := ga.GetNamespace() + "/" + ga.GetName()
		r, _ := st.get(key)
		return true, &autoscalingv1.Scale{
			ObjectMeta: metav1.ObjectMeta{Name: ga.GetName(), Namespace: ga.GetNamespace()},
			Spec:       autoscalingv1.ScaleSpec{Replicas: r},
		}, nil
	})

	cs.PrependReactor("update", "deployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
		ua, ok := action.(k8stesting.UpdateActionImpl)
		if !ok || ua.GetSubresource() != "scale" {
			return false, nil, nil
		}
		scale := ua.GetObject().(*autoscalingv1.Scale)
		key := ua.GetNamespace() + "/" + scale.Name
		st.mu.Lock()
		st.replicas[key] = scale.Spec.Replicas
		st.updates[key]++
		st.mu.Unlock()
		return true, scale, nil
	})

	return cs, st
}

func newBuilderInfo(name, rv string, idleTimeout int64, last time.Time) *builderInfo {
	return &builderInfo{
		envMetadata: &metav1.ObjectMeta{
			Name:            name,
			Namespace:       "default",
			ResourceVersion: rv,
			UID:             types.UID(name),
		},
		idleTimeout:   idleTimeout,
		lastBuildTime: last,
	}
}

func TestDoIdleBuilderReaper(t *testing.T) {
	logger := loggerfactory.GetLogger()
	cs, st := newScaleFake(nil)
	nsResolver := utils.DefaultNSResolver()
	bns := nsResolver.GetBuilderNS("default")

	envw := &environmentWatcher{
		logger:           logger,
		cache:            map[types.UID]*builderInfo{},
		kubernetesClient: cs,
		nsResolver:       nsResolver,
	}

	old := time.Now().Add(-time.Hour)

	idle := newBuilderInfo("idle-env", "1000", 600, old)
	busy := newBuilderInfo("busy-env", "1000", 600, old)
	busy.activeBuilds = 1
	fresh := newBuilderInfo("fresh-env", "1000", 600, time.Now())
	never := newBuilderInfo("never-env", "1000", 0, old)

	for _, bi := range []*builderInfo{idle, busy, fresh, never} {
		envw.cache[bi.envMetadata.UID] = bi
	}

	envw.doIdleBuilderReaper(t.Context())

	// Idle builder past its timeout must be scaled to zero.
	if r, ok := st.get(bns + "/idle-env-1000"); !ok || r != 0 {
		t.Errorf("expected idle builder scaled to 0, got replicas=%d present=%v", r, ok)
	}
	// Build in progress, not yet idle, and idleTimeout=0 must all be skipped.
	if c := st.updateCount(bns + "/busy-env-1000"); c != 0 {
		t.Errorf("expected build-in-progress builder to be skipped, got %d scale calls", c)
	}
	if c := st.updateCount(bns + "/fresh-env-1000"); c != 0 {
		t.Errorf("expected fresh builder to be skipped, got %d scale calls", c)
	}
	if c := st.updateCount(bns + "/never-env-1000"); c != 0 {
		t.Errorf("expected idleTimeout=0 builder to never scale down, got %d scale calls", c)
	}
}

func TestScaleBuilderForDemand(t *testing.T) {
	logger := loggerfactory.GetLogger()
	nsResolver := utils.DefaultNSResolver()
	bns := nsResolver.GetBuilderNS("default")
	builderKey := bns + "/nodejs-1000"

	newEnv := func(poolSize *int32) *fv1.Environment {
		e := &fv1.Environment{
			ObjectMeta: metav1.ObjectMeta{
				Name:            "nodejs",
				Namespace:       "default",
				ResourceVersion: "1000",
				UID:             types.UID("nodejs"),
			},
		}
		e.Spec.Builder.PoolSize = poolSize
		return e
	}

	setup := func(initial int32) (*environmentWatcher, *packageWatcher, *scaleTracker) {
		cs, st := newScaleFake(map[string]int32{builderKey: initial})
		envw := &environmentWatcher{
			logger:           logger,
			cache:            map[types.UID]*builderInfo{},
			kubernetesClient: cs,
			nsResolver:       nsResolver,
		}
		// Seed a cache entry so UpdateLastBuildTime has somewhere to write.
		envw.cache[types.UID("nodejs")] = newBuilderInfo("nodejs", "1000", 600, time.Time{})
		pkgw := &packageWatcher{logger: logger, k8sClient: cs, nsResolver: nsResolver, envWatcher: envw}
		return envw, pkgw, st
	}

	size3 := int32(3)

	t.Run("scales from zero to one on first build", func(t *testing.T) {
		envw, pkgw, st := setup(0)
		if err := pkgw.scaleBuilderForDemand(t.Context(), newEnv(nil), 1); err != nil {
			t.Fatalf("scaleBuilderForDemand returned error: %v", err)
		}
		if r, _ := st.get(builderKey); r != 1 {
			t.Errorf("expected builder scaled to 1, got %d", r)
		}
		if envw.GetLastBuildTime(types.UID("nodejs")).IsZero() {
			t.Errorf("expected lastBuildTime to be updated")
		}
	})

	t.Run("scales up to concurrent build count within cap", func(t *testing.T) {
		_, pkgw, st := setup(1)
		if err := pkgw.scaleBuilderForDemand(t.Context(), newEnv(&size3), 2); err != nil {
			t.Fatalf("scaleBuilderForDemand returned error: %v", err)
		}
		if r, _ := st.get(builderKey); r != 2 {
			t.Errorf("expected builder scaled to 2, got %d", r)
		}
	})

	t.Run("caps at builder pool size", func(t *testing.T) {
		_, pkgw, st := setup(1)
		if err := pkgw.scaleBuilderForDemand(t.Context(), newEnv(&size3), 10); err != nil {
			t.Fatalf("scaleBuilderForDemand returned error: %v", err)
		}
		if r, _ := st.get(builderKey); r != 3 {
			t.Errorf("expected builder capped at 3, got %d", r)
		}
	})

	t.Run("never scales down", func(t *testing.T) {
		_, pkgw, st := setup(3)
		if err := pkgw.scaleBuilderForDemand(t.Context(), newEnv(&size3), 1); err != nil {
			t.Fatalf("scaleBuilderForDemand returned error: %v", err)
		}
		if c := st.updateCount(builderKey); c != 0 {
			t.Errorf("expected no scale-down call, got %d", c)
		}
		if r, _ := st.get(builderKey); r != 3 {
			t.Errorf("expected builder to stay at 3, got %d", r)
		}
	})
}

func TestClaimReleaseBuilderPod(t *testing.T) {
	logger := loggerfactory.GetLogger()
	envw := &environmentWatcher{
		logger: logger,
		cache:  map[types.UID]*builderInfo{},
	}
	uid := types.UID("nodejs")
	envw.cache[uid] = newBuilderInfo("nodejs", "1000", 600, time.Now())

	ips := []string{"10.0.0.1", "10.0.0.2"}

	// Two concurrent builds must claim two distinct pods.
	ip1, ok1 := envw.ClaimFreeBuilderPod(uid, ips)
	ip2, ok2 := envw.ClaimFreeBuilderPod(uid, ips)
	if !ok1 || !ok2 || ip1 == ip2 {
		t.Fatalf("expected two distinct claimed pods, got %q (%v) and %q (%v)", ip1, ok1, ip2, ok2)
	}

	// A third build finds no free pod (both busy) — it must queue.
	if ip3, ok3 := envw.ClaimFreeBuilderPod(uid, ips); ok3 {
		t.Errorf("expected no free pod when all are busy, got %q", ip3)
	}

	// Releasing one frees it for the next build.
	envw.ReleaseBuilderPod(uid, ip1)
	if ip4, ok4 := envw.ClaimFreeBuilderPod(uid, ips); !ok4 || ip4 != ip1 {
		t.Errorf("expected to reclaim released pod %q, got %q (%v)", ip1, ip4, ok4)
	}
}

func TestActiveBuildsCounter(t *testing.T) {
	logger := loggerfactory.GetLogger()
	envw := &environmentWatcher{
		logger: logger,
		cache:  map[types.UID]*builderInfo{},
	}
	uid := types.UID("nodejs")
	bi := newBuilderInfo("nodejs", "1000", 600, time.Now())
	envw.cache[uid] = bi

	if n := envw.IncActiveBuilds(uid); n != 1 {
		t.Errorf("expected 1 active build, got %d", n)
	}
	if n := envw.IncActiveBuilds(uid); n != 2 {
		t.Errorf("expected 2 active builds, got %d", n)
	}
	if !bi.isBuilding() {
		t.Errorf("expected isBuilding to be true with active builds")
	}
	envw.DecActiveBuilds(uid)
	envw.DecActiveBuilds(uid)
	if bi.isBuilding() {
		t.Errorf("expected isBuilding to be false after all builds finished")
	}
}
