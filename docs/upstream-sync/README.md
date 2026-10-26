# Upstream syncs

`release/custom-v3.0` is the fork's release line, and it is always
`upstream/main` + the fork's commits **on top** (no merge commits).
`git log upstream/main..origin/release/custom-v3.0` is exactly the fork's
patch set. One note per sync lives here (`<date>.md`).

## How a sync is done

1. Replay on a temporary branch:

   ```bash
   git fetch upstream origin
   git switch -c rebase/upstream-<date> upstream/main
   git cherry-pick <fork commits of release/custom-v3.0, oldest first>
   ```

   Run `go build ./...` after **every** pick (auto-merges can break code
   without a conflict marker), then `go vet ./...` and the unit suite. Write
   `<date>.md`.
2. Check that no fork commit was lost. Every subject in
   `git log <rebase>..origin/release/custom-v3.0` must also be on the rebase
   branch.
3. Add the release bump commit: `charts/fission-all/Chart.yaml`
   (version/appVersion), `values.yaml` (3x imageTag), `install_custom.sh`.
4. Move the release line onto the rebase branch. This is a force-push, made
   safe by a backup tag and a lease on the current tip:

   ```bash
   old=$(git rev-parse origin/release/custom-v3.0)
   git tag backup/release-custom-v3.0-<date> "$old"
   git push origin backup/release-custom-v3.0-<date>
   git push --force-with-lease=release/custom-v3.0:"$old" \
     origin rebase/upstream-<date>:release/custom-v3.0
   ```

   The push touches `charts/**`, so `chart-publish.yaml` publishes the new
   chart. Other checkouts: `git fetch && git reset --hard
   origin/release/custom-v3.0`.
5. Tag the release: `git tag vX.Y.Z-watch-all-namespaces && git push origin
   vX.Y.Z-watch-all-namespaces`. `release.yaml` (goreleaser) builds the
   `ghcr.io/stradivario/*` images.
6. Delete the temporary branch. Production changes only with a `helm upgrade`
   (never `install_custom.sh` against production).

Why not merge: a merge commit ends "fork commits on top", and every later
sync would replay through it. Why not a new branch per sync:
`chart-publish.yaml` and the docs name `release/custom-v3.0`.
