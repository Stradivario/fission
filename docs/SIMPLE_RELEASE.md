# Simple flow: change → test on minikube → release

The short, linear checklist. For detail see
[`LOCAL_DEVELOPMENT.md`](LOCAL_DEVELOPMENT.md) (local build) and
[`RELEASE.md`](RELEASE.md) (publishing).

**Before you start:** minikube running, Docker up, `helm` installed, and you're on
branch `release/custom-v1.22.1` with a clean tree.

---

## 1. Make your code change

Edit code under `pkg/...` (or the chart under `charts/...`). Quick sanity:

```bash
go build ./pkg/... && go test ./pkg/buildermgr/... ./pkg/executor/...
```

> If you changed `pkg/apis/core/v1/types.go`, also run
> `make generate-crds && make generate-swagger-doc`.

## 2. Build the code + load images into minikube

```bash
./package_local.sh
```

Builds the 4 binaries, packages the chart, builds `ghcr.io/fission/*:local-<timestamp>`
images, side-loads them into minikube, and **prints a `helm upgrade` command**.

## 3. Deploy to minikube & test

Run the printed command (substitute the `local-<timestamp>` it gave you):

```bash
helm upgrade --install fission ./dist-local/fission-all-1.22.0-local.tgz \
  --namespace fission --create-namespace \
  --set repository=ghcr.io/fission        --set imageTag=local-<timestamp> \
  --set fetcher.repository=ghcr.io/fission --set fetcher.imageTag=local-<timestamp> \
  --set preUpgradeChecks.repository=ghcr.io/fission --set preUpgradeChecks.imageTag=local-<timestamp> \
  --set analytics=false --set pullPolicy=Never
```

Verify the control plane rolled and a function still works:

```bash
kubectl get pods -n fission
kubectl get pod -n fission -l svc=executor \
  -o jsonpath='{.items[0].spec.containers[0].image}'    # -> ...:local-<timestamp>

fission fn test --name <some-fn> -n <project-ns>         # or deploy a lambda via the app and curl it
```

**Iterate steps 1–3 until it works.**

## 4. Commit your change

```bash
git add -A
git commit -m "fix(...): what you changed"
```

## 5. Tag + push the TAG → builds & pushes images (draft release)

Tag the commit from step 4 (your code change):

```bash
git tag v1.22.8-watch-all-namespaces
git push origin v1.22.8-watch-all-namespaces
```

Watch **GitHub → Actions → "Create Draft release"**. When it's green, confirm the
images appear in GHCR, then **manually publish the draft release** on GitHub
(GoReleaser leaves it as a draft).

## 6. Bump the version (all FOUR fields) — in a NEW commit, AFTER the tag

Pick the next version (e.g. `1.22.7` → `1.22.8`) and set:

- `charts/fission-all/Chart.yaml` → `version: 1.22.8-watch-all-namespaces` (no `v`),
  `appVersion: v1.22.8-watch-all-namespaces` (with `v`)
- `charts/fission-all/values.yaml` → `imageTag`, `fetcher.imageTag`,
  `preUpgradeChecks.imageTag` → all `v1.22.8-watch-all-namespaces`

```bash
git add charts/
git commit -m "Bump to v1.22.8-watch-all-namespaces"
```

> ⚠️ Two traps here:
> - The chart reads the image tag from `values.yaml` `imageTag`, **not**
>   `appVersion` — bump Chart.yaml only and the new chart ships the old images.
> - This bump **must be a commit newer than the tag** from step 5. chart-releaser
>   only publishes charts that changed *since the latest git tag*; if the bump and
>   the tag are the same commit it logs `No chart changes detected` and skips.

## 7. Push the BRANCH → publishes the Helm chart

```bash
git push origin release/custom-v1.22.1
```

Fires **"Release Charts"** (because `charts/**` changed) and updates the helm repo.
Confirm a `fission-all-1.22.8-watch-all-namespaces` tag appears on the remote.

## 8. Install / upgrade from the published chart

```bash
helm repo update
helm upgrade --install fission fission-custom/fission-all \
  -n fission --version 1.22.8-watch-all-namespaces
```

---

### Remember
- **Tag the code commit first** (images), **then bump the chart in a newer commit**
  (chart). Images must exist before the chart points at them, and chart-releaser
  only publishes charts changed *since the latest tag*.
- The GitHub release is a **draft** until you publish it.
- Bump **all four** version fields together (the chart reads `imageTag` from
  `values.yaml`, not `appVersion`).
