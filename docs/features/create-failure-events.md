# Create-failure Events on the Function

**Status:** Active — not in a published image yet.

## Problem

When the executor cannot create a newdeploy function's Kubernetes objects,
the error only reached the executor's own log. The caller sees
`specialization_failed` and nothing in the cluster says why. Seen on a
Lambforge free sandbox (2026-11-14): the namespace quota refused the
function's Service (`exceeded quota ... services=3/3`), the function never
started, and nobody was told.

## Change

`pkg/executor/util/create_failure_event.go` — `RecordCreateFailure` writes a
Warning Event on the **Function**:

| Field | Value |
|---|---|
| reason | `FailedCreate` |
| source / reportingController | `fission-executor` / `fission.io/executor` |
| involvedObject | `fission.io/v1` `Function` (name, namespace, uid) |
| message | `could not create the function's <service\|deployment>: <API error>` (cut to 1024 chars) |

One Event per function and object kind (stable name from uid + kind):
the executor retries on every request, and a retry raises the Event's
count and last time instead of creating a new one. It never fails the
caller — a missing permission or conflict is only logged at V(1). The
executor's role already allows `create/get/patch` on events.

Called from `newdeploy` `createFunction` when `createOrGetSvc` or
`createOrGetDeployment` fails.

## Why it matters downstream

The Event shows in `kubectl describe function`, and Lambforge's event
watcher (graphql-server-lambdas `QuotaEventWatcherPlugin`, feature
`quota-capacity`) turns "exceeded quota" Events into a project
notification naming the lambda.

## Test

```bash
go test -race -run 'TestRecordCreateFailure|TestCreateFailureEventName' ./pkg/executor/util/
go test -race ./pkg/executor/executortype/newdeploy/
```

Verified live on minikube (2026-11-14) with a locally built executor: a
namespace with `ResourceQuota services: "0"` and a newdeploy function ->
the router answers `specialization_failed`, and
`kubectl get events --field-selector reason=FailedCreate` shows the
Function's Event from `fission-executor` with the quota message, its count
rising with each retry.
