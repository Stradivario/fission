# Demand-Driven Builder Concurrency (on-demand builder pods, one per build)

**Type:** Feature · **Date:** 2026-06-03 · **Area:** `buildermgr`, `fission-cli`

## Motivation

Each Fission **environment** historically ran exactly **one** builder pod pinned to
`MAX_PARALLEL_BUILDS=1` ("queue mode", to prevent OOM). When several lambdas share one
environment image, concurrent deploys from a team **serialize on that single builder** —
user 1's build must finish before user 2's starts. We want a shared environment to build
concurrently **without forking it into separate environments**, and without paying for
idle capacity.

The [builder scale-to-zero](builder-scale-to-zero.md) feature (idle builder Deployment →
0 after `idleTimeout`, back up on the next build) makes on-demand builder capacity cheap.

## Design — builder pods provisioned on demand, one per concurrent build

`spec.builder.poolsize` is a **maximum**, not a fixed replica count:

- A single build always uses a **single** builder pod (so `poolsize: 10` + one build
  does **not** spin up 10 pods).
- When **N** builds for the same environment are in flight at once, the builder
  Deployment is scaled up on demand to `min(N, poolsize)` pods, and **each build is
  dispatched to its own dedicated pod**.
- If concurrent builds exceed `poolsize`, the extra builds **queue** until a pod frees.
- The builder Deployment **only scales up** while builds run; the idle reaper scales it
  back to **0** once all builds finish (never scaling down mid-build).

`poolsize` defaults to **1**, which reproduces the original single-builder,
one-build-at-a-time behaviour exactly.

### Why per-pod dispatch is required (correctness, not just speed)

A build is three calls — **fetch** (fetcher, :8000) → **build** (builder, :8001) →
**upload** (fetcher, :8000) — and the fetched source archive lives on the **pod's local
volume**. The old code addressed the builder **Service** DNS, so with more than one
replica kube-proxy could load-balance each call to a *different* pod, and the build pod
would not find the fetched source. So multi-pod builds require pinning all three calls to
one pod. buildermgr now resolves Ready builder pods, **claims a free one**, and addresses
it **directly by pod IP** for the whole build. This also guarantees true 1-build-per-pod
parallelism rather than relying on kube-proxy.

## Behavior (buildermgr)

Builds already run as concurrent goroutines (`buildWithCache` → `go build`). Each build:

1. **Counts itself** — `IncActiveBuilds(env)` (and `DecActiveBuilds` on completion). This
   count drives scaling and keeps the idle reaper from scaling down mid-build.
2. **Scales up on demand** — `scaleBuilderForDemand` sets the Deployment to
   `clamp(activeBuilds, 1, poolsize)` replicas (UP only). From an idle (0-replica)
   builder, the first build scales `0→1`.
3. **Claims a dedicated pod** — `acquireReadyBuilderPod` waits (with backoff) for a Ready
   builder pod whose IP is not already assigned to another build, then claims its IP.
   If every Ready pod is busy (at the cap), it queues until one frees.
4. **Builds on that pod IP** — fetch/build/upload all target the claimed pod, then the
   pod IP is released.

Scale-down is owned solely by the idle reaper (→ 0 after `idleTimeout`), so a pod running
a build is never terminated underneath it.

### `MAX_PARALLEL_BUILDS` (M)

With one build per pod, the per-pod `MAX_PARALLEL_BUILDS` semaphore is effectively a
safety valve and stays at its default of **1**. The app still templates it
(`builderMaxParallelBuilds`, default 1) for environments that intentionally want a pod to
accept more than one build; it is independent of `poolsize`.

## Configuration

### Max builder pods (CRD field)

`spec.builder.poolsize` — integer (`*int32`).

| Value | Meaning |
|-------|---------|
| unset (`nil`) | Default **1** (single builder, serial builds) |
| `< 1` | Treated as the default, **1** |
| `N ≥ 1` | Up to **N** builder pods, provisioned on demand (one per concurrent build) |

CLI flag (on `fission env create` / `fission env update`):

```bash
fission env create --name nodejs --image rxdi/fission-node:0.0.14 \
  --builder rxdi/fission-node-builder:1.0.8 --builder-poolsize 5 --version 2
fission env update --name nodejs --builder-poolsize 10
```

Default constant: `DefaultBuilderPoolSize int32 = 1` (`pkg/buildermgr/envwatcher.go`),
resolved by `builderPoolSize(env)` (the cap).

## ⚠️ Critical end-to-end constraint

`spec.builder.poolsize` reaches the cluster only if **both** hold:

1. the **cluster CRD includes `builder.poolsize`** — ship the regenerated
   `crds/v1/fission.io_environments.yaml` (`make update-crds`, i.e.
   `kubectl replace -k crds/v1`), else the API server prunes the unknown field; and
2. whatever creates the environment preserves it. An app that applies environments via
   **`fission spec apply`** deserializes the YAML into the typed `fv1.Environment` struct,
   so its **`fission` CLI binary must be this fork's build** (an older CLI silently drops
   `poolsize` before it reaches the API server).

`MAX_PARALLEL_BUILDS` (M) has **no** such constraint — it's a plain container env var.

## Files

| File | Change |
|------|--------|
| `pkg/apis/core/v1/types.go` | `Builder.PoolSize *int32` (documented as a max) |
| `pkg/apis/core/v1/zz_generated.deepcopy.go` | Regenerated deepcopy |
| `pkg/apis/core/v1/zz_generated.swagger_doc_generated.go` | Regenerated swagger doc |
| `crds/v1/fission.io_environments.yaml` | Regenerated CRD schema (`spec.builder.poolsize`, int32) |
| `pkg/buildermgr/envwatcher.go` | `builderInfo.activeBuilds` + `busyPodIPs`; `Inc/DecActiveBuilds`, `ClaimFreeBuilderPod`/`ReleaseBuilderPod`, `isBuilding`; reaper uses `activeBuilds`; `createBuilderDeployment` creates 1 warm pod |
| `pkg/buildermgr/pkgwatcher.go` | `build` rewritten for demand scaling + per-pod claim; `scaleBuilderForDemand` (up-only), `acquireReadyBuilderPod` (replaces `ensureBuilderReady`) |
| `pkg/buildermgr/common.go` | `buildPackage` takes a `builderPodIP` and pins fetch/build/upload to that pod |
| `pkg/buildermgr/scaletozero_test.go` | Tests for demand scaling, pod claim/release, active-build counter |
| `pkg/fission-cli/...` | `--builder-poolsize` CLI flag |

> Regeneration after editing `pkg/apis/core/v1/types.go`: `make codegen`,
> `make generate-crds`, `make generate-swagger-doc`.

## Testing

- Unit (race): `go test -race ./pkg/buildermgr/...` — demand scale-up (0→1, up to cap,
  caps at `poolsize`, never scales down), pod claim/release gives distinct pods and
  queues when full, active-build counter / `isBuilding`. Scale-to-zero tests unaffected.
- **Live-cluster e2e (pending):**
  1. `make update-crds` so the cluster CRD has `builder.poolsize`.
  2. `fission env create … --builder-poolsize 5 --version 2`. Deploy ONE lambda → exactly
     **1** builder pod is created (not 5).
  3. Trigger **3 concurrent** builds (3 lambdas on that env) → builder Deployment scales
     to 3, three pods, each build runs on its own pod (logs show one build per pod IP).
  4. Trigger more than 5 concurrent → scales to 5, the rest queue, then drain.
  5. Stop building, wait `idleTimeout` → reaper scales to 0; next single build scales 0→1.
  7. `kubectl get environment <name> -o yaml` retains `spec.builder.poolsize`.

## Notes / future

- The builder pool only scales **up** during a burst and back to **0** when idle; it does
  not shrink to a smaller non-zero count between bursts (it would risk killing a pod
  mid-build, since Deployment scale-down can't target idle pods). Acceptable: extra pods
  cost nothing once the burst ends and the reaper zeroes them.
- Per-pod dispatch uses the pod IP from the informer; a pod that dies mid-build fails that
  one build (released and retried by the normal package flow).
