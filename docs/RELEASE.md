# Releasing a new version of this Fission fork

Single source of truth for cutting a release of this fork (`release/custom-v1.22.1`,
the `*-watch-all-namespaces` builds). It publishes to the **`stradivario`** GitHub
org, the **`ghcr.io/stradivario`** container registry, and the Helm chart repo at
**`https://stradivario.github.io/fission`** (added locally as `fission-custom`).

A release has **two halves**, driven by two GitHub Actions:

| Half | Trigger | Workflow | Produces |
|------|---------|----------|----------|
| **Images + GitHub release** | push a **git tag** `v1.**` / `v2.**` | `.github/workflows/release.yaml` (GoReleaser) | images on `ghcr.io/stradivario/*` (`:latest` + `:<tag>`), a **draft** pre-release |
| **Helm chart** | **push to `release/custom-v1.22.1`** changing `charts/**` | `.github/workflows/chart-publish.yaml` (chart-releaser) | chart published to the gh-pages helm repo |

CI does **not** pick the version number — you bump it by hand (see below), then the
tag drives the images and the branch push drives the chart.

---

## TL;DR — cut version `X.Y.Z` (example below uses `1.22.7 → 1.22.8`)

```bash
# 0. clean tree on the fork branch
git checkout release/custom-v1.22.1
git pull
git status                                   # working tree clean

# 1. sanity build/test (regen CRDs/swagger only if you changed pkg/apis types)
go build ./pkg/... && go test ./pkg/buildermgr/... ./pkg/executor/...
# make generate-crds && make generate-swagger-doc   # only if types.go changed

# 2. commit your CODE change (NOT the version bump yet)
git add -A
git commit -m "fix(...): what you changed"

# 3. tag THAT commit + push the TAG -> release.yaml builds & pushes images, draft release
git tag v1.22.8-watch-all-namespaces
git push origin v1.22.8-watch-all-namespaces
#    ...watch Actions "Create Draft release"; when green, confirm images in GHCR,
#    then MANUALLY publish the draft release on GitHub.

# 4. NOW bump the FOUR version fields, in a commit that is NEWER than the tag
#    Chart.yaml: version 1.22.8-watch-all-namespaces, appVersion v1.22.8-watch-all-namespaces
#    values.yaml: imageTag, fetcher.imageTag, preUpgradeChecks.imageTag = v1.22.8-watch-all-namespaces
git add charts/
git commit -m "Bump to v1.22.8-watch-all-namespaces"

# 5. push the BRANCH (charts changed) -> chart-publish.yaml publishes the chart
git push origin release/custom-v1.22.1

# 6. verify + upgrade a cluster
helm repo update
helm search repo fission-custom/fission-all --versions    # 1.22.8 should appear
helm upgrade --install fission fission-custom/fission-all -n fission \
  --version 1.22.8-watch-all-namespaces
```

> **Why this exact order (read the "chart didn't publish" gotcha below):**
> 1. Images must exist before the chart references them → **tag first**.
> 2. chart-releaser only publishes charts that changed **since the latest git tag**,
>    so the **version-bump commit must be newer than the tag**. If the bump and the
>    tag are the same commit, chart-releaser sees an empty `charts/` diff, logs
>    `Nothing to do. No chart changes detected.`, and silently skips publishing.

---

## Version source of truth (bump these by hand — CI does not)

| File | Field(s) | Approx. line | Value for `1.22.8` |
|------|----------|--------------|--------------------|
| `charts/fission-all/Chart.yaml` | `version` | 3 | `1.22.8-watch-all-namespaces` (no leading `v`) |
| `charts/fission-all/Chart.yaml` | `appVersion` | 4 | `v1.22.8-watch-all-namespaces` (with `v`) |
| `charts/fission-all/values.yaml` | `imageTag` | ~28 | `v1.22.8-watch-all-namespaces` |
| `charts/fission-all/values.yaml` | `fetcher.imageTag` | ~134 | `v1.22.8-watch-all-namespaces` |
| `charts/fission-all/values.yaml` | `preUpgradeChecks.imageTag` | ~716 | `v1.22.8-watch-all-namespaces` |

**Tag format:** `vX.Y.Z-watch-all-namespaces` (e.g. `v1.22.8-watch-all-namespaces`).
The git tag and `appVersion` keep the leading `v`; `Chart.yaml` `version` omits it.

### ⚠️ The imageTag gotcha (read this)

The chart resolves the image tag from **`.Values.imageTag`**, *not* from
`.Chart.AppVersion` — see `charts/fission-all/templates/_helpers.tpl`:

```
{{- $args := list .Values.repository .Values.image .Values.imageTag -}}
```

So if you bump `Chart.yaml` but **forget `values.yaml`**, the new chart version
will deploy the **old** image tag. This actually happened: `Chart.yaml` was bumped
to `1.22.6` while the three `values.yaml` `imageTag`s stayed at `v1.22.5`, so the
published `1.22.6` chart pulls `v1.22.5` images. **Always bump all four fields
together** (and realign them if they have drifted).

---

## What the automation does (detail)

### `release.yaml` — binaries + images + GitHub release
- **Trigger:** pushing a git **tag** matching `v1.**` or `v2.**`.
- Runs `hack/build-yaml.sh $VERSION` (generates install YAML manifests) then
  `goreleaser release`, which:
  - builds the 6 binaries (`builder`, `fetcher`, `fission-bundle`, `fission-cli`,
    `pre-upgrade-checks`, `reporter`);
  - builds & pushes 5 images to
    `ghcr.io/stradivario/{builder,fetcher,fission-bundle,pre-upgrade-checks,reporter}`,
    each tagged **`:latest`** and **`:<tag>`** (`.goreleaser.yml`, `GHCR_REPO=ghcr.io/stradivario`);
  - creates a **DRAFT pre-release** on `stradivario/fission`
    (`release.github` → `draft: true`, `prerelease: "true"`);
  - signs everything with Cosign and attaches SBOM/provenance.
- **Auth:** the built-in `GITHUB_TOKEN` only — no extra secrets to configure.

### `chart-publish.yaml` — Helm chart
- **Trigger:** a **push to branch `release/custom-v1.22.1`** that changes anything
  under `charts/**`.
- Runs `helm/chart-releaser-action`, which packages the chart and publishes it to
  the **gh-pages** chart repo (creates a GitHub release per chart version and
  updates `index.yaml`), served at `https://stradivario.github.io/fission`.

---

## Pre-flight checklist

- [ ] On `release/custom-v1.22.1`, tree clean, `git pull` done.
- [ ] `go build ./pkg/...` passes; relevant tests pass
      (`go test ./pkg/buildermgr/... ./pkg/executor/...`).
- [ ] If you changed `pkg/apis/core/v1/types.go`: ran `make generate-crds` and
      `make generate-swagger-doc`, and committed the regenerated
      `crds/`, `zz_generated.*` files.
- [ ] Added/updated a `docs/features/` entry for anything user-visible
      (the index lives in [`features/README.md`](features/README.md)).
- [ ] Bumped **all four** version fields (table above), kept in sync.
- [ ] (Optional but recommended) tested the build locally first — see
      [`LOCAL_DEVELOPMENT.md`](LOCAL_DEVELOPMENT.md) (`package_local.sh` → minikube,
      or skaffold → kind).

---

## Verify the images landed (after the tag push)

```bash
# the tags should resolve in GHCR once "Create Draft release" is green
docker manifest inspect ghcr.io/stradivario/fission-bundle:v1.22.8-watch-all-namespaces >/dev/null && echo OK
# or browse: https://github.com/orgs/Stradivario/packages
```

Then **publish the draft release** on GitHub (Releases → the draft → Publish) — it
stays hidden until you do.

---

## Verify the chart published (after the branch push)

```bash
helm repo update
helm search repo fission-custom/fission-all --versions   # new version appears
```

If `fission-custom` isn't added yet:

```bash
helm repo add fission-custom https://stradivario.github.io/fission
helm repo update
```

---

## Gotchas

- **"Release Charts" is green but the chart didn't publish** (`Nothing to do. No
  chart changes detected.`). chart-releaser publishes only charts whose files
  changed **since the latest git tag** (`git diff <latest-tag> -- charts/`). If you
  put the version bump in the **same commit** you tag, that diff is empty and the
  chart is silently skipped — the images publish but the chart does not (this hit
  1.22.8). **Prevent:** keep the bump in a commit *newer* than the tag (steps 3→4
  above). **Recover:** make any fresh `charts/` commit (e.g. add an
  `artifacthub.io/changes` annotation to `Chart.yaml`) and push the branch — the
  diff is now non-empty, so chart-releaser publishes `fission-all-<version>`.
  Confirm with a `fission-all-<version>` tag appearing on the remote.
- **imageTag vs appVersion** — the chart uses `.Values.imageTag`; bump `values.yaml`
  too (see the ⚠️ section above).
- The GoReleaser GitHub release is a **draft** — invisible until you publish it.
- **`:latest` images are overwritten on every release** — don't rely on `:latest`
  for reproducibility; pin `:<tag>`.
- **Order:** tag (images) first, then branch (chart).
- The `Makefile` `release` target (`hack/release.sh`) is the **upstream** flow and
  assumes the `main` branch — **not** used for this fork. Use the tag-push flow.
- `CHANGELOG.md` is auto-generated from upstream GitHub PRs (`hack/changelog.sh`)
  and gets overwritten — don't hand-edit it. Track fork-specific changes in
  [`docs/features/`](features/README.md).
- `chart-publish.yaml` only fires on the `release/custom-v1.22.1` branch and only
  when files under `charts/**` change. Editing only `docs/` won't republish a chart.

---

## Related

- Local build & test (minikube via `package_local.sh`, or kind via skaffold):
  [`LOCAL_DEVELOPMENT.md`](LOCAL_DEVELOPMENT.md).
- Fork-specific features & fixes change log: [`features/README.md`](features/README.md).
