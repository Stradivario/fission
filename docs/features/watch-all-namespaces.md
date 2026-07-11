# watch-all-namespaces → tenancy.mode=dynamic

**Status:** superseded on `release/custom-v3.0`. The custom `FISSION_WATCH_ALL_NAMESPACES` mechanism this fork carried on `develop` (a single env var making every control-plane component watch every namespace with cluster-wide RBAC) is replaced by upstream's native multi-namespace tenancy system.

## Why

The fork's own `watch-all-namespaces` + `same_namespace_guard` design was originally proposed upstream by this fork's maintainer (issue #3298, PR #3476). Upstream instead built a more thoroughly-reviewed system: a cluster-scoped `FissionTenant` CRD, a dedicated `--tenantController` component, and three selectable postures via `tenancy.mode`:

- `static` (upstream default) — today's env-var-driven namespace list, unchanged.
- `dynamic` — namespaces onboarded/offboarded at runtime (`fission tenant enable <ns>` or a `fission.io/enabled=true` namespace label), **zero control-plane restart**. Only Fission's own CRDs are watched cluster-wide; Secrets/ConfigMaps/workloads stay per-namespace — never in a cluster-wide cache.
- `cluster` — auto-onboards every namespace with no explicit label needed, but grants the control plane cluster-wide Secret/ConfigMap read. Upstream's own docs mark this **unsafe for shared multi-tenant clusters**.

lambforge.com runs Fission as a shared control plane for mutually-untrusting tenant projects — exactly the case `cluster` mode warns against. This fork therefore defaults `tenancy.mode: dynamic` (see `charts/fission-all/values.yaml`), not `cluster`.

## What this changes operationally

Unlike the old watch-all mechanism (every namespace watched automatically, no action needed), `dynamic` mode requires **explicit onboarding per tenant namespace**:

- New tenant namespaces: the provisioning automation that creates them must set the `fission.io/enabled=true` label (or call `fission tenant enable <ns>`) at creation time. **Action needed in `graphql-server-lambdas`'s namespace-provisioning code** — it currently only relies on the blanket watch-all behavior.
- **Existing tenant namespaces**: this install's current values (`watchAllNamespaces: true`, `additionalFissionNamespaces: []`) mean every existing tenant is watched only through the blanket mechanism being removed here. Before cutover, every existing tenant namespace needs a **one-time backfill** — label it `fission.io/enabled=true` (or `fission tenant enable <ns>` for each) — or its functions silently stop being reconciled the moment `tenancy.mode=dynamic` takes effect. `tenantController.seedExistingNamespaces: true` (the chart default) only seeds `defaultNamespace` + `additionalFissionNamespaces`, which is empty here — it does NOT discover namespaces the old watch-all mechanism picked up implicitly.

## internalAuth interaction

This fork keeps `internalAuth.enabled: false` (see `charts/fission-all/values.yaml`, unchanged reasoning: unsigned KEDA HTTP connectors + the GraphQL federation gateway). `dynamic` mode's per-namespace derived-HMAC-key provisioning (`pkg/tenant/provision_auth.go`) is a documented no-op when the master secret is empty, so that pillar stays inactive — but the RBAC/cache isolation pillar (no cluster-wide Secrets/ConfigMaps read) applies regardless of internalAuth, and is already a strict improvement over the old watch-all's cluster-wide RBAC.

## Same-namespace invocation guard

`dynamic`/`cluster` mode's cross-namespace isolation story relies on admission + NetworkPolicy, which cannot see the `/fission-function/<ns>/<fn>` HTTP path — it cannot distinguish a tenant calling its own function from a tenant calling another tenant's function over the same connection to `router-internal`. This fork's `same_namespace_guard` (application-layer, podIP-attributed) is therefore kept — see the router package — rather than retired in favor of upstream's mechanism.
