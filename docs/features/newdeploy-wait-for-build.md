# newdeploy: wait for package build before provisioning

**Type:** Fix · **Date:** 2026-06-03 · **Area:** `executor/newdeploy`

## Motivation

A `newdeploy` function is always-warm (`minScale >= 1`), so the executor
provisions its Deployment as soon as the Function object is created —
independently of the package build, which runs asynchronously in `buildermgr`.

When a function is created (or pointed at a fresh package revision) before its
source package has finished building, the new pod's `fetcher` sidecar
specializes on startup, tries to fetch a deploy archive that does not exist yet
(`fetcher.Fetch` rejects packages whose `BuildStatus` is not
`succeeded`/`none`), exits non-zero, and the pod `CrashLoopBackOff`s (`1/2`,
runtime container up, fetcher crashing) until the build lands ~30s later and a
restart finally succeeds.

## Behavior

Before provisioning (or rolling) a newdeploy Deployment, the executor now waits
for the function's referenced package to be ready (`waitForBuild`):

- `succeeded` / `none` → provision.
- `failed` → return an error and skip provisioning (the pod would crash-loop
  forever anyway).
- `pending` / `running` / empty → poll once a second until the build settles or
  the wait window elapses, then **fall back to provisioning anyway** so an
  unusually slow build can never permanently block the function (this is the
  legacy behavior: the fetcher retries until the build lands).

The gate runs in both `fnCreate` (create / eager `AddFunc`) and
`updateFuncDeployment` (update to a new package revision).

## Configuration

`NEWDEPLOY_BUILD_WAIT_TIMEOUT` — seconds to wait for the build before falling
back to provisioning. Default **600** (`defaultBuildWaitTimeout`).

## Files

| File | Change |
|------|--------|
| `pkg/executor/executortype/newdeploy/newdeploymgr.go` | `waitForBuild`; called from `fnCreate` and `updateFuncDeployment` |

## Notes

- The self-heal path is unchanged: the fetcher gets the package **by name
  (latest)**, so it ignores the function's now-stale package `ResourceVersion`.
- `poolmgr` does not need this — it specializes on demand at request time.
- A complementary app-side guard lives in `graphql-server-lambdas`
  (`FissionSDK.waitForPackageBuild`, called before `fission fn create` and route
  creation), so the HTTP route is also not created against an unbuilt package.

## Testing

- `go vet` / `go build ./pkg/executor/...` clean;
  `go test ./pkg/executor/executortype/newdeploy/...` passes.
- Deployed to local minikube (control plane rolled onto the new `fission-bundle`
  image). Live e2e (create a `newdeploy` lambda, observe no `CrashLoopBackOff`)
  still pending.
