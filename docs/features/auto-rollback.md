# Automatic rollback of a failed newdeploy rollout

**Status:** Active — not in a published image yet (2026-11-04).

## Why

On 2026-11-04 two platform deploys went out with code that could not start
(a crash at module load). Both times the function stayed on the broken
package until an operator patched `spec.package.packageref` back by hand,
and the platform's own deploy tooling could not fix it because it ran
through the broken service. The platform owner asked for every function to
go back to its previous version on its own when a new one cannot start.

## What it does

`pkg/executor/executortype/newdeploy/rollback.go`. When an update moves a
newdeploy Function to a new package, the executor starts a **rollout guard**
next to the rolling update (in the background, on the executor's lifetime
context - the reconcile is not held):

- every 5 s it looks at the Deployment and the pods of its newest
  ReplicaSet;
- all replicas updated and available -> the package is recorded on the
  Function as `fission.io/last-ready-package` (JSON PackageRef) and the
  guard ends;
- a new pod's container waits with `CrashLoopBackOff` or
  `InvalidImageName`, or restarted 3 times, or waits with an error the
  kubelet retries itself (`ErrImagePull`, `ImagePullBackOff`,
  `CreateContainerConfigError`, `CreateContainerError`,
  `RunContainerError`) for `confirmSeconds` in a row, or the rollout is still not available after the grace period
  (`max(graceSeconds, 2 x specializationTimeout)`) -> the guard merge-patches
  `spec.package.packageref` back to the last ready package, stamps
  `fission.io/rolled-back-from` / `-reason` / `-at`, and fires
  `function.deploy.rolledback` on the deploy event hook
  ([build-deploy-webhooks](build-deploy-webhooks.md)) with `package` (the
  failed one), `rollbackPackage` and `reason`.

Rollback target: the recorded last ready package; before anything was
recorded, the package the Function ran before this update - only when its
Deployment was available on it. With neither, the guard only records.

## What it never does

- **Loop.** The target is always a package that was Ready, and an update
  that does not change the package (the rollback itself, a secret / env /
  environment change) is not guarded.
- **Roll back over a newer deploy.** Each look re-reads the Function and
  stops when its packageref is no longer the guarded one.
- **React to a build failure.** `waitForBuild` holds the Deployment update
  until the package is built, so a failed build never reaches the pods - the
  old version keeps running.
- **Judge a scale-to-zero function** (no pod to look at).
- **Catch an app that starts and answers with errors.** The guard judges pod
  health (fetcher specialization keeps a pod unready until it works); a pod
  that is Ready and returns 500s is not rolled back. A federated subgraph
  that cannot compose is a separate gate.

## Verified on minikube (2026-11-04)

Executor image built from this branch, newdeploy function (minScale 1,
specializationTimeout 120 -> grace 4 min), deploy hook pointed at local
graphql-server-lambdas:

| Case | Result |
|---|---|
| good update (v1 -> v2) | recorded as last ready 5 s after the new pod was ready; nothing rolled back |
| new package that throws at load | pod stays 1/2 (fetcher keeps re-specializing, no restart); rolled back at exactly 4 min (`not available after 4m0s`); the old pod served until the rolled-back pod was ready - no downtime; the rollback did not start another guard |
| new package whose process exits at load | same as above: the `rxdi/fission-node` runtime turns a load failure into a 503 readiness, not a container restart, so the grace timer decides |
| event -> graphql-server-lambdas -> project notification | delivered to the owner and both members |

## Known limits

- **A package rewritten in place cannot be rolled back.** `fission fn
  update --code` keeps the package name and changes its content; the old
  code no longer exists, so such an update is only recorded, never rolled
  back. The platform deploys a NEW package per version (`<ns>-<fn>-<hash>`),
  which is the case this protects.
- **Load failures are caught by the grace timer**, not the crash-loop path,
  on runtimes that keep the process alive (fission-node): detection takes
  `max(graceSeconds, 2 x specializationTimeout)`.
- **The guard lives in the executor's memory.** An executor restart during
  a guarded rollout drops that guard (the old pods keep serving; nothing is
  rolled back).
- Ready-but-broken apps are not detected (see "What it never does").

## Configuration

| Helm | Env | Default |
|---|---|---|
| `executor.autoRollback.enabled` | `AUTO_ROLLBACK_ENABLED` | `true` |
| `executor.autoRollback.graceSeconds` | `AUTO_ROLLBACK_GRACE_SECONDS` | `180` |
| `executor.autoRollback.confirmSeconds` | `AUTO_ROLLBACK_CONFIRM_SECONDS` | `60` |

Why the confirm window (prod, 2026-11-04): a good graphql-server-lambdas
package was rolled back 3 s after its pod landed on a node Karpenter had just
added - the kubelet's first try said `CreateContainerConfigError: failed to
sync secret cache: timed out waiting for the condition`, which it fixes on
its own next try. One look at a retryable error is not a failure.

## Test

Unit: 
`go test -race -run 'TestRollbackTarget|TestAssessRollout|TestGuard' ./pkg/executor/executortype/newdeploy/`
(fake clientsets: crash loop -> rolled back with annotations; ready -> recorded;
moved on -> untouched; grace over -> rolled back).

## Debug

Executor log lines `guarding package rollout`, `rollout guard: ...`;
`ROLLBACK FAILED` means the patch was refused (RBAC, admission) and the
function stays on the failing package. The Function's annotations show the
last ready package and the last rollback.
