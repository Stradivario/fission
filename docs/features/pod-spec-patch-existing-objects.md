# pod-spec-patch-existing-objects

**Status:** Active (2026-10-25, `release/custom-v3.0`) · not yet in a published image.

## The bug

`runtimePodSpec` / `builderPodSpec` (chart values → the
`runtime-podspec-patch` / `builder-podspec-patch` ConfigMaps) were applied only
to objects created after the executor/buildermgr started with them:

- **newdeploy**: adopting an existing Deployment (every executor start) copied
  only containers, service account and grace period from the computed spec, so
  a newly added nodeSelector never reached running functions.
- **builders**: an existing builder Deployment (named `<env>-<resourceVersion>`)
  is "already present" and never updated until its Environment changes.
- poolmgr pools were already rebuilt from the current spec.

Motivating use: pinning every function and builder pod to
`kubernetes.io/arch: amd64` after Karpenter placed function pods on an ARM
(`cax21`) node, where the amd64-only runtime images fail with
`exec format error` (production outage 2026-10-25).

## The fix

- `pkg/executor/executortype/newdeploy/newdeploy.go`: `adoptDeployment` also
  copies `nodeSelector`, `affinity` and `tolerations`.
- `pkg/buildermgr/environment_reconciler.go`: `syncBuilderNodeSelector` adds the
  patch's nodeSelector keys an existing builder lacks (keys the builder sets are
  kept, as in `createBuilderDeployment`'s merge).

The pin itself is configuration (`fission-autopilot-lowcost.yaml`):

```yaml
runtimePodSpec:
  enabled: true
  podSpec:
    securityContext: null        # drop the chart default (runAsNonRoot, uid 10001)
    nodeSelector:
      kubernetes.io/arch: amd64
builderPodSpec:
  enabled: true
  podSpec:
    securityContext: null
    nodeSelector:
      kubernetes.io/arch: amd64
```

`securityContext: null` is required: helm merges maps, so the chart default
would otherwise switch every function pod to runAsNonRoot uid 10001.

## How to test

```bash
go test -race ./pkg/executor/executortype/newdeploy/ -run TestAdoptDeploymentCarriesScheduling
go test -race ./pkg/buildermgr/ -run TestSyncBuilderNodeSelector
```

Live (verified on minikube): upgrade with the values above; every
`newdeploy-*`, `poolmgr-*` and builder Deployment gets
`nodeSelector: {kubernetes.io/arch: amd64}`; buildermgr logs
`added pod spec patch node selector to builder deployment` once per builder.
