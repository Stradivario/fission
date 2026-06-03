# Scale-to-Zero for Builders

**Type:** Feature · **Date:** 2026-05-30 · **Area:** `buildermgr`

## Motivation

Environments with a builder image provision a builder Deployment (1 replica) that
runs indefinitely. In a multi-tenant cluster this can mean 120+ idle builders
holding CPU/memory for months with no build activity. This feature scales an idle
builder down to 0 replicas and brings it back to 1 on demand when a build is
triggered.

## Behavior

- **Scale down:** a reaper goroutine runs on an interval. For each builder it
  scales the Deployment to 0 once it has been idle (no builds) for longer than the
  configured idle timeout. Builders with a build in progress are skipped.
- **Scale up:** when a package build is triggered, the builder is scaled back to 1
  (if not already), the existing readiness/back-off loop waits for the builder pod
  to become ready, and only then is the build dispatched. A `buildInProgress` flag
  prevents the reaper from scaling the builder down mid-build.
- **First build after idle** pays a cold start (pod schedule + image pull). The
  existing health-check back-off in the package watcher covers this wait, so builds
  still succeed — they are just slower right after the builder was idled.

## Configuration

### Per-environment idle timeout (CRD field)

`spec.builder.idleTimeout` — integer seconds (`*int64`).

| Value | Meaning |
|-------|---------|
| unset (`nil`) | Use the default, **600s** (10 min) |
| `0` | **Never** scale to zero (disable for this environment) |
| `N > 0` | Scale to zero after `N` seconds idle |

CLI flag (on `fission env create` and `fission env update`):

```bash
fission env create --name nodejs --builder fission/node-builder --builder-idletimeout 300
fission env update --name nodejs --builder-idletimeout 0   # disable scale-to-zero
```

Default constant: `DefaultBuilderIdleTimeout = 600` (`pkg/buildermgr/envwatcher.go`).

### Reaper interval (env var)

How often the reaper checks for idle builders. Resolved by
`GetBuilderIdleReaperInterval` (`pkg/executor/util/util.go`):

1. `BUILDER_IDLE_REAPER_INTERVAL` (seconds)
2. falls back to `OBJECT_REAPER_INTERVAL` (seconds)
3. falls back to default **10s**

## Files

| File | Change |
|------|--------|
| `pkg/apis/core/v1/types.go` | Added `Builder.IdleTimeout *int64` |
| `pkg/apis/core/v1/zz_generated.deepcopy.go` | Regenerated deepcopy for the pointer field |
| `pkg/apis/core/v1/zz_generated.swagger_doc_generated.go` | Regenerated swagger doc |
| `crds/v1/fission.io_environments.yaml` | Regenerated CRD schema (`spec.builder.idleTimeout`) |
| `pkg/buildermgr/envwatcher.go` | `builderInfo` fields, reaper (`idleBuilderReaper`/`doIdleBuilderReaper`), `ScaleBuilderDeployment`, cache locking, last-build-time/build-in-progress accessors |
| `pkg/buildermgr/pkgwatcher.go` | `ensureBuilderReady` (scale 0→1), `SetBuildInProgress`, `UpdateLastBuildTime` on build |
| `pkg/buildermgr/buildermgr.go` | Wire reaper interval into `makeEnvironmentWatcher` |
| `pkg/executor/util/util.go` | `GetBuilderIdleReaperInterval` |
| `pkg/fission-cli/flag/key/key.go`, `flag/flag.go`, `cmd/environment/{command,create,update}.go` | `--builder-idletimeout` CLI flag |
| `pkg/buildermgr/scaletozero_test.go` | Unit tests |
| `charts/fission-all/templates/_fission-kubernetes-roles.tpl` | buildermgr ClusterRole: `deployments/scale` + `deployments` `get`/`update` — **required**, else `GetScale`/`UpdateScale` are RBAC-forbidden and builds fail |

> Regeneration: after changing `pkg/apis/core/v1/types.go`, run
> `make generate-crds` and `make generate-swagger-doc`.

## Correctness fixes applied during review

The initial implementation had four bugs that were fixed:

1. **`lastBuildTime` was never initialized**, so every freshly created builder was
   scaled to 0 on the first reaper tick regardless of the timeout. Now set to
   `time.Now()` at creation.
2. **Data race on the builder cache map** — the reaper iterated the map while
   event handlers / build goroutines mutated it without a shared lock. All cache
   access now goes through a single `RWMutex` (`cacheMu`) via helper methods.
3. **Reaper held the lock across network I/O and did a live API `Get` per builder
   per tick.** It now snapshots the cache under the lock and scales outside it, and
   caches `idleTimeout` in `builderInfo` instead of fetching the Environment each
   tick.
4. **Missing RBAC for the scale subresource.** The chart's buildermgr ClusterRole
   granted only `deployments [list, create, delete]`, so `GetScale`/`UpdateScale`
   were forbidden: the reaper could not scale builders down and — worse —
   `ensureBuilderReady` could not scale a 0-replica builder back to 1, so **every
   build whose builder was idle failed** at "ensuring builder ready"
   (`cannot get resource deployments/scale`). The package was marked `failed`, the
   function never specialized, and the router returned "error sending request to
   function" for both poolmgr and newdeploy. Fixed by adding `deployments/scale`
   (`get`/`update`) to `buildermgr-kuberules`.

## Testing

- Unit tests (`pkg/buildermgr/scaletozero_test.go`), run with the race detector:
  - reaper scales an idle builder to 0;
  - skips a builder with a build in progress;
  - does not reap a freshly created builder;
  - never scales a builder with `idleTimeout=0`;
  - `ensureBuilderReady` scales 0→1 and is a no-op at 1.

```bash
go test -race ./pkg/buildermgr/...
```

- **Not yet done:** live-cluster end-to-end (create env with
  `--builder-idletimeout`, observe scale to 0, trigger a build, observe scale to 1,
  observe scale back to 0). Confirm `kubectl get environment <name> -o yaml`
  retains `spec.builder.idleTimeout`.
