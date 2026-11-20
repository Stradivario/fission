# idle-reaper-hpa-fight

**Status:** Active (2026-10-31, `release/custom-v3.0`) · not yet in a published image.

## The bug

The executor's idle reaper (`pkg/executor/reaper/idle`, `ScaleDownStrategy`)
scaled every newdeploy/container Deployment back to `MinScale` once the function
had seen no **router** traffic for `idletimeout`, regardless of its HPA. A
function with `MinScale 1, MaxScale > 1` that the HPA scaled up on CPU was
forced back to 1 within one reaper tick (5 s); the HPA's 5-minute scale-down
stabilization then re-asserted the higher count on its next sync. Result: a pod
created and killed ~1 s after start, every ~15 s, per function.

Production, 2026-10-30/31: Deployment `generation` in the hundreds,
`spec.replicas` owned by `fission-bundle` via `/scale`, the executor logging
`scaling down idle deployment` for opencost ~every 13 s. The doomed pods went
`Pending` (Insufficient cpu), Karpenter provisioned nodes for them, then
consolidated the underutilized nodes again — evicting every function and
restarting the loop (`failed to sync secret cache` on each fresh node).

## The fix

`ScaleDownStrategy.Reap` returns early when `MinScale > 0`. The HPA
(`minReplicas = MinScale`, `pkg/executor/util/hpa`) owns every replica above the
floor and scales down by itself. The reaper keeps doing the one thing an HPA
cannot: scaling an idle `MinScale 0` function to zero.

Not affected: poolmgr reaping (`PoolDeleteStrategy`) and builder scale-to-zero
(buildermgr's own idle reaper).

## How to test

```bash
go test -race ./pkg/executor/reaper/idle/ -run 'TestScaleDownStrategy_Reap|TestReaperMetrics'
```

Live: after rollout the executor log stops showing `scaling down idle
deployment` for `MinScale >= 1` functions, and the HPA `SuccessfulRescale`
events / Deployment `generation` stop climbing.
