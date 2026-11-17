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
`topologySpreadConstraint` to the Deployment's pod template, after every
pod spec merge (the function's choice replaces an environment constraint on
the same key; others are kept):

```yaml
maxSkew: 1
topologyKey: kubernetes.io/hostname
whenUnsatisfiable: ScheduleAnyway      # DoNotSchedule when strict
labelSelector: { matchLabels: { functionUid: <uid> } }
matchLabelKeys: [pod-template-hash]    # a rollout does not count the old pods
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
- Adopting an orphan Deployment carries `TopologySpreadConstraints`.

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
