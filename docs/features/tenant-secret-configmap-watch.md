# tenant-secret-configmap-watch

**Status:** Active (2026-10-25, `release/custom-v3.0`) · not yet in a published image.

## The bug

Updating a Secret or ConfigMap that a function mounts (`spec.secrets` /
`spec.configmaps`) no longer recycled the function's pods in production after
the switch to `tenancy.mode=dynamic` (`v1.24.0-watch-all-namespaces`,
2026-07-11).

Under dynamic tenancy `executorCacheOptions()` (`pkg/executor/start.go`) keeps
Secret/ConfigMap namespace-scoped to the env-seeded set fixed at startup
(`FISSION_RESOURCE_NAMESPACES` + builder/function/default — in production
just `default`), so the `executor-secret` / `executor-configmap` reconcilers
never saw an update in a tenant namespace. Upstream's answer is cluster mode,
which gives the executor a cluster-wide Secret read and cache.

## The fix

`pkg/executor/tenantwatch` (wired in `pkg/executor/cms/reconciler.go`): under
dynamic tenancy a leader-only `tenantwatch.Watcher` runnable runs one **metadata-only** Secret and ConfigMap
informer per live tenant namespace (resolver set + builder/function/default,
unioned with the FissionTenants it lists itself). It re-syncs on every
FissionTenant event and every 5 minutes, starting watches on onboarding and
stopping them on offboarding. ResourceVersion-changing updates are fed to the
unchanged reconcilers through `source.Channel`; adds (the initial list) and
deletes are dropped, as `contentChangedPredicate` does. The reconcilers read the
object through the API reader in this mode.

- No Secret data in executor memory (metadata only).
- No new RBAC: the tenant controller's `fission-executor-workload` RoleBinding
  already grants secrets/configmaps get/list/watch per tenant namespace.
- No restart or chart change when a namespace is onboarded.
- Static and cluster modes are unchanged (Manager-cache path).

Also fixed in the same change: newdeploy's `getResources` shallow-copied
`env.Spec.Resources` and wrote each function's CPU/memory overrides into the
**cached Environment's** maps. Concurrent function creations on one environment
raced on those maps (the executor's `fatal error: concurrent map read and map
write` in `util.MergeContainer`, exit code 2), and one function's overrides
leaked into the next function's Deployment. It now deep-copies; container's
`getResources` likewise no longer writes into the cached Function.

## How to test

```bash
go test -race ./pkg/executor/tenantwatch/ ./pkg/executor/cms/
go test -race ./pkg/executor/executortype/newdeploy/ -run TestGetResourcesDoesNotMutateEnvironment
```

Live: `kubectl patch secret <s> -n <tenant-ns> -p '{"stringData":{"X":"'$(date +%s)'"}}'`
for a secret listed in a newdeploy function's `spec.secrets`; within seconds
the function's `newdeploy-*` Deployment rolls (its `RESOURCE_VERSION_COUNT`
env changes).

## How to debug

Executor logs `started tenant watch {"namespace": ...}` (logger `secret_configmap_tenant_watch`) for every
watched namespace at startup and on onboarding, and `stopped tenant watch` on
offboarding. A tenant missing from those lines → check its FissionTenant
exists; `forbidden` reflector errors for a namespace → the tenant's
`fission-executor-workload` RoleBinding is missing. Workaround on an image
without this fix: `kubectl rollout restart deploy/newdeploy-<fn>-... -n <ns>`.
