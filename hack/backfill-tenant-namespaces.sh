#!/bin/bash
# SPDX-FileCopyrightText: The Fission Authors
#
# SPDX-License-Identifier: Apache-2.0

# Backfills tenancy.mode=dynamic onboarding for every namespace that already
# has Fission resources in it, ahead of migrating an existing cluster onto
# release/custom-v3.0. See docs/features/watch-all-namespaces.md.
#
# tenancy.mode=dynamic requires each tenant namespace to be explicitly
# onboarded (the fission.io/enabled=true label materializes a FissionTenant
# CR, which the tenant controller then provisions per-namespace RBAC for).
# The old watch-all-namespaces mechanism picked up every namespace
# implicitly, so a cluster migrating from it has tenants with no explicit
# opt-in yet. This script finds them by an objective criterion -- actually
# having a Fission resource -- rather than blanket-labelling every namespace
# in the cluster (which would needlessly onboard unrelated namespaces, e.g.
# other operators' install namespaces, system namespaces, leftover test
# namespaces with nothing Fission-related in them).
#
# Usage:
#   hack/backfill-tenant-namespaces.sh            # dry run (default): lists
#                                                  # the namespaces that would
#                                                  # be labelled, changes nothing
#   hack/backfill-tenant-namespaces.sh --apply     # actually applies the label
#
# Safe to re-run: labelling is idempotent (--overwrite).

set -euo pipefail

APPLY=false
for arg in "$@"; do
  case "$arg" in
    --apply) APPLY=true ;;
    -h|--help)
      echo "Usage: $0 [--apply]"
      echo "  (no args)  dry run: print the namespaces that would be labelled"
      echo "  --apply    actually label them fission.io/enabled=true"
      exit 0
      ;;
    *)
      echo "unknown argument: $arg" >&2
      exit 1
      ;;
  esac
done

CURRENT_CONTEXT=$(kubectl config current-context)
echo "kubectl context: ${CURRENT_CONTEXT}"
if [ "$APPLY" = true ]; then
  read -r -p "About to label namespaces on this cluster. Type the context name to confirm: " confirm
  if [ "$confirm" != "$CURRENT_CONTEXT" ]; then
    echo "Context name did not match. Aborting, nothing changed."
    exit 1
  fi
fi

FISSION_RESOURCES=(functions environments packages httptriggers timetriggers kuberneteswatchtriggers messagequeuetriggers canaryconfigs)

echo "Scanning for namespaces with Fission resources..."
NAMESPACES=$(
  for res in "${FISSION_RESOURCES[@]}"; do
    kubectl get "${res}.fission.io" -A --no-headers 2>/dev/null | awk '{print $1}'
  done | sort -u
)

if [ -z "$NAMESPACES" ]; then
  echo "No namespaces with Fission resources found. Nothing to do."
  exit 0
fi

echo "Found $(echo "$NAMESPACES" | wc -l | tr -d ' ') namespace(s) with Fission resources:"
echo "$NAMESPACES" | sed 's/^/  - /'

# Namespaces already labelled don't need re-labelling, but --overwrite makes
# re-running harmless either way; this is just for a clearer dry-run report.
ALREADY_LABELLED=$(kubectl get ns -l fission.io/enabled=true --no-headers 2>/dev/null | awk '{print $1}' || true)
TO_LABEL=$(comm -23 <(echo "$NAMESPACES") <(echo "$ALREADY_LABELLED" | sort -u))

if [ -z "$TO_LABEL" ]; then
  echo "All of the above are already labelled fission.io/enabled=true. Nothing to do."
  exit 0
fi

echo
echo "Namespaces needing the fission.io/enabled=true label:"
echo "$TO_LABEL" | sed 's/^/  - /'

if [ "$APPLY" != true ]; then
  echo
  echo "Dry run only -- no changes made. Re-run with --apply to label these namespaces."
  exit 0
fi

echo
echo "Labelling..."
FAILED=()
while read -r ns; do
  [ -z "$ns" ] && continue
  if kubectl label namespace "$ns" fission.io/enabled=true --overwrite; then
    :
  else
    FAILED+=("$ns")
  fi
done <<< "$TO_LABEL"

if [ "${#FAILED[@]}" -gt 0 ]; then
  echo "Failed to label: ${FAILED[*]}" >&2
  exit 1
fi

echo
echo "Done. Verify onboarding:"
echo "  kubectl get fissiontenants.fission.io"
echo "Every namespace above should appear with READY=True within a few seconds."
echo "If the tenant controller was already running and any entries are missing or not"
echo "Ready, see the 'tenant controller does not self-heal' note in"
echo "docs/features/watch-all-namespaces.md (you may need to restart it: "
echo "  kubectl delete pod -n fission -l svc=tenantcontroller )."
