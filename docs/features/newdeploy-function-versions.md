# newdeploy-function-versions

**Status:** Active (2026-10-25, `rebase/upstream-2026-10-25`, commit 0deff7f6) ·
not yet in a published image. Lambforge uses it for lambda versions/aliases
(`graphql-server-lambdas/docs/features/lambda-versioning.md`).

## The bug

RFC-0025 FunctionVersion/FunctionAlias worked for poolmgr functions only. A
newdeploy (or container) function updates its live Deployment in place, but the
executor's fscache entry stayed keyed under the function's OLD `(UID, Generation)`.
A version snapshots that Generation, so a request for the version (through an alias or
`functionref.version`) found the live entry and was served the **live, updated**
code. The version's own `-v<seq>` Deployment was never created, so for these
executors aliases did nothing, silently.

## The fix

- `pkg/executor/fscache/functionServiceCache.go`:
  - `Rekey(fsvc, fn)` moves an entry to the function's current key after an in-place update.
  - `GetByFunctionVersion(meta)` returns an entry only when its `fission.io/function-version` label matches the request. Otherwise it returns NotFound, so the executor creates the version's own Deployment.
- `newdeploy/newdeploymgr.go`, `container/containermgr.go`: `GetFuncSvcFromCache` uses `GetByFunctionVersion`, and `updateDeployment` re-keys the live entry when it succeeds.

## How to test

```bash
go test -race ./pkg/executor/fscache/ -run "TestRekeyMovesLiveEntryToNewGeneration|TestGetByFunctionVersionRequiresSameVersion"
```

Live (minikube, the scratchpad `spike/newdeploy-versions.sh`): a newdeploy
function with `fission fn versioning --mode auto`, two updates. Results:

- `v1` and `v2` each get their own `-v<seq>` Deployment.
- An alias on `v1` is pinned for 20/20 requests while live is `v2`.
- Repointing took ~3.6 s, and a rollback 55 ms.
- A 50/50 split served 31/29.

## How to debug

An alias returns the live code for a newdeploy function → no
`newdeploy-<fn>-…-v<seq>` Deployment exists. The executor is older than this fix,
or the version was pruned by `retain`. Check the FunctionAlias `Resolved` condition.
