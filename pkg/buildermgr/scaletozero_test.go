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
	busy.buildInProgress.Store(true)
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

func TestEnsureBuilderReady(t *testing.T) {
	logger := loggerfactory.GetLogger()
	nsResolver := utils.DefaultNSResolver()
	bns := nsResolver.GetBuilderNS("default")

	env := &fv1.Environment{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "nodejs",
			Namespace:       "default",
			ResourceVersion: "1000",
			UID:             types.UID("nodejs"),
		},
	}
	builderKey := bns + "/nodejs-1000"

	t.Run("scales from zero to one", func(t *testing.T) {
		cs, st := newScaleFake(map[string]int32{builderKey: 0})
		envw := &environmentWatcher{
			logger:           logger,
			cache:            map[types.UID]*builderInfo{},
			kubernetesClient: cs,
			nsResolver:       nsResolver,
		}
		// Seed a cache entry so UpdateLastBuildTime has somewhere to write.
		bi := newBuilderInfo("nodejs", "1000", 600, time.Time{})
		envw.cache[env.UID] = bi
		pkgw := &packageWatcher{logger: logger, k8sClient: cs, nsResolver: nsResolver, envWatcher: envw}

		if err := pkgw.ensureBuilderReady(t.Context(), env); err != nil {
			t.Fatalf("ensureBuilderReady returned error: %v", err)
		}
		if r, _ := st.get(builderKey); r != 1 {
			t.Errorf("expected builder scaled to 1, got %d", r)
		}
		if envw.GetLastBuildTime(env.UID).IsZero() {
			t.Errorf("expected lastBuildTime to be updated")
		}
	})

	t.Run("no scale when already at one", func(t *testing.T) {
		cs, st := newScaleFake(map[string]int32{builderKey: 1})
		envw := &environmentWatcher{
			logger:           logger,
			cache:            map[types.UID]*builderInfo{},
			kubernetesClient: cs,
			nsResolver:       nsResolver,
		}
		envw.cache[env.UID] = newBuilderInfo("nodejs", "1000", 600, time.Time{})
		pkgw := &packageWatcher{logger: logger, k8sClient: cs, nsResolver: nsResolver, envWatcher: envw}

		if err := pkgw.ensureBuilderReady(t.Context(), env); err != nil {
			t.Fatalf("ensureBuilderReady returned error: %v", err)
		}
		if c := st.updateCount(builderKey); c != 0 {
			t.Errorf("expected no scale call when already at 1, got %d", c)
		}
	})
}
