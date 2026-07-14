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

## Applying the new CRDs (required on any pre-existing cluster)

This branch inherits upstream's `FissionTenant` CRD (`crds/v1/fission.io_fissiontenants.yaml`), a **brand-new CRD type** the tenant controller depends on. CRDs are **not Helm-managed** in this repo (no `crds/` dir in the chart, no CRD templates — see `crd-status-subresource-gotcha` in project memory for the general pattern) — they're applied separately via `-k crds/v1`. A cluster that previously ran an older Fission version (this fork's `v1.22.x`/`v2.0.0`, or plain upstream pre-tenancy) will **not** have `fissiontenants.fission.io` and the install will fail.

**Symptom:** the `fission-tenant-seed` post-install/post-upgrade hook Job crash-loops and the Helm release ends in `failed`:
```
tenant seeding failed: listing existing tenants: the server could not find the requested resource (get fissiontenants.fission.io)
```

**Fix — apply the CRDs before (re-)installing:**
```bash
kubectl apply --server-side -k crds/v1
```
If the cluster's existing CRDs were originally applied via `kubectl replace` (this fork's own `make update-crds`, or an older manual `kubectl replace -k crds/v1`), server-side apply will report a **field-manager conflict on `.spec.versions`** for those pre-existing CRDs (the new `fissiontenants` CRD, having no prior manager, applies cleanly regardless). This is expected — it's just Kubernetes' server-side-apply ownership tracking noticing a different tool touched the object before. Since a CRD is only a schema/type definition (no tenant data lives on the CRD object itself), forcing ownership here is safe:
```bash
kubectl apply --server-side --force-conflicts -k crds/v1
```
This is additive/non-destructive to your actual Function/Package/Environment/etc. **objects** — it only updates the type definitions they're validated against (e.g. this branch's new `Environment.spec.builder.idleTimeout`/`poolsize` fields). Existing CRs are untouched.

**Gotcha — the tenant controller does not self-heal if it started before the CRD existed.** `fission-bundle --tenantController` checks CRD access once at startup (`Checking FissionTenant CRD access`, 30s timeout) and, on failure, logs `service exited: error waiting for CRDs: timeout waiting for FissionTenant CRD access` — but the **pod does not crash-loop or retry** (restart count stays 0; no further log lines ever appear). If you apply the CRD fix *after* the tenantcontroller pod already started and failed this check, you must manually bounce it:
```bash
kubectl delete pod -n fission -l svc=tenantcontroller
```
After that, watch it materialize `FissionTenant`s for every already-labeled namespace within a couple seconds: `kubectl logs -n fission deploy/tenantcontroller | grep materialized`, and confirm `kubectl get fissiontenants.fission.io` shows `READY: True` for all of them.

## Backfilling existing tenant namespaces

Once the CRDs are applied and the tenant controller is running, label every namespace that actually has Fission resources (rather than blanket-labeling every namespace in the cluster — a real cluster accumulates plenty of unrelated ones, e.g. `kube-system`, other operators' namespaces, leftover test namespaces with no Fission objects in them):

```bash
for res in functions environments packages httptriggers timetriggers kuberneteswatchtriggers messagequeuetriggers canaryconfigs; do
  kubectl get $res.fission.io -A --no-headers 2>/dev/null | awk '{print $1}'
done | sort -u | while read -r ns; do
  kubectl label namespace "$ns" fission.io/enabled=true --overwrite
done
```

Verify: `kubectl get fissiontenants.fission.io` should show one `Ready: True` entry per namespace found above (materialization + RBAC provisioning happens within seconds — no restart needed for namespaces labeled while the tenant controller is already running correctly, only for the "started before the CRD existed" case above).
