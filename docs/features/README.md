# Custom Features & Fixes

This directory tracks features and fixes introduced in **this fork** of Fission
(the `*-watch-all-namespaces` custom builds), separate from the upstream
[`CHANGELOG.md`](../../CHANGELOG.md), which is auto-generated from upstream
GitHub PRs and should not be hand-edited.

## How to add an entry

1. Add a row to the index below (newest first).
2. For anything non-trivial, create a dedicated page `docs/features/<slug>.md`
   describing motivation, behavior, configuration, files touched, and tests.
3. Keep the entry honest about status (e.g. "merged", "in review", "not yet
   tested on a live cluster").

## Index

| Date | Type | Title | Details | Status |
|------|------|-------|---------|--------|
| 2026-06-03 | Fix | newdeploy: wait for package build before provisioning | [newdeploy-wait-for-build.md](newdeploy-wait-for-build.md) | Implemented; build/unit verified. Live e2e pending. |
| 2026-05-30 | Feature | Scale-to-zero for builders | [builder-scale-to-zero.md](builder-scale-to-zero.md) | Implemented; unit-tested. Live-cluster e2e pending. |
