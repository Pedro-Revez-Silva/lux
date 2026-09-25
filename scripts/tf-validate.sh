#!/usr/bin/env bash
# Checks every Terraform root under deploy/terraform: the modules
# (deploy/terraform/<module>) and their examples (deploy/terraform/examples/
# <name>, deploy/terraform/<module>/examples/<name>). For each: fmt -check,
# init without a backend against the committed lock file, validate, and
# `terraform test` where the root has tests (mocked providers: no
# credentials). TERRAFORM names the binary (default: terraform on PATH).
set -euo pipefail

tf=${TERRAFORM:-terraform}
cd "$(dirname "$0")/../deploy/terraform"

"$tf" fmt -check -recursive -diff

roots=()
for d in */ examples/*/ */examples/*/; do
  d=${d%/}
  [ -d "$d" ] && [ "$d" != examples ] || continue
  compgen -G "$d/*.tf" >/dev/null || continue
  roots+=("$d")
done

for d in "${roots[@]}"; do
  echo "== $d"
  "$tf" -chdir="$d" init -backend=false -input=false -lockfile=readonly -no-color >/dev/null
  "$tf" -chdir="$d" validate -no-color
  if compgen -G "$d/tests/*.tftest.hcl" >/dev/null; then
    "$tf" -chdir="$d" test -no-color
  fi
done
