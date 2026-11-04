// SPDX-FileCopyrightText: The Fission Authors
//
// SPDX-License-Identifier: Apache-2.0

package fetcher

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/go-logr/logr"
	apiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/fission/fission/pkg/svcinfo"
)

// fetcherContainerName is this sidecar's container name in every function pod
// (config.addFetcherToPodSpecWithCommand); every other container is runtime.
const fetcherContainerName = "fetcher"

// Specialization lives in the runtime container's memory, but the kubelet
// restarts a crashed or OOM-killed container IN PLACE: the pod, this sidecar
// and its readiness survive, while the new runtime process comes up generic
// (no user code). Without a watchdog that pod keeps receiving traffic it
// cannot serve until someone deletes it - a federated subgraph then answers
// with an empty schema and the gateway cannot compose.
//
// The watchdog notices the restart (the runtime port refusing connections,
// confirmed by the pod's container restart count; a slow backstop check
// catches a restart too fast to see) and replays the last successful
// specialization: the deploy archive and secrets are still on the shared
// volumes, so Fetch is skipped and only the load call is repeated. The pod
// is not ready while that happens.
type runtimeWatchdog struct {
	logger logr.Logger
	// restarts returns the summed restart count of the runtime containers.
	restarts func(ctx context.Context) (int32, error)
	// alive reports whether the runtime port accepts connections.
	alive func(ctx context.Context) bool
	// respecialize replays the last successful specialization.
	respecialize func(ctx context.Context) error
	setReady     func(ready bool)

	probeEvery   time.Duration // runtime port check
	confirmEvery time.Duration // restart-count check while suspicious
	suspectFor   time.Duration // how long a refused port stays suspicious
	backstop     time.Duration // restart-count check with no suspicion
	retryMax     time.Duration // cap of the re-specialize backoff
}

func (w *runtimeWatchdog) run(ctx context.Context, baseline int32) {
	var suspectUntil, nextConfirm time.Time
	nextBackstop := time.Now().Add(w.backstop)
	for sleepCtx(ctx, w.probeEvery) {
		now := time.Now()
		if !w.alive(ctx) {
			suspectUntil = now.Add(w.suspectFor)
			continue // still down; check the count once it is back
		}
		confirm := now.Before(suspectUntil) && !now.Before(nextConfirm)
		if !confirm && now.Before(nextBackstop) {
			continue
		}
		nextConfirm = now.Add(w.confirmEvery)
		nextBackstop = now.Add(w.backstop)

		count, err := w.restarts(ctx)
		if err != nil {
			w.logger.Error(err, "runtime watchdog: reading pod status failed")
			continue
		}
		if count <= baseline {
			continue
		}
		w.logger.Info("runtime container restarted - re-specializing", "restarts", count, "previous", baseline)
		w.setReady(false)
		if !w.recover(ctx) {
			return
		}
		baseline = count
		suspectUntil = time.Time{}
		w.setReady(true)
		w.logger.Info("runtime re-specialized after restart", "restarts", count)
	}
}

// recover retries the re-specialization until it works (false = ctx done).
func (w *runtimeWatchdog) recover(ctx context.Context) bool {
	delay := time.Second
	for attempt := 1; ; attempt++ {
		err := w.respecialize(ctx)
		if err == nil {
			return true
		}
		w.logger.Error(err, "runtime watchdog: re-specialize failed, retrying", "attempt", attempt, "in", delay)
		if !sleepCtx(ctx, delay) {
			return false
		}
		delay = min(delay*2, w.retryMax)
	}
}

func runtimeRestarts(pod *apiv1.Pod) int32 {
	var total int32
	for _, s := range pod.Status.ContainerStatuses {
		if s.Name != fetcherContainerName {
			total += s.RestartCount
		}
	}
	return total
}

func (fetcher *Fetcher) podRuntimeRestarts(ctx context.Context) (int32, error) {
	pod, err := fetcher.kubeClient.CoreV1().Pods(fetcher.Info.Namespace).Get(ctx, fetcher.Info.Name, metav1.GetOptions{})
	if err != nil {
		return 0, fmt.Errorf("get pod %s/%s: %w", fetcher.Info.Namespace, fetcher.Info.Name, err)
	}
	return runtimeRestarts(pod), nil
}

func runtimePortAlive(ctx context.Context) bool {
	d := net.Dialer{Timeout: 500 * time.Millisecond}
	conn, err := d.DialContext(ctx, "tcp", fmt.Sprintf("127.0.0.1:%d", svcinfo.PortEnvRuntime))
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// EnableRuntimeWatchdog makes WatchRuntime start a watchdog that lives for
// ctx and flips this pod's readiness with setReady.
func (fetcher *Fetcher) EnableRuntimeWatchdog(ctx context.Context, setReady func(bool)) {
	fetcher.watchdogCtx = ctx
	fetcher.setReady = setReady
}

// WatchRuntime starts the runtime watchdog for a pod that was just
// specialized with fetchReq/loadReq. Only the first call per pod starts it
// (a pod is specialized once); a no-op until EnableRuntimeWatchdog.
func (fetcher *Fetcher) WatchRuntime(fetchReq FunctionFetchRequest, loadReq FunctionLoadRequest) {
	ctx, setReady := fetcher.watchdogCtx, fetcher.setReady
	if ctx == nil || setReady == nil || !fetcher.watchdogStarted.CompareAndSwap(false, true) {
		return
	}
	logger := fetcher.logger.WithName("runtime-watchdog")
	baseline, err := fetcher.podRuntimeRestarts(ctx)
	if err != nil {
		// No pod read (RBAC / API): stay up, but without the watchdog.
		logger.Error(err, "runtime watchdog disabled")
		return
	}
	w := &runtimeWatchdog{
		logger:   logger,
		restarts: fetcher.podRuntimeRestarts,
		alive:    runtimePortAlive,
		respecialize: func(ctx context.Context) error {
			_, err := fetcher.SpecializePod(ctx, fetchReq, loadReq)
			return err
		},
		setReady:     setReady,
		probeEvery:   time.Second,
		confirmEvery: 2 * time.Second,
		suspectFor:   time.Minute,
		backstop:     time.Minute,
		retryMax:     30 * time.Second,
	}
	go w.run(ctx, baseline)
}
