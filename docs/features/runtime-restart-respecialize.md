# runtime-restart-respecialize

**Status:** Active (2026-11-02, `release/custom-v3.0`) · not yet in a published image.

## The bug

Specialization lives in the runtime container's memory. When the kubelet
restarts that container in place (crash, OOM kill), the pod, the fetcher
sidecar and the pod's readiness all survive, but the new runtime process is
generic: no user code. The fetcher had specialized once at startup
(`-specialize-on-startup`) and never again, so the pod stayed Ready and kept
receiving traffic it could not serve, until someone deleted it.

Separately, a failed startup specialization (e.g. a federation gateway whose
subgraphs were still starting) left the pod unready forever, with a passing
liveness probe and no retry.

Production, 2026-11-02: `graphql-server` (newdeploy, the main federated
subgraph) was OOM-killed at 06:07. Its restarted container served only the
generic runtime schema (`type Query {}`). The old gateway pod kept routing
`releaseStage`/`billingCatalog` to it ("Cannot query field ... on type Query",
`DOWNSTREAM_SERVICE_ERROR`). A gateway pod the HPA added at 09:02 could not
compose ("Syntax Error: Expected Name, found }") and stayed unready.

## The fix

- **Runtime watchdog** (`pkg/fetcher/runtime_watchdog.go`): after a successful
  specialization (startup or `/specialize`), the fetcher watches the runtime.
  It probes the runtime port every second. A refused connection marks it
  suspicious, and the pod's runtime restart count is then checked every 2 s
  (`get` on its own pod, which the fetcher role already allows). A backstop
  check every minute catches a restart too fast to see. On a higher restart
  count the pod goes unready, the last specialization is replayed (the deploy
  archive and secrets are still on the shared volumes, so Fetch is skipped),
  retrying with backoff up to 30 s, and then the pod is ready again.
- **Startup retry** (`cmd/fetcher/app/server.go` `specializeWithRetry`): a
  failed startup specialization is retried (2 s doubling to 30 s) until it
  works; the pod is unready meanwhile.

No chart or RBAC change. The fetcher container must keep the name `fetcher`;
every other container counts as runtime.

Companion fixes in the runtime images (fission-node): `nodejs-graphql` answers
503 on every non-lifecycle route until specialized, and the federation gateway
polls its subgraphs and refuses a subgraph serving only the generic schema.

- **Test:** `go test -race ./pkg/fetcher/ ./cmd/fetcher/...`
  (`TestRuntimeWatchdog*`, `TestSpecializeWithRetry*`, synctest).
- **Debug:** fetcher log `runtime container restarted - re-specializing` /
  `runtime re-specialized after restart`; `runtime watchdog disabled` = the
  pod could not be read (RBAC).
