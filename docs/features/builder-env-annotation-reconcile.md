# builder-env-annotation-reconcile

**Status:** Active (2026-10-20, `release/custom-v3.0`) · not yet in a published image.

## The bug

buildermgr's environment controller was registered with
`GenerationChangedPredicate` only, while its builder Service/Deployment are
named `<env.Name>-<env.ResourceVersion>` and the package reconciler waits for
exactly that name. An annotation-only patch of an Environment (Lambforge's
graphql-server-lambdas used `fission.io/force-update: <ts>` when a suspended
project was resumed) bumps `resourceVersion` without bumping `generation`:
the executor rebuilt the warm pools, the environment controller never saw
the change, no builder named after the new resourceVersion existed, and
every build of that environment stayed `pending` with
`builder deployment not created yet, will retry` every 5 s - until buildermgr
restarted (its startup reconcile creates the builder).

Production (`fission-bundle:v1.24.1-watch-all-namespaces`) carries the bug;
it had never been triggered there (no environment has the annotation).

## The fix

`pkg/buildermgr/buildermgr.go` registers the environment controller with
`environmentPredicate()` (`environment_reconciler.go`):
`GenerationChangedPredicate OR AnnotationChangedPredicate OR LabelChangedPredicate`
- spec, annotation and label changes reconcile; status-only writes (the
`/status` subresource) are still dropped. Finalizer / ownerReference-only
writes still are not - the real fix is naming the builder after the
generation, a follow-up.
`environment_predicate_test.go` covers the four cases.

## How to test

```bash
go test -race ./pkg/buildermgr/ -run TestEnvironmentPredicate
```

Live: `kubectl annotate environment <env> -n <ns> fission.io/force-update=$(date +%s) --overwrite`,
then `kubectl get deploy -n <ns> -l owner=buildermgr` shows a deployment named
after the new resourceVersion within seconds; a package build of that
environment leaves `pending`.

## How to debug

`builder deployment not created yet, will retry` in buildermgr logs with a
`<env>-<rv>` that does not exist -> the environment's `resourceVersion`
changed without a generation change; restarting buildermgr is the
workaround on an image without this fix.
