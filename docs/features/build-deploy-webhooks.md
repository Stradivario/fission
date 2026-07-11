# Build & deploy lifecycle events

Push package-build and function-deploy lifecycle events to a designated **Fission
function**, so consumers stop polling `fission package info` / `kubectl rollout
status` to find out when a build finished or a new pod actually specialized.

Deliberately *not* called "webhooks" as the primary mechanism, even though that's
how the idea started — see [Delivery target](#delivery-target-invoke-a-fission-function-recommended)
for why invoking a function through the router beats POSTing to an arbitrary URL,
and [Naming](#naming) for why the word is avoided in code/config (it already means
something else in this codebase).

## Motivation

`graphql-server-lambdas` (a consumer of this fork) needs to know two things after a
lambda deploy: "did the package build?" and "did the new pod actually come up and
replace the old one?" Today it answers both by polling — `FissionSDK.waitForPackageBuild`
(`fission package info` in a loop) and `KubectlNamespaceService.waitForLambdaRollout`
(`kubectl rollout status`, with a hand-rolled `.metadata.generation` check to avoid a
false-positive against a stale rollout — see
`graphql-server-lambdas/docs/federation-gateway-restart-plan.md`). That works, but:

- every consumer that cares about this reinvents the same poll loop;
- polling has an inherent latency floor (poll interval) and burns API-server
  requests proportional to (consumers × deploys);
- there's no way to know a build/deploy event happened at all without asking.

A push model removes the poll loop for the common case while keeping polling
available as a fallback (see [Relationship to existing features](#relationship-to-existing-features)).

## Design goals

- **Best-effort, fire-and-forget.** A slow or unreachable dispatch target must
  never stall the buildermgr or executor reconciler — no blocking the build/deploy
  of *other* functions while waiting on a dead HTTP endpoint. Same philosophy as
  `waitForBuild`'s "provision anyway" fallback in
  [newdeploy-wait-for-build.md](newdeploy-wait-for-build.md): an external dependency
  gets a bounded budget, never an unbounded stall.
- **Not a replacement for reconciliation.** Consumers keep a slower periodic
  poll/reconcile as a safety net for dropped deliveries — the same watch+resync
  pattern Kubernetes controllers use internally. This feature only removes the
  *common-case* latency and load of polling, not the need for eventual-consistency
  fallback.
- **Opt-in, zero default behavior change.** Unset env var = feature disabled,
  matching this fork's existing convention (e.g. `internalAuth.enabled` defaulting
  off — [internal-auth.md](internal-auth.md)).
- **Small security surface.** One configured target per event family (buildermgr,
  executor), not a per-tenant registry. This fork backs one platform
  (graphql-server-lambdas), not a general-purpose multi-tenant event broker — a
  registry/CRD-based per-tenant subscription model would be a much larger scope
  increase for no near-term benefit (see [Open questions](#open-questions) for how
  that could layer on top later without reworking this).

## Delivery target: invoke a Fission function (recommended)

The natural target isn't an arbitrary external URL — it's **a Fission function,
identified by namespace + name**, invoked through the router's existing internal
per-function route:

```
http://router.fission.svc.cluster.local/fission-function/<namespace>/<function>
```

This is the exact mechanism `FissionServiceDiscovery.discoverServiceUrl`
(graphql-server-lambdas) already uses to reach subgraph functions — reusing it here
means the event dispatcher is not a new kind of client, just another Fission
invocation, so it inherits router-level routing/scale-from-zero behavior for free.

It turned out this fork already had the exact delivery mechanism needed:
`pkg/publisher.WebhookPublisher` (the same async, retrying dispatcher
`timer`/`mqtrigger`/`kubewatcher` already use to invoke
`/fission-function/<ns>/<name>` on the router, HMAC-signing automatically when
`FISSION_INTERNAL_AUTH_SECRET` is set). `pkg/eventhook` is a thin layer on top —
env-var config plus payload shaping — not a new HTTP client, retry loop, or
signing path. See [How delivery works](#how-delivery-works).

Why this beats a raw webhook URL:

- **No new dependency, no new delivery mechanism.** graphql-server-lambdas is
  *itself* deployed as a Fission function (with elevated in-cluster privileges — it
  runs the platform's own control plane, unlike tenant namespaces). Pointing the
  dispatcher at `<its-namespace>/<its-function>` reaches it as a completely normal
  incoming HTTP request on a route it already serves — no new ingress, no new port,
  no new attack surface beyond "one more route on a function it already runs."
- **Can't be misconfigured to leave the cluster.** A `namespace` + `function` pair
  resolves internally by construction; there's no way to accidentally (or
  maliciously, via a compromised values.yaml) point cluster-internal build/deploy
  telemetry at an external host.
- **Matches this fork's own architecture.** HTTPTrigger, MessageQueueTrigger,
  TimeTrigger — every existing trigger type fires a Function. A lifecycle event
  target is the same shape: "invoke this Function when X happens."

A raw `url:` escape hatch (see [Configuration](#configuration)) stays available for
a non-Fission receiver outside the cluster, but the function-target is the
recommended and primary mode.

### Considered alternative: RabbitMQ

Publish events to a queue instead (this cluster already runs RabbitMQ for
tenant-facing `AMQPSubscribe` messaging). Rejected for this use case:

- It would couple a **control-plane** signal (gateway-restart triggers, deploy
  telemetry) to the availability and blast radius of a broker whose current job is
  **tenant-facing** messaging — an unrelated concern with different failure modes
  and different people who'd notice it breaking.
- A broker adds real operational weight (HA, queue/exchange provisioning, consumer
  ack/redelivery semantics) for a signal that's low-volume, latency-tolerant (the
  receiver reconciles anyway), and already has a zero-new-dependency alternative
  (above).
- The function-target design gets "queue-like" delivery for free where it matters:
  the router already handles routing and scale-from-zero for the target function,
  and a dispatch failure just means the receiver's own fallback poll catches it
  next cycle — no broker needed to get that property.

## Events

### Package build events (buildermgr)

Fired from `pkg/buildermgr/package_reconciler.go`, at the same point the existing
reconcile loop already detects a `pkg.Status.BuildStatus` transition and writes it
back to the Package CR. Hooking in right there (not a separate watcher) guarantees
the dispatcher can't observe a transition the reconciler itself coalesced away.

Events: `package.build.succeeded`, `package.build.failed`. (`package.build.started`
is a plausible future addition for live UI build-status — lower priority, not
needed by the graphql-server-lambdas use case.)

```json
{
  "event": "package.build.succeeded",
  "namespace": "<projectId>",
  "package": "<packageName>",
  "buildStatus": "succeeded",
  "timestamp": "2026-07-11T12:00:00Z"
}
```

### Function deploy events (both executor types)

Both `newdeploy` and `poolmgr` fire the same two events —
`function.deploy.ready` / `function.deploy.failed` — with the same JSON shape,
but at genuinely different moments, because the two executors have
fundamentally different execution models. This is not a partial
implementation; it's the honest equivalent signal for each.

**newdeploy** (`pkg/executor/executortype/newdeploy/newdeploymgr.go`) has a
real per-function Deployment to roll out and wait on. `fnCreate` already calls
`util.WaitForDeployment` (blocks until `AvailableReplicas` reaches the
Deployment's own target replica count) via `createOrGetDeployment` — the event
fires right after `SetFunctionReady`.

`updateFuncDeployment` (the path used on every redeploy of an *existing*
function) historically did **not** wait for availability; it patched the
Deployment and returned, leaving the rolling update to finish in the
background (this is the exact gap graphql-server-lambdas' `previousGeneration`
snapshot works around today). This is now closed: when
`deployEventHook.Enabled()` (i.e. `eventHooks.functionDeploy` is configured),
`updateFuncDeployment` calls the same `executorUtils.WaitForDeployment`
`fnCreate` already uses before firing the event. **A timeout there returns
`nil`, not the wait error** — this wait is an observation, not a mutation
(`updateDeployment` above already succeeded), so there's nothing for a
reconcile requeue to usefully retry; returning the error instead was tried
first and caused a real incident (an unbounded stream of
`function.deploy.failed` events every few seconds against a slow rollout —
controller-runtime requeues a failed `Reconcile` with exponential backoff,
re-running the whole wait AND re-firing the event every attempt). Two other
bugs surfaced by live minikube testing before this shipped, both now fixed:
`WaitForDeployment` was waiting for `MinScale` (floored to 1) instead of the
Deployment's actual target replica count (`newDeployment.Spec.Replicas`),
falsely firing `.failed` for scale-to-zero functions at 0 replicas; and
`getDeploymentSpec` never set `.ObjectMeta.Namespace` on the Deployment object
it constructs (`newdeploy.go`) — harmless for every *other* caller (namespace
comes from the `.Deployments(ns)` client builder, or from the API server's
returned object), but fatal for `WaitForDeployment`, which reads
`.ObjectMeta.Namespace` directly off the raw object, producing a malformed
`.Deployments("")` request that 404s instantly regardless of `MinScale`. This
wait is entirely gated on `Enabled()`: an install that hasn't configured
`eventHooks.functionDeploy` gets byte-for-byte the same behavior as before
this feature existed.

**poolmgr** (`pkg/executor/executortype/poolmgr/`) has no per-function
Deployment at all — pods are lazily specialized on demand from a shared
per-environment pool (`gp.go` `specializePod`, called from `getFuncSvc`), and
a pod gets re-specialized constantly during normal operation (idle recycle,
health-check failure) for reasons having nothing to do with a deploy. So
there is no synchronous "rollout" to wait for, and firing on every specialize
would be noise, not signal. Instead: `GenericPoolManager.ReconcileFunction`
marks the function's UID pending (`deploypending.go`) on **both** create and
update (mirroring newdeploy's fnCreate + updateFuncDeployment coverage); the
specialize choke point in `getFuncSvc` consumes (checks-and-clears) that mark
on its very next outcome — success or failure — and fires the event only if
it was actually pending. A routine respecialize with no pending mark fires
nothing, exactly as before this feature existed. The practical consequence:
for poolmgr, `function.deploy.ready`/`.failed` fires whenever the function is
*next invoked* after the update — immediately if it's already receiving
traffic, much later (or never, before the next redeploy) if it's idle. That
asymmetry with newdeploy's near-immediate signal is inherent to the two
executors' designs, not a gap in this feature.

```json
{
  "event": "function.deploy.ready",
  "namespace": "<projectId>",
  "function": "<functionName>",
  "deploymentGeneration": "7",
  "timestamp": "2026-07-11T12:00:05Z"
}
```

`deploymentGeneration` is newdeploy-only (`omitempty` — poolmgr has no
Deployment, so no generation to report) and directly answers "is this MY
update or a stale one" — consumers no longer need graphql-server-lambdas' own
before/after generation
snapshot dance to avoid a false positive.

## How delivery works

`pkg/eventhook.Dispatcher` (one instance per event family, constructed once at
startup and reused — `PackageReconciler.eventHook` in buildermgr,
`NewDeploy.deployEventHook` in the executor) does three things and nothing more:

1. Reads its target from `EVENTHOOK_*` env vars once, at construction.
   Unconfigured → every `Send` is a no-op (nil-publisher check, no allocation, no
   network call) — this is what makes the feature genuinely zero-cost when
   disabled, including on the gated `updateFuncDeployment` wait (see
   [Events](#events)).
2. Marshals the JSON `Event` payload and resolves the target path:
   `utils.UrlForFunction(function, namespace)` (`/fission-function/<ns>/<name>`,
   the same helper `timer.go` uses) for the recommended mode, or `/` for the raw
   `url:` escape hatch.
3. Hands both to a `publisher.WebhookPublisher` (`pkg/publisher/webhookPublisher.go`)
   constructed against the router's internal URL (`ROUTER_INTERNAL_URL`, same
   resolution and same env var `cmd/fission-bundle/main.go` already uses for
   timer/kubewatcher/mqtrigger — now also set on buildermgr and executor, but
   inert unless `eventHooks` is enabled).

Everything past that — async queued delivery, retry with backoff, and automatic
HMAC signing via `hmacauth.ServiceSigner` whenever `FISSION_INTERNAL_AUTH_SECRET`
is set — is `WebhookPublisher`'s existing, already-in-production behavior.
`pkg/eventhook` doesn't reimplement any of it; there is no separate timeout,
retry count, or signing path to configure or keep in sync with the rest of the
fork's internal-publisher machinery.

On auth specifically: signing follows whatever `internalAuth.enabled` already is
on the cluster — **not** a new mandate specific to this feature. If it's on,
every `eventHooks` dispatch is signed for free, same as every other internal
call to the router's listener. If it's off (this fork's current default — see
[internal-auth.md](internal-auth.md)), `eventHooks` dispatches unsigned too,
consistent with the rest of the system's current posture rather than
inventing a stricter special case. The forgery risk this leaves (any pod that
can already reach `router.fission.svc.cluster.local` — which, per this fork's
default NetworkPolicy, is every namespace — can attempt a spoofed event against
a known namespace+function target) is real and worth turning `internalAuth.enabled`
on if a receiver starts taking non-idempotent action (e.g. fanning events out to
user-facing notifications) based on these events. It is a cluster-wide toggle,
not something `eventHooks` can opt into unilaterally.

## Configuration

Mirrors the `internalAuth.envs` pattern: a Helm-templated env block
(`eventHooks.envs`, next to `internalAuth.envs` in `_helpers.tpl`), included into
both the buildermgr and executor Deployments, off by default. No separate
timeout/retry config — see [How delivery works](#how-delivery-works) for why
`WebhookPublisher`'s own values are reused as-is instead.

```yaml
# values.yaml
eventHooks:
  enabled: false
  packageBuild:
    # Preferred: invoke a Fission function through the router (see Delivery target).
    namespace: ""
    function: ""
    # subpath matters when the target is a full app with its own router
    # rather than a single-purpose function — see graphql-server-lambdas
    # example below.
    subpath: ""
    # Escape hatch: an arbitrary URL for a non-Fission / out-of-cluster receiver.
    # Mutually exclusive with namespace/function (namespace/function wins if both are set).
    url: ""
  functionDeploy:
    namespace: ""
    function: ""
    subpath: ""
    url: ""
```

| Setting | Default | Meaning |
|---|---|---|
| `eventHooks.enabled` | `false` | Master switch; matches `internalAuth.enabled`'s off-by-default convention |
| `eventHooks.packageBuild.{namespace,function}` | unset | Target for `package.build.*` events — recommended mode |
| `eventHooks.packageBuild.subpath` | `""` (root) | Path appended after `/fission-function/<ns>/<fn>` — the router only strips that prefix and forwards the rest, so this is what lets the event land on a specific route on a multi-route target function |
| `eventHooks.packageBuild.url` | unset | Target for `package.build.*` events — raw-URL escape hatch |
| `eventHooks.functionDeploy.{namespace,function,subpath}` / `.url` | unset / `""` | Same, for `function.deploy.*` events |

Rendered into env vars (`EVENTHOOK_ENABLED`, `EVENTHOOK_PACKAGE_BUILD_NAMESPACE`,
`EVENTHOOK_PACKAGE_BUILD_FUNCTION`, `EVENTHOOK_PACKAGE_BUILD_SUBPATH`,
`EVENTHOOK_PACKAGE_BUILD_URL`, and the `FUNCTION_DEPLOY` equivalents) by
`eventHooks.envs` in `_helpers.tpl`, settable at `helm install` or `helm upgrade`
like any other chart value — no separate config channel.

### Worked example: graphql-server-lambdas

`graphql-server-lambdas` receives these events on a normal route on its own
Fission function (it's a full Hapi app running as a Fission function, not a
single-purpose one) — `POST /webhooks/fission-events`, registered by
`FissionEventsPlugin` (`src/app/webhooks/fission-events.plugin.ts` there). It
validates the payload shape, logs, and republishes to the internal
`fission-lifecycle-event` pubsub channel (`FissionEventsService`) as the
extension point for further fan-out — it does not verify a signature itself
(see [How delivery works](#how-delivery-works) on why: the router's own gate
already covers this when `internalAuth.enabled` is on). Configuration:

```yaml
eventHooks:
  enabled: true
  functionDeploy:
    namespace: "<graphql-server-lambdas-project-namespace>"
    function: "<graphql-server-lambdas-function-name>"
    subpath: "/webhooks/fission-events"
```

## Relationship to existing features

- Complements [newdeploy-wait-for-build.md](newdeploy-wait-for-build.md): that
  feature prevents a bad outcome (crash-loop on an unbuilt package); this one
  notifies external systems of a *good* outcome (build/deploy actually landed),
  so they stop polling for it.
- Does not replace polling as a fallback — see Design goals above.

## Naming

The word "webhook" already means something else in this codebase: `values.yaml:494`
has a top-level `webhook:` key and `_helpers.tpl` defines `fission-webhook.svc` —
both for the K8s **admission** webhook (`pkg/webhook/function.go`, validates
Function objects on write). To avoid colliding with that, this feature uses
distinct names throughout: Helm key `eventHooks` (not `webhooks`), Go package
`pkg/eventhook/` (not `pkg/webhook/`), env var prefix `EVENTHOOK_`.

## Key files

- `pkg/eventhook/eventhook.go` — `Dispatcher`, `Event`, `NewPackageBuildDispatcher`,
  `NewFunctionDeployDispatcher`. Config-from-env plus payload shaping only; delivery
  is `pkg/publisher.WebhookPublisher` (see [How delivery works](#how-delivery-works)).
- `pkg/buildermgr/package_reconciler.go` — `PackageReconciler.eventHook` field;
  dispatch on the confirmed `BuildStatusSucceeded` write (end of `build()`) and
  inside `markBuildFailed` (the shared failure choke point, dispatched only after
  the status write itself succeeds).
- `pkg/executor/executortype/newdeploy/newdeploymgr.go` — `NewDeploy.deployEventHook`
  field; dispatch at the end of `fnCreate` (after `SetFunctionReady`) and inside
  `updateFuncDeployment` (gated on `deployEventHook.Enabled()` — see
  [Events](#events)).
- `pkg/executor/executortype/newdeploy/newdeploy.go` — `getDeploymentSpec`'s
  `Deployment.ObjectMeta.Namespace`, fixed as part of this feature (see Events
  above for why it mattered here specifically).
- `pkg/executor/executortype/poolmgr/deploypending.go` — `deployPendingSet`,
  the mark/consume tracking that makes poolmgr's signal precise instead of
  firing on every routine respecialize.
- `pkg/executor/executortype/poolmgr/reconciler.go` — `GenericPoolManager.ReconcileFunction`
  marks pending (both create and update) before delegating to
  `reconcilePoolmgrFunc`.
- `pkg/executor/executortype/poolmgr/gp.go` — `GenericPool.deployEventHook` /
  `.deployPending` fields (shared by pointer with the owning
  `GenericPoolManager`, same pattern as `fsCache`); consume + dispatch at the
  `specializePod` choke point inside `getFuncSvc`.
- `pkg/executor/executortype/poolmgr/gpm.go` — `GenericPoolManager.deployEventHook`
  / `.deployPending` construction; threaded into `MakeGenericPool`.
- `charts/fission-all/templates/_helpers.tpl` — `eventHooks.envs` define, next to
  `internalAuth.envs`.
- `charts/fission-all/templates/buildermgr/deployment.yaml` and
  `charts/fission-all/templates/executor/deployment.yaml` — `ROUTER_INTERNAL_URL`
  (same value timer/kubewatcher/mqt already set, previously absent on these two
  since neither needed it before) and `{{- include "eventHooks.envs" . | indent 8 }}`.
- `charts/fission-all/values.yaml` — `eventHooks` block, next to `internalAuth`.
- `graphql-server-lambdas/src/app/webhooks/fission-events.plugin.ts` +
  `fission-events.service.ts` (separate repo) — the receiving end; see
  [Worked example](#worked-example-graphql-server-lambdas).

## Open questions

- **Per-tenant self-service, as a later layer.** This design covers the
  platform-level need (one global, admin-configured target — graphql-server-lambdas'
  own control-plane function). A *different*, larger feature would let a tenant
  register their own function to run on their own function's build/deploy events
  ("notify me on Slack when my lambda deploys," "run a smoke test after deploy") —
  that needs a namespaced CRD (own reconciler, own RBAC scoping so a tenant can only
  target their own namespace) and is out of scope here, but this design doesn't
  preclude layering it on top later: same event sources, same `pkg/eventhook`
  payload shapes, just a per-namespace target list instead of one global one.
- **In-memory retry state only** (inherited from `WebhookPublisher`, not introduced
  here). Lost on buildermgr/executor restart. Acceptable given consumers reconcile
  independently; flagged here so it's a known property, not a surprise.
