# Fork vs. Upstream Comparison — Findings & Options

**Date:** 2026-07-11
**Scope:** read-only comparison of `develop` (this fork, Stradivario/fission) against `upstream/main` (fission/fission). No code changed. This document is the record of what I found; no merge/rebase has been attempted.

---

## TL;DR

- A blind `git merge upstream/main` is **not** a low-effort operation. Upstream has done **69 commits / 685 files / +59,771 −11,267 lines** of work since the fork's last sync point, and a large fraction of it is architectural rewrites of the exact subsystems this fork patches (router internals, buildermgr, HMAC, namespace handling).
- The "overlapping multitenancy" you noticed is real, and it has a specific backstory: **upstream's new multi-tenancy design is a direct response to a PR you opened** (`#3476`, tracking issue `#3298`). They didn't merge your PR — they used it as the motivating/contrast case in their own PRD and built a different, more elaborate mechanism instead. See [§3](#3-the-multitenancy-overlap-is-not-a-coincidence).
- My recommendation is **not** "merge everything" or "ignore upstream forever." It's a **third option**: retire the fork's own multitenancy code in favor of upstream's native `tenancy.mode: cluster`, and re-port only the two features that are genuinely still fork-unique (builder concurrency pool, build/deploy webhooks) onto current upstream. This is real work, but far less than reconciling two independent multitenancy implementations line-by-line. See [§6](#6-recommendation).
- Nothing here is urgent-security-critical: the two named CVEs fixed upstream since your fork's baseline (GHSA-qf5v, GHSA-vchh) are **already in `develop`**. The security exposure of staying as-is is "missing 4+ months of hardening and dependency bumps," not "known unpatched CVE."

---

## 1. Repo topology

```
origin   = git@github.com:Stradivario/fission.git   (this fork)
upstream = git@github.com:fission/fission.git        (canonical)
```

Local branches worth knowing about:
- `develop` (current branch, HEAD `bed3a7e8`) — active fork work, this is what you called "our changes."
- `main` / `master` (local + origin) — kept in sync with upstream for now, per your description.
- `release/custom-v1.22.1` — the **old, still-released** fork line (v1.22 base). Per earlier work in this repo, this branch must never be touched.
- `release/custom-v2.0` — a from-scratch "re-port fork features onto upstream's controller-runtime architecture" branch, done in an earlier session. **Finding: `develop` has since absorbed everything that branch did, plus more** (see §2). `release/custom-v2.0` is now redundant history, not a divergent line you need to reconcile separately — but see the caveat in §6.

Merge-base math (`git merge-base develop upstream/main`):

```
                ae970aaa  (merge-base — last point develop and upstream share)
               /        \
   develop (26 commits)   upstream/main (69 commits)
   HEAD = bed3a7e8         HEAD = 1079529c
```

- `git rev-list --count upstream/main..develop` → **26** (fork-only)
- `git rev-list --count develop..upstream/main` → **69** (upstream-only)
- `git diff --shortstat ae970aaa upstream/main` → **685 files, +59771/−11267**
- `git diff --shortstat ae970aaa develop` → **68 files, +4012/−115**

Both of the security fixes named in `CLAUDE.md`'s "things that bite" section and in memory are already inherited: `2569b42b` (GHSA-qf5v, podspec cap allowlist) and `0deed6bf` (GHSA-vchh, HTTPTrigger path validation) are both ancestors of `develop`'s fork point. You're not missing either.

---

## 2. What's fork-only (the 26 commits on `develop` since `ae970aaa`)

Four real features, plus release plumbing:

1. **Builder concurrency + scale-to-zero pool** (`39cfe37d`, `fc9d73db`, `4dc61eb0`, `0d9e3ad8`, `a636a3b6`, `d8643c79`) — `Builder.IdleTimeout`/`PoolSize` on the Environment spec, a semaphore-bound builder pool with an idle reaper, newdeploy waits for package build before provisioning. Files: `pkg/buildermgr/builderpool.go`, `idle_reaper.go`, `scale.go`, changes to `environment_reconciler.go`/`package_reconciler.go`, `pkg/executor/executortype/newdeploy/*`.
2. **Watch-all-namespaces** (`83891e18`) — the fork's namesake feature. Single control plane watches every namespace instead of a fixed list.
3. **Same-namespace invocation guard + HMAC posture** (`20699f1e`, `9774107a`, `0c10d202`, `334f312d`, `5946caed`, `e26a051f`) — `pkg/router/same_namespace_guard.go` (257 lines, self-contained) attributes the caller's namespace by pod IP and 403s cross-namespace `/fission-function/` calls on the internal listener; `internalAuth.enabled` defaults off in the chart; a self-heal path keeps the internal-auth Secret in sync across dynamic namespaces.
4. **Build/deploy event webhooks** (`b0d8cf5e`) — new `pkg/eventhook` package (203 lines) that POSTs a webhook when a package build finishes; hooks into `package_reconciler.go`, poolmgr, and newdeploy.

Plus: Stradivario release identity (`ghcr.io/stradivario` images, own goreleaser/chart-publish process) and several chart-version bump commits.

**Total footprint is genuinely small** — 68 files, ~4,000 lines. That's the good news: your actual custom logic is compact and well-isolated (mostly new files, not scattered edits), which is why a re-port onto a new upstream base is tractable at all.

---

## 3. The multitenancy overlap is not a coincidence

I fetched the actual GitHub PR and issue to confirm this rather than guess from commit messages:

- **Issue `#3298`** ("Everything in the cluster gets recreated when adding `additionalFissionNamespace`...") was opened by **Stradivario** (you). It describes exactly the problem `watch-all-namespaces` was built to sidestep: `additionalFissionNamespaces` is baked into env vars read once at process `init()`, so adding a tenant namespace mutates the pod-template hash of every control-plane Deployment and restarts the whole cluster.
- **PR `#3476`** ("Watch All Namespaces + Cross-Namespace Invocation Isolation"), also opened by **Stradivario**, is still **open** — it's this fork's `watch-all-namespaces` + `same-namespace-guard` work, offered upstream. Your own comment on it: *"This is like the 4th PR which i am making trying to help the community and yet still i can see nobody gives attention at all."*
- Upstream's `docs/multiple-namespace/prd.md` (landed via `a0756d02`, PR `#3497`) opens by describing `#3298` verbatim as the motivating bug, then explicitly critiques `#3476`'s approach:

  > "The community PR #3476 solves the *operational* symptom (dynamic namespaces without restart) but does so by **watching all namespaces with cluster-wide RBAC** and proposes **disabling internal auth by default** ... plus a **spoof-prone podIP→namespace cache** for invocation isolation. Each of those is a regression on the axis the user ranked #1: security."

So: this isn't two teams independently inventing similar features. Upstream read your PR, agreed on the problem, disagreed on the solution's security posture, and built their own. That's useful context for how to treat their design — it's not a random competing feature, it's a **direct answer to your own design**, worth taking seriously rather than defending against.

### Upstream's actual design (PRD phases, all shipped per `docs/multiple-namespace/implementation-status.md`)

A new cluster-scoped `FissionTenant` CRD + a dedicated `--tenantController` subsystem, with three operator-selectable modes (`tenancy.mode`, default `static` = today's behavior, byte-identical):

| Mode | Behavior |
|---|---|
| `static` | today's `additionalFissionNamespaces` behavior, unchanged (default) |
| `dynamic` | onboard/offboard a namespace at runtime via `fission tenant enable <ns>` or a `fission.io/enabled=true` label, **zero control-plane restart**, per-namespace derived HMAC keys (master key never leaves the control plane) |
| `cluster` | **this is the fork's `watch-all-namespaces` mode, now built natively upstream** — auto-onboards every namespace, opt-out via `fission.io/enabled=false`; the PRD literally says operators choosing this mode "get PR #3476-style cluster-wide RBAC by their own explicit choice" |

Key design differences from the fork's approach, each backed by a specific critique in their `backward-compatibility.md`:

- **RBAC**: only `fission.io` CRDs go cluster-wide (Tier A); Pods/Services/Secrets/ConfigMaps stay per-namespace dynamic (Tier B) even in `dynamic` mode. Your fork's `watch-all-namespaces` grants broader cluster-wide read.
- **Cross-namespace invocation isolation**: admission + NetworkPolicy, not a podIP→namespace cache. Their own doc calls the podIP approach "spoof-prone" (a brand-new pod's status.podIP can lag, which matches the caveat already recorded in this fork's own memory about the same feature).
- **HMAC**: per-namespace *derived* keys with version-aware signing during rollout (old pods still verify with the master key, new pods with the derived key, keyed off a pod-version annotation) — solves the exact "toggle HMAC on/off leaves stale secrets" self-heal problem the fork worked around operationally (`334f312d`/`5946caed`), by not needing a toggle at all.
- Archive isolation: storagesvc now namespace-prefixes archive IDs (`_tenant_/<ns>/<uuid>`), something the fork doesn't have.

**This is a materially better-engineered version of the same idea**, reviewed with an explicit threat model. Re-deriving the same guarantees by hand-patching the fork's current approach onto new upstream router internals (see §4) would be strictly worse than adopting the native feature.

---

## 4. What else is in the 69 upstream-only commits (why this isn't "just pull latest")

Grouped by RFC/theme (`docs/rfc/README.md` on upstream is the index — 20 RFCs, most implemented):

- **Router: near-total rewrite.** `git diff --stat ae970aaa upstream/main -- pkg/router` shows **184 files, +20,533/−3,829** just in router + namespace code. `functionHandler.go` alone changed ~900 lines. New subsystems: `endpointcache/` (EndpointSlice-native data plane, RFC-0002), `routetable/` + `incremental.go` (RFC-0013 incremental route updates), `streaming/` + `stream.go` (RFC-0008 SSE/chunked/WebSocket), `transport.go` (RFC-0014 hot-path rewrite — shared transport, allocation diet), `gatewayapi.go` (RFC-0007, deprecates Ingress), `routeshape.go`, `proxypolicy.go`, `rewrite.go`. This is the exact area the fork's `same_namespace_guard.go` hooks into (`httpTriggers.go`, `router.go`, `mutablemux.go` — all rewritten). The fork's guard file itself is small and portable, but re-attaching its ~13+24 lines of hooks into `buildMuxes`/`router.go` means understanding a effectively-new router, not applying a patch.
- **Buildermgr: moderate rewrite, not total.** `git diff --stat ae970aaa upstream/main -- pkg/buildermgr` → **11 files, +662/−71**. New `registry.go`/`registry_test.go` (OCI registry push, RFC-0001/0012 "OCI-native package delivery" — packages can now be delivered as OCI images instead of only URL/literal archives). `common.go`, `environment_reconciler.go`, `package_reconciler.go` all touched — the same three files the fork's builder-pool feature modifies. Real conflict, but bounded and comprehensible (buildermgr still exists, still reconciler-based, not replaced wholesale).
- **Dependency swaps that affect fork-patched code directly.** `chore(deps): replace hashicorp/go-retryablehttp with a stdlib retry transport (#3505)` removes the library the fork patched (`pkg/executor/client/client.go`, `CheckRetry`) to stop 429s from being retried into a masked 500 (see this fork's own `poolmgr-pod-cap-gotchas` history). The replacement, `pkg/utils/httpretry`, explicitly documents itself as retrying "on network errors / 429 / 5xx-except-501" — i.e. **it still retries on 429**, the same behavior the fork fixed. Adopting this dependency swap as-is would silently reintroduce that bug; the fork's CheckRetry-skip-on-429 logic needs to be re-applied against the new transport, not dropped.
- **Everything else**, briefly: OCI-native packaging as default (RFC-0001/0012), CRD modernization + reconciler consolidation (RFC-0003/0004 — already partly inherited pre-`ae970aaa`, some more since), Gateway API route provider (RFC-0007), streaming invocation (RFC-0008), MCP tool exposure (RFC-0011), invocation correlation/failure attribution (RFC-0015), a whole new OTLP-native logging pipeline (RFC-0016, InfluxDB integration fully removed), CLI debugging toolkit + `fission function run-local` (RFC-0017/0018), full migration off Prometheus `client_golang` onto OTel Metrics API (RFC-0019), an e2e benchmarking suite (RFC-0020), `gorilla/mux` → custom `httpmux`, `gorilla/websocket` → `coder/websocket`, and — as of the very latest commit `1079529c` — a full "portless" refactor of ports/service-discovery/listeners (`pkg/svcinfo`), which is the same architectural neighborhood as the fork's router-internal-listener split (GHSA-3g33-6vg6-27m8 handling).
- Regular `chore(deps)` bumps (several groups) — these are the closest thing to "just security patches," but they're interleaved with the rest, not a separable tail you can cherry-pick without also pulling dependent refactors in some cases.

---

## 5. Direct answer to "can we get up to date with fission's security fixes without much effort?"

**No, not as a single low-effort action**, for two independent reasons:

1. The specific CVEs you'd be chasing (GHSA-qf5v, GHSA-vchh) are **already merged**. There's no discrete "just the security patches" commit range left to grab — what's left upstream is entangled with the RFC rewrites above (dependency bumps sit between and depend on refactor commits in several spots).
2. A `git merge upstream/main` right now would hit real conflicts in exactly the files this fork customizes most (`pkg/router/*`, `pkg/buildermgr/*`, `pkg/executor/client/client.go`, `charts/fission-all/templates/*`, `pkg/apis/core/v1/types.go`, `go.mod`), several of which are conflicts of *design* (two different multitenancy/HMAC models), not just text.

What **is** true: staying on the current fork isn't a security emergency. You're not sitting on a known unpatched CVE — you're accumulating distance from ~4 months of hardening, dependency bumps, and a router rewrite that includes real bug fixes (e.g. `291d37b7 fix(router): expire endpoint quarantines after a TTL`, `b073799d fix(security): confine fetcher file ops to shared volume via os.Root`). The cost of waiting is "more to reconcile later," not "actively vulnerable now."

---

## 6. Recommendation

Don't attempt a wholesale merge, and don't try to hand-reconcile the fork's multitenancy code against upstream's line-by-line. Instead:

1. **Retire, don't merge, the fork's namespace/tenancy code.** Drop `watch-all-namespaces` + `same_namespace_guard.go` + the internal-auth self-heal plumbing, and adopt upstream's native `tenancy.mode: cluster` — it's the documented, opt-in, upstream-maintained equivalent of what you built, with a stronger threat model and no ongoing maintenance burden on your side. This eliminates the single biggest conflict zone (`pkg/router`) almost entirely, since that's exactly where the fork's guard code lives.
2. **Re-port, not merge, the two still-unique features** onto current `upstream/main` (`1079529c`): builder concurrency/scale-to-zero pool, and the build/deploy event webhooks. Both are compact, mostly-new-file features (~4,000 lines combined for everything fork-only) — this is a re-implementation exercise against new reconciler code in `pkg/buildermgr`, not a conflict-resolution slog, and it's the same pattern this repo already executed once (`release/custom-v2.0`) — just against a much newer, further-diverged base now.
3. **Re-apply the 429-no-retry fix** against the new `pkg/utils/httpretry` transport once you're on top of the retryablehttp removal — small, but easy to silently lose.
4. Treat `release/custom-v2.0` as superseded/retired once `develop` is confirmed to have absorbed everything from it (§2) — worth a quick diff check before deleting anything, but nothing to actively reconcile there.
5. Keep `release/custom-v1.22.1` untouched, as before.

This is still a real project — re-porting two features onto a router/buildermgr that have both moved substantially is not a rebase, it's closer to what `release/custom-v2.0` did the first time — but it is bounded, and it ends with the fork actually *smaller* (net less custom code to maintain) and *automatically* getting upstream's future security fixes in the areas that mattered most (router, HMAC, RBAC), because those are no longer forked.

**Not decided by me, needs your call:**
- Do you actually want to give up `same_namespace_guard.go`'s podIP-based model, or do you have a reason (e.g. can't run NetworkPolicies in your target clusters, per the `router-internal-function-path` memory noting minikube's CNI doesn't enforce them) that still favors an application-layer guard regardless of upstream's critique? Upstream's model leans on NetworkPolicy + admission; if your deployment targets don't reliably enforce NetworkPolicies, their isolation story is weaker there than the podIP guard in practice, even if the podIP guard is "spoof-prone" in principle.
- Whether to also pursue PR `#3476` further upstream now that you can see exactly why it stalled, or just quietly diverge (drop the PR, adopt `tenancy.mode: cluster` downstream).
- Priority/order: tenancy cutover first, or builder-pool/webhook re-port first.

I've made no branch changes, commits, or pushes — this is analysis only.
