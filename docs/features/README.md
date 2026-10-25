# Fork features

Custom features and fixes for this Fission fork (`ghcr.io/stradivario` builds), tracked here rather than in the auto-generated `CHANGELOG.md`.

| Feature | Status |
|---|---|
| [watch-all-namespaces](watch-all-namespaces.md) | Superseded by upstream's `tenancy.mode=dynamic` on `release/custom-v3.0` |
| [internal-invocation-isolation](internal-invocation-isolation.md) | Active — re-ported onto `release/custom-v3.0`'s router (RFC-0002/0013 rewrite); kept alongside upstream's tenancy work, which does not cover this threat model |
| [internal-auth](internal-auth.md) | Active — `internalAuth.enabled` defaults off (upstream defaults on) |
| [build-deploy-webhooks](build-deploy-webhooks.md) | Active — re-ported onto `release/custom-v3.0` |
| [builder-env-annotation-reconcile](builder-env-annotation-reconcile.md) | Active — buildermgr reconciles annotation- and label-only Environment changes (builder objects are named after resourceVersion); not in a published image yet |
| [tenant-secret-configmap-watch](tenant-secret-configmap-watch.md) | Active — dynamic tenancy: Secret/ConfigMap changes in runtime-onboarded namespaces recycle function pods again; also fixes newdeploy resource overrides mutating the cached Environment; not in a published image yet |
| [adopt-tenant-sync](adopt-tenant-sync.md) | Active — executor restarts no longer delete tenant function Deployments / leak specialized pods (adopt waited on a partial tenant set); not in a published image yet |
