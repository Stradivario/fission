# Docs (this fork)

Documentation specific to this Fission fork (`release/custom-v1.22.1`, the
`*-watch-all-namespaces` builds).

- [SIMPLE_RELEASE.md](SIMPLE_RELEASE.md) — the short, linear flow:
  change → test on minikube → tag → push → release. Start here.
- [LOCAL_DEVELOPMENT.md](LOCAL_DEVELOPMENT.md) — build the binaries/images and run
  Fission locally (minikube via `package_local.sh`, or kind via skaffold), plus a
  function smoke test.
- [RELEASE.md](RELEASE.md) — full release reference: workflows, version fields,
  gotchas (images to `ghcr.io/stradivario`, chart to gh-pages).
- [features/](features/README.md) — running log of features and fixes added in
  this fork (kept here instead of the auto-generated top-level `CHANGELOG.md`).
