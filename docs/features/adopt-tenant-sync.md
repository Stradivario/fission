# adopt-tenant-sync

**Status:** Active (2026-10-25, `release/custom-v3.0`) · not yet in a published image.

## The bug

Under dynamic (or cluster) tenancy, every executor restart deleted function
Deployments in tenant namespaces and leaked poolmgr specialized pods.

At startup the leader runs the adopt pass (`AdoptExistingResources`: re-stamp
existing objects with the new executor instanceID and re-register specialized
pods into the fsCache) and then the stale-instanceID reaper
(`CleanupOldExecutorObjects`). Both snapshot the resolver's namespace set, which
the resolver-sync controller fills from the FissionTenants **concurrently**.
Adopt ran against the env seed only; the reaper, moments later, saw the tenants
and deleted every Deployment adopt had not re-stamped. The Function reconciler
then recreated them — a brief outage of every newdeploy function.

Production evidence (`fission-bundle:v1.24.1-watch-all-namespaces`): the
2026-10-24 15:36 restart reaped all 45 newdeploy Deployments; the 2026-10-25
00:55 one reaped 8 newdeploy + 19 poolmgr objects. poolmgr's
`adoptPools` built its env map before the tenants arrived while
`adoptSpecializedPods` listed pods after, so every tenant specialized pod failed
with `failed to adopt pod for function due to lack of necessary information`
(`env: ""`) — never in the fsCache, so neither function deletion nor the idle
reaper removed it (e.g. `slugify-diag`'s two pods, running for days after the
function was deleted).

## The fix

`syncTenantsBeforeAdopt` (`pkg/executor/start.go`) loads the tenant set into
the resolver with `tenant.SyncResolverFromTenants` through the API reader
before the pass, retrying for up to `adoptCleanupMaxWait` (30 s). If it cannot,
the pass is **skipped** — leaving orphans is safe, reaping against a partial
tenant set is not. Static tenancy is unchanged.

Not changed: a ReplicaSet-driven specialized-pod cleanup for tenant namespaces.
Environment v3 envs without `poolsize` have pool size 0, so their pool
ReplicaSets sit at zero replicas normally — the upstream "RS at 0 ⇒ pool
destroyed ⇒ reap its specialized pods" rule would kill live pods.

## How to test

```bash
go test -race ./pkg/executor/ -run TestSyncTenantsBeforeAdopt
```

Live: restart the executor (`kubectl rollout restart deploy/executor -n fission`);
the logs show `adopt function pod` / `adopted resources for function` for
tenant namespaces and no `cleaning up deployment` for live functions; the
`newdeploy-*` Deployments keep their creationTimestamp.

## How to debug

`could not load the tenant set; skipping adopt and orphan cleanup` → the
executor cannot list `fissiontenants` (check its ClusterRole); objects of the
previous instance are left in place until the next successful start.
