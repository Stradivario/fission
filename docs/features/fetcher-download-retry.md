# fetcher-download-retry

**Status:** Active (2026-10-25, `release/custom-v3.0`) · not yet in a published image.

## The bug

The fetcher downloaded a function's package archive exactly once. On a node
that had just joined the cluster, the node-local DNS and CNI are not up for the
first seconds, so the lookup of storagesvc failed:

```
failed to download url from archive http://storagesvc.fission/v1/archive?id=...:
dial tcp: lookup storagesvc.fission on 10.96.0.10:53: dial udp 10.96.0.10:53: connect: no route to host
```

The specialize request ended there; the pod stayed Running but not-ready
(`1/2`) until something replaced it. Production, 2026-10-25: after the ARM-node
incident, 6 functions sat unready on the fresh amd64 node this way.

## The fix

`pkg/fetcher/retry.go`: `downloadWithRetry` retries the archive download (both
the package archive and FETCH_URL) on `archiveDownloadRetrySchedule` — 250 ms
doubling to 4 s, ~28 s total — when `isTransientDownloadError` holds: a network
failure (DNS, dial, timeout, reset, also mid-body) or a 429/5xx. A 4xx (e.g. a
missing archive's `404 Not Found`), a local file error or a cancelled context
fails immediately, as before. `utils.DownloadUrl*` now return a typed
`*utils.HTTPStatusError` (same message, the bare status line) so the status can
be classified.

## How to test

```bash
go test -race ./pkg/fetcher/ -run 'TestIsTransientDownloadError|TestDownloadWithRetry|TestArchiveDownloadRetrySchedule'
```

Live (verified on minikube): scale `deploy/storagesvc -n fission` to 0, restart
a function whose Package is a storagesvc `url` archive, scale storagesvc back
within ~25 s. The fetcher logs `transient error downloading archive, retrying`
with increasing `attempt`, then `successfully placed` and the pod goes `2/2`
without a restart.

## How to debug

`transient error downloading archive, retrying` with `attempt` reaching 10 and
then `failed to download url from archive` → the archive source stayed
unreachable for ~28 s; check storagesvc and DNS on that node.
