# Releasing a new version of this Fission fork

This fork (`release/custom-v1.22.1`, the `*-watch-all-namespaces` builds) publishes
to the **`stradivario`** GitHub org and **`ghcr.io/stradivario`** container
registry. A release is split across two GitHub Actions workflows: one builds and
pushes the binaries/images, the other publishes the Helm chart.

---

## What the automation does

### `.github/workflows/release.yaml` — binaries + images + GitHub release
- **Trigger:** pushing a git **tag** matching `v1.**` or `v2.**`.
- Runs `hack/build-yaml.sh $VERSION` (generates the install YAML manifests) then
  `goreleaser release`, which:
  - builds the 6 binaries (`builder`, `fetcher`, `fission-bundle`, `fission-cli`,
    `pre-upgrade-checks`, `reporter`);
  - builds & pushes 5 images to
    `ghcr.io/stradivario/{builder,fetcher,fission-bundle,pre-upgrade-checks,reporter}`,
    each tagged **`:latest`** and **`:<tag>`** (`.goreleaser.yml`);
  - creates a **DRAFT pre-release** on `stradivario/fission`
    (`.goreleaser.yml` → `release.github`, `draft: true`, `prerelease: "true"`);
  - signs everything with Cosign and attaches SBOM/provenance.
- **Auth:** uses the built-in `GITHUB_TOKEN` only — no extra secrets to configure.

### `.github/workflows/chart-publish.yaml` — Helm chart
- **Trigger:** a **push to branch `release/custom-v1.22.1`** that changes anything
  under `charts/**`.
- Runs `helm/chart-releaser-action`, which packages the chart and publishes it to
  the **gh-pages** chart repo (updates `index.yaml`).

---

## Version source of truth (bump these by hand — CI does not)

| File | Field(s) | Line |
|------|----------|------|
| `charts/fission-all/Chart.yaml` | `version`, `appVersion` | `3-4` |
| `charts/fission-all/values.yaml` | `imageTag` | `28` |
| `charts/fission-all/values.yaml` | `fetcher.imageTag` | `134` |
| `charts/fission-all/values.yaml` | `preUpgradeChecks.imageTag` | `716` |

All of these currently read `v1.22.5`/`1.22.5-watch-all-namespaces`. Keep them in
sync with the tag you push.

**Tag format:** `vX.Y.Z-watch-all-namespaces` — e.g. `v1.22.7-watch-all-namespaces`.
(The `Chart.yaml` `version` omits the leading `v`; `appVersion` and the git tag
keep it.)

---

## Step-by-step

Order matters: build & push the images **before** publishing a chart that points
at them, so users never get a chart referencing a tag that doesn't exist yet.

```bash
# 0. start clean on the fork branch
git checkout release/custom-v1.22.1
git pull
git status            # working tree clean

# 1. sanity build/test (regenerate CRDs/swagger if you changed pkg/apis types)
go build ./pkg/... && go test ./pkg/buildermgr/...
# make generate-crds && make generate-swagger-doc   # only if types.go changed

# 2. bump the version (example: 1.22.6 -> 1.22.7)
#    - charts/fission-all/Chart.yaml : version + appVersion
#    - charts/fission-all/values.yaml: imageTag, fetcher.imageTag, preUpgradeChecks.imageTag
#    Also add a docs/features/ entry describing what's shipping.

# 3. commit the bump
git add charts/ docs/
git commit -m "Bump to v1.22.7-watch-all-namespaces"

# 4. tag and push the TAG -> triggers release.yaml (images + draft release)
git tag v1.22.7-watch-all-namespaces
git push origin v1.22.7-watch-all-namespaces
```

5. Watch **Actions → "Create Draft release"**. When it finishes, confirm the images
   show up under the org's GHCR packages, then **manually publish the draft
   release** on GitHub (goreleaser leaves it as a draft pre-release).

```bash
# 6. push the branch commit (charts changed) -> triggers chart-publish.yaml
git push origin release/custom-v1.22.1
```

7. Verify the chart was published:

```bash
helm repo update
helm search repo <your-repo>/fission-all --versions   # new version should appear
# upgrade a test cluster:
helm upgrade --install fission <your-repo>/fission-all -n fission --version 1.22.7-watch-all-namespaces
```

---

## Gotchas

- The goreleaser GitHub release is a **draft** — it won't be visible until you
  publish it manually.
- `:latest` images are overwritten on every release.
- The `Makefile` `release` target (`hack/release.sh` etc.) is the **upstream**
  flow and assumes the `main` branch — it is **not** used for this fork. Use the
  tag-push flow above.
- `CHANGELOG.md` is auto-generated from upstream GitHub PRs (`hack/changelog.sh`)
  and gets overwritten — don't hand-edit it. Track fork-specific changes in
  [`docs/features/`](features/README.md).
- The chart-publish workflow only fires on the `release/custom-v1.22.1` branch and
  only when files under `charts/**` change.
