#!/bin/bash
# SPDX-License-Identifier: Apache-2.0
# Vendors the Corroborate SDK (MIT, https://github.com/Hugo0/poh-aggregator)
# into vendor/corroborate-sdk as compiled JavaScript, so the sidecar needs no
# build step and only one runtime dependency (viem, pinned in package.json).
#
#   scripts/vendor-sdk.sh /path/to/poh-aggregator
#
# Vendor from a commit pushed to GitHub main (a checkout, or the extracted
# tarball of `gh api repos/Hugo0/poh-aggregator/tarball/SHA` with COMMIT=SHA).
# typescript and viem come from NODE_MODULES (default: the checkout's own
# node_modules). The commit vendored is written to SOURCE.json.
#
# No RPC URL carrying a key in its path is vendored: the Galxe adapter's
# keyed BNB archive endpoint is replaced by CORROBORATE_BNB_RPC_URL, default
# a keyless public BNB endpoint, and the script fails if any keyed URL is
# left (test/sidecar.test.mjs checks the same).
set -euo pipefail
src=$(cd "${1:?usage: vendor-sdk.sh /path/to/poh-aggregator}" && pwd)
here=$(cd "$(dirname "$0")/.." && pwd)
out="$here/vendor/corroborate-sdk"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

mkdir -p "$work/packages/sdk"
cp -r "$src/ontology" "$work/ontology"
cp -r "$src/packages/sdk/src" "$work/packages/sdk/src"
find "$work/packages/sdk/src" -name '*.test.ts' -delete
cp "$src/ontology/adapters.json" "$work/packages/sdk/src/ontology-data.json"
sed 's/"declaration": true/"declaration": false/' "$src/packages/sdk/tsconfig.json" > "$work/packages/sdk/tsconfig.json"
modules=${NODE_MODULES:-$src/node_modules}
ln -s "$modules" "$work/packages/sdk/node_modules"
(cd "$work/packages/sdk" && node node_modules/typescript/bin/tsc -p tsconfig.json)

rm -rf "$out"
mkdir -p "$out"
cp -r "$work/packages/sdk/dist/." "$out/"
cp "$work/packages/sdk/src/ontology-data.json" "$out/ontology-data.json"
cp "$src/ontology/enrollment.json" "$out/enrollment.json"
# enroll.js reads the enrollment table from the monorepo root; vendored, it
# sits beside it.
sed -i "s#'../../../ontology/enrollment.json'#'./enrollment.json'#" "$out/enroll.js"
cp "$src/LICENSE" "$out/LICENSE"
# The keyed BNB archive RPC becomes an operator setting with a keyless default.
# Without an archive endpoint only Galxe's issuance date is lost, never held.
sed -i -E "s#'https://bsc-mainnet\.nodereal\.io/v1/[0-9a-fA-F]+'#(globalThis.process?.env?.CORROBORATE_BNB_RPC_URL || 'https://bsc-dataseed.bnbchain.org')#" "$out/adapters/galxe.js"
if grep -rEn "nodereal\.io|/v[0-9]+/[0-9a-fA-F]{32}" "$out"; then
  echo "refusing: an RPC URL with a key in its path is still vendored" >&2
  exit 1
fi
# The commit: COMMIT when given, else the checkout's HEAD as git records it.
commit=${COMMIT:-}
if [ -z "$commit" ]; then
  head=$(cat "$src/.git/HEAD")
  case "$head" in
    ref:*) ref=${head#ref: }
      commit=$(cat "$src/.git/$ref" 2>/dev/null || grep " $ref\$" "$src/.git/packed-refs" | cut -d' ' -f1) ;;
    *) commit=$head ;;
  esac
fi
cat > "$out/SOURCE.json" <<EOF
{
  "name": "@corroborate/sdk",
  "repository": "https://github.com/Hugo0/poh-aggregator",
  "commit": "$commit",
  "license": "MIT",
  "built_with": "typescript $(node -p "require('$modules/typescript/package.json').version")",
  "viem": "$(node -p "require('$modules/viem/package.json').version")",
  "patched": "adapters/galxe.js: the keyed BNB archive RPC replaced by CORROBORATE_BNB_RPC_URL (default https://bsc-dataseed.bnbchain.org); enroll.js: enrollment.json read from beside it"
}
EOF
echo "vendored $commit into $out"
