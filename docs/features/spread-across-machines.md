# Spread a function's replicas across machines

**Status:** Active — not in a published image yet (Lambforge plan 33).

## Problem

A newdeploy function with `minScale: 3` on a 3-machine cluster could end up
with 2 or 3 of its pods on one machine: the scheduler's built-in default
spreading is soft and allows a skew of 3 per hostname. Losing that machine
took most of the function's capacity at once.

## Change

Two **Function annotations** (metadata, not spec — older executors ignore
them, no CRD change):

| Annotation | Value | Effect |
|---|---|---|
| `lambforge.io/spread` | `machines` / `zones` | topology key `kubernetes.io/hostname` / `topology.kubernetes.io/zone` |
| `lambforge.io/spread-strict` | `"true"` | `DoNotSchedule` instead of `ScheduleAnyway` |

`pkg/executor/executortype/newdeploy/spread.go` — `applySpread` adds one
`topologySpreadConstraint` to the Deployment's pod template, last, when the
template is complete (the function's choice replaces an environment constraint on
the same key; others are kept):

```yaml
maxSkew: 1
topologyKey: kubernetes.io/hostname
whenUnsatisfiable: ScheduleAnyway      # DoNotSchedule when strict
labelSelector:                         # a rollout does not count the old pods:
  matchLabels:                         #   the pods carry a hash of their template
    functionUid: <uid>
    lambforge.io/spread-rev: <hash>
nodeTaintsPolicy: Honor                # tainted / unreachable machines and
nodeAffinityPolicy: Honor              #   machines the pod cannot use don't count
```

- Soft never blocks a scale-up; strict keeps a replica Pending rather than
  putting two on one machine while another usable machine has fewer. A
  machine that is down is tainted `unreachable`, so with `Honor` it does not
  keep a strict replica Pending.
- Rolling it: an annotation change does not move `Generation`, so the
  Function reconciler's predicate also passes changes to these annotations
  (`spreadAnnotationsPredicate`, `pkg/executor/funcreconciler`), and
  `podTemplateChanged` treats them as pod template fields (`spreadChanged`).
- Adopting an orphan Deployment carries `TopologySpreadConstraints` and the
  `lambforge.io/spread-rev` pod label.
- **Why not `matchLabelKeys: [pod-template-hash]`** (1.29.0 did that): a
  virtual cluster cannot run it. Its API server merges the key into the
  label selector (Kubernetes >= 1.34), and the copy of the pod on the host
  is refused: `"pod-template-hash": exists in both matchLabelKeys and
  labelSelector` - the new pod stays Pending with a `SyncError` event and
  the rollout hangs (old pods keep serving). Seen 2026-11-17, vCluster 0.37
  on Kubernetes 1.35. Since 1.29.1 the executor labels the pod template
  with a hash of the template (`spreadRevision`, 12 hex of sha256 over the
  template without the label and the constraint) and selects on it - the
  same effect on every cluster.

Constants: `fv1.FunctionSpreadAnnotation`, `fv1.FunctionSpreadStrictAnnotation`.

## Test

```bash
go test -race -run 'TestSpread|TestApplySpread|TestNewDeployPodSpecSpread|TestPodTemplateChanged' ./pkg/executor/executortype/newdeploy/
go test -race -run TestSpreadAnnotationsPredicate ./pkg/executor/funcreconciler/
```

## Debug

```bash
kubectl get fn <name> -n <ns> -o jsonpath='{.metadata.annotations}'
kubectl get deploy -n <ns> -l functionName=<name> -o jsonpath='{.items[0].spec.template.spec.topologySpreadConstraints}'
kubectl get pods -n <ns> -l functionName=<name> -o wide      # NODE column
```

No constraint although annotated: the executor is older than this feature,
or the function is not newdeploy (poolmgr / container ignore it).
