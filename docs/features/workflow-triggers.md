# workflow-triggers

**Status:** Active (2026-10-26, `release/custom-v3.0`) · not yet in a
published image. The RFC-0022 phase-5 item "HTTPTrigger → WorkflowRun
adapter", plus the same for TimeTriggers. Lambforge uses it for the
trigger cards of its workflow editor.

## What it does

An HTTPTrigger or TimeTrigger can start a workflow run instead of calling a
function. The target is a new function reference type:

```yaml
functionref:
  type: workflow        # new: name is a Workflow in the same namespace
  name: order-flow
```

- **HTTP trigger:** every request to its route creates a `WorkflowRun`.
  - The JSON body is the run input; an empty body means `{}`.
  - It answers `202 {"run": "<name>", "workflow": "...", "namespace": "..."}` without waiting for the run.
  - A body that is not JSON gets 400. One over the 256 KiB run-input limit gets 413.
  - CORS config works as for function routes. `invocationMode` does not apply.
- **Time trigger:** every tick creates a `WorkflowRun` whose input is
  `spec.workflowInput` (any JSON, default `{}`). `method` / `subpath` do not
  apply.
- **Run labels:** runs carry `fission.io/trigger-kind` (`httptrigger` / `timetrigger`)
  and `fission.io/trigger-name`, so you can tell where a run came from.
  Names look like the CLI's: `<workflow>-<5 random>`.
- **Missing workflow:** it is not checked when the route is built. The run itself
  fails if its workflow never appears (the engine's grace period).
- **Other triggers:** message queue and kubewatcher triggers refuse `type: workflow` (CRD
  CEL). `FunctionReference.Validate()` (MQ, kubewatcher, async destinations,
  workflow Task states) still refuses it. HTTP and time triggers validate with
  `ValidateTriggerTarget()`.

## CLI

```bash
fission httptrigger create --name orders --url /orders --method POST --workflow order-flow
fission timetrigger create --name nightly --cron "@daily" --workflow report --workflow-input '{"scope":"all"}'
fission timetrigger update --name nightly --workflow-input @input.json
```

`--workflow` and `--function` are mutually exclusive. `timetrigger update
--function` switches a workflow trigger back to a function.

## Code

- **Types:** `FunctionReferenceTypeWorkflow` (`pkg/apis/core/v1/const.go`); the
  enum, the name CEL rule and the MQ/kubewatcher CEL refusals
  (`types.go`); `TimeTriggerSpec.WorkflowInput`;
  `FunctionReference.ValidateTriggerTarget` (`validation.go`).
- **Shared helper:** `pkg/workflowtrigger`: `Input` (JSON, size limit) and `NewRun`
  (name, labels).
- **Router:** the resolver returns `resolveResultWorkflow` (no function to
  resolve); `buildTriggerHandler` serves it with `workflowTriggerHandler`
  (`pkg/router/workflowtrigger.go`), which creates the run with the router's
  Fission clientset. Route shapes, conflicts, incremental apply and resync
  are unchanged: the route is just another trigger route.
- **Timer:** `startWorkflowRun` (`pkg/timer/timer.go`) creates the run with the
  manager client (`Start` sets `createRun`).
- **RBAC:** `router-rules` and `timer-rules` grant `workflowruns: create`
  (`charts/fission-all/templates/_fission-component-roles.tpl`).
- **Generated code:** deepcopy, the CRDs and swagger docs come from the generators.
  `hack/update-codegen.sh` could not run locally (its parser rejects Go 1.27's
  generic methods in the standard library), so the one changed apply
  configuration (`pkg/generated/applyconfiguration/core/v1/timetriggerspec.go`:
  `WorkflowInput` + `WithWorkflowInput`) was patched by hand, in the
  generator's own shape. Re-run `make codegen` where it works and expect no
  diff.

## How to test

```bash
go test -race ./pkg/workflowtrigger/ ./pkg/timer/ -run 'Test'
go test -race ./pkg/router/ -run TestWorkflowTrigger
go test -race ./pkg/apis/core/v1/ -run TestWorkflowTriggerTargetValidate
KUBEBUILDER_ASSETS=$(go tool setup-envtest -p path use 1.32.x) go test -race ./test/cel/ -run TestCELWorkflowTriggerTarget
```

Live: create a workflow, then
`fission httptrigger create --name wf --url /wf --method POST --workflow <wf>`,
`curl -XPOST $ROUTER/wf -d '{"x":1}'` returns 202 with the run name, and
`fission workflow runs get --name <run>` shows the run.

## How to debug

- **403 / "forbidden" in the router or timer log when starting a run:** the chart's
  role lacks `workflowruns: create` (an older chart with a newer image).
- **503 "workflow triggers need the router's Fission client":** the router runs
  without its clientset (informer-less test mode only).
- **The run exists but fails with "workflow did not appear":** the trigger names a
  workflow that does not exist in the trigger's namespace.
