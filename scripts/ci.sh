#!/usr/bin/env bash
# The public CI's jobs, one list: .github/workflows/test.yml runs them, and so
# can anyone from a checkout.
#
#   bash scripts/ci.sh race   # Go race detector, with -short
#   bash scripts/ci.sh test   # Go tests, vet and builds; the Python, JavaScript and MCP suites
#
# Needs go, python3, node 22+, npm and uv on PATH. Runs every step, prints one
# PASS or FAIL line a step with its seconds, and exits 1 when any failed. Local
# fixtures only: it never points at a deployed origin.
#
# SWARMMEMO_GATE=1 is set only by the operator's release gate, which has already
# run the same plain go test ./... (same packages, same 20m bound) on the source
# tree this snapshot is cut from: "test" then skips that one step. Everything
# else, the race job included, still runs every package here.
set -uo pipefail
cd "$(dirname "$0")/.."
export PYTHONDONTWRITEBYTECODE=1
JOB=${1:?usage: scripts/ci.sh race|test}
T=$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/swarmmemo-ci.XXXXXX") || exit 1
trap 'rm -rf "$T"' EXIT
BIN=$T/swarmmemo-test
FAILED=""

# step NAME COMMAND...: run one step, its output streamed, and note a failure.
step() {
  local name=$1 t0=$SECONDS
  shift
  echo "== $name =="
  if "$@"; then
    echo "  PASS $name ($((SECONDS - t0))s)"
  else
    echo "  FAIL $name ($((SECONDS - t0))s)"
    FAILED="$FAILED $name"
  fi
}

node_suites() {
  export SWARMMEMO_TEST_BINARY=$BIN SWARMMEMO_DELEGATION_TEST_BINARY=$BIN
  node -e 'if (Number(process.versions.node.split(".")[0]) < 22) throw Error("Node 22+ required")' &&
    node --test clients/javascript/test_client.mjs &&
    node --experimental-strip-types --test workers/runs-loader/test/loader.test.mjs &&
    npm ci --ignore-scripts --no-audit --no-fund --prefix workers/corroborate &&
    node --test workers/corroborate/test/sidecar.test.mjs
}

case $JOB in
race)
  # The race detector makes the pure-Go SQLite some thirty times slower, so
  # this runs with -short: the large-fixture tests (every operation's
  # authority matrix, floods of tens of thousands of rows, scale timings)
  # skip here and run in full, with their unscaled time bounds, in "test".
  # internal/board alone takes ~29 min under -race (m848 timed out at 30m).
  step "go test -race -short" go test -race -short -timeout 50m ./...
  ;;
test)
  if [ "${SWARMMEMO_GATE:-}" = 1 ]; then
    echo "== go test =="
    echo "  SKIP go test (SWARMMEMO_GATE=1: the gate ran it on the source tree)"
  else
    step "go test" go test -timeout 20m ./...
  fi
  step "go vet" go vet ./...
  step "go build" go build -trimpath -o "$BIN" ./cmd/swarmmemo
  step "curation" env SWARMMEMO_TEST_BINARY="$BIN" \
    python3 -B -W error::ResourceWarning -m unittest discover -s curation -p 'test_*.py' -v
  step "javascript" node_suites
  step "scripts" env SWARMMEMO_TEST_BINARY="$BIN" SWARMMEMO_DELEGATION_TEST_BINARY="$BIN" \
    SWARMMEMO_PRIVATE_INBOX_TEST_BINARY="$BIN" SWARMMEMO_PRIVATE_READ_TEST_BINARY="$BIN" \
    uv run --project scripts --locked python -m unittest discover -s scripts -p 'test_*.py' -v
  step "trust" python3 -B -W error::ResourceWarning -m unittest discover -s scripts/trust -p 'test_*.py' -v
  step "python client" env SWARMMEMO_TEST_BINARY="$BIN" \
    uv run --project scripts --locked python -B -W error::ResourceWarning -m unittest discover -s clients/python -p 'test_*.py' -v
  step "mcp" env SWARMMEMO_MCP_TEST_BINARY="$BIN" \
    uv run --project clients/mcp --locked python -B -W error::ResourceWarning -m unittest discover -s clients/mcp -p 'test_*.py' -v
  step "arm64 build" env GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -o "$T/swarmmemo-arm64" ./cmd/swarmmemo
  ;;
*)
  echo "usage: scripts/ci.sh race|test" >&2
  exit 2
  ;;
esac
if [ -n "$FAILED" ]; then
  echo "ci $JOB FAILED:$FAILED"
  exit 1
fi
echo "ci $JOB passed"
