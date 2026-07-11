# Fork features

Custom features and fixes for this Fission fork (`ghcr.io/stradivario` builds), tracked here rather than in the auto-generated `CHANGELOG.md`.

| Feature | Status |
|---|---|
| [watch-all-namespaces](watch-all-namespaces.md) | Superseded by upstream's `tenancy.mode=dynamic` on `release/custom-v3.0` |
| [internal-invocation-isolation](internal-invocation-isolation.md) | Active — re-ported onto `release/custom-v3.0`'s router (RFC-0002/0013 rewrite); kept alongside upstream's tenancy work, which does not cover this threat model |
