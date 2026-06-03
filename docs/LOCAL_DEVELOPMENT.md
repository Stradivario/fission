# Local Development — build & test this Fission fork

How to build Fission from this repository (`release/custom-v1.22.1`, the
`*-watch-all-namespaces` builds) and run it on a local [kind](https://kind.sigs.k8s.io/)
cluster.

Every command below is taken from the repo's `Makefile`, `skaffold.yaml` and
`kind.yaml` — nothing invented.

---

## 1. Prerequisites

| Tool | Version this repo expects | Notes |
|------|---------------------------|-------|
| Go | from `go.mod` (currently **1.25.5**) | |
| Docker | any recent; **daemon must be running** | builds & loads images |
| kind | **v0.30.0** | local Kubernetes cluster |
| goreleaser | v2 | used by `make build-fission-cli` and `make skaffold-deploy` |
| skaffold | **v2.17.0** | build + deploy loop |
| helm | CI uses **v4.0.1** (local v3.x works) | chart install |
| kubectl | matching your cluster | |
| golangci-lint | optional | only for `make check` / `code-checks` |

Quick check + install the two that are commonly missing:

```bash
# check what you already have
for t in go docker kind skaffold helm kubectl goreleaser; do
  printf '%-12s ' "$t"; command -v "$t" >/dev/null && echo OK || echo MISSING
done

# install the usual missing two (into $(go env GOPATH)/bin — make sure it's on PATH)
go install sigs.k8s.io/kind@v0.30.0
go install github.com/goreleaser/goreleaser/v2@latest
```

---

## 2. Build & install the `fission` CLI

```bash
make build-fission-cli            # goreleaser snapshot -> dist/fission-cli_<os>_<arch>_v1/fission
sudo make install-fission-cli     # moves it to /usr/local/bin/fission
fission version
```

- Override the embedded version with `make build-fission-cli VERSION=dev`
  (defaults to `v0.0.0`).
- `install-fission-cli` hardcodes the `..._v1` dist path (GOAMD64=v1, i.e. amd64).
  On arm the dist directory name differs — copy the binary by hand from `dist/`.
- **No goreleaser?** Quick fallback:
  `go build -o /usr/local/bin/fission ./cmd/fission-cli`.

---

## 3. Create a local cluster and deploy Fission

```bash
# 3.1 — kind cluster (1 control-plane node, host ports 80/443; see kind.yaml)
kind create cluster --config kind.yaml --name kind

# 3.2 — install the CRDs
make create-crds                          # kubectl create -k crds/v1

# 3.3 — build images locally + deploy the chart via skaffold
SKAFFOLD_PROFILE=kind make skaffold-deploy
```

What `make skaffold-deploy` does (`Makefile` `skaffold-prebuild` + `skaffold-deploy`):

1. `goreleaser build` compiles linux/amd64 binaries into `dist/`.
2. The `cmd/*/Dockerfile`s are copied next to those binaries and the
   `$TARGETPLATFORM/` prefix is stripped.
3. `skaffold run -p kind` builds the **fission-bundle**, **fetcher**,
   **pre-upgrade-checks** and **reporter** images, side-loads them into the kind
   cluster, and `helm install`s `charts/fission-all` into the **`fission`**
   namespace.

The `kind` profile (`skaffold.yaml`) sets `repository=""` (use the locally built
images instead of pulling from `ghcr.io/stradivario`) and
`routerServiceType=NodePort`. Other profiles: `kind-debug` (pprof + debug env),
`kind-ci` (monitoring on).

Verify:

```bash
fission version
kubectl get pods -n fission
```

---

## 4. Smoke-test a function

Uses the upstream Node environment image (same images the CI uses in
`test/kind_CI.sh`):

```bash
fission env create --name nodejs --image ghcr.io/fission/node-env-22

echo 'module.exports = async () => ({ status: 200, body: "hello\n" });' > hello.js
fission fn create --name hello --env nodejs --code hello.js
fission fn test --name hello          # expect: hello
```

### Optional: exercise the builder scale-to-zero feature

This fork can scale idle builders to zero (see
[features/builder-scale-to-zero.md](features/builder-scale-to-zero.md)):

```bash
fission env create --name node-src \
  --image   ghcr.io/fission/node-env-22 \
  --builder ghcr.io/fission/node-builder-22 \
  --builder-idletimeout 120        # scale builder to 0 after 120s idle (0 disables)

# watch the builder deployment scale 1 -> 0 when idle, and back to 1 on a build
kubectl get deploy -n fission-builder -w
```

---

## 5. Iterate & tear down

```bash
# rebuild + redeploy after code changes
SKAFFOLD_PROFILE=kind make skaffold-deploy

# remove the release / CRDs / cluster
skaffold delete -p kind
make delete-crds
kind delete cluster --name kind
```

---

## Notes

- This fork installs into the `fission` namespace and, by default,
  **watches all namespaces** (`watchAllNamespaces: true` in
  `charts/fission-all/values.yaml`).
- Running tests: `hack/runtests.sh` (or `make test-run`); for the builder package
  with the race detector: `go test -race ./pkg/buildermgr/...`.
- Releasing a new version is documented in [RELEASE.md](RELEASE.md).
