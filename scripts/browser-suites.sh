#!/usr/bin/env bash
# The browser suites (internal/web/*.cjs), each against a disposable loopback
# server. The one runner for them: public CI (.github/workflows/test.yml) and
# the operator's gate call it.
#
#   SWARMMEMO_TEST_BINARY=/path/to/swarmmemo scripts/browser-suites.sh [SUITE.cjs ...]
#
# Environment:
#   SWARMMEMO_TEST_BINARY  the built server (required)
#   PLAYWRIGHT_MODULE      Playwright's module path (default: require('playwright'))
#   CHROMIUM_PATH          a Chromium binary (default: Playwright's own)
#   NODE                   node (default: node on PATH)
#   BROWSER_SUITES_DIR     where logs and fixture data go (default: a fresh temp dir)
#
# references_test.cjs is left out: the references harness runs it. Suites in
# READ_SUITES need the inbox entry log read, so they get a second server with
# INBOX_ENTRIES=read. Prints one PASS or FAIL line a suite and exits 1 when
# any failed. Local fixtures only: it never points at a deployed origin.
set -uo pipefail
cd "$(dirname "$0")/.."
BIN=${SWARMMEMO_TEST_BINARY:?set SWARMMEMO_TEST_BINARY to a built swarmmemo}
NODE=${NODE:-node}
S=${BROWSER_SUITES_DIR:-$(mktemp -d "${TMPDIR:-/tmp}/swarmmemo-browser.XXXXXX")}
mkdir -p "$S" || exit 1
READ_SUITES="inbox_waiting_test.cjs"

free_port() { python3 -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1]);s.close()'; }

echo '{"anonymous_top_level_per_hour":1000000,"repeat_flood_copies":1000000,"new_key_posts_per_hour":1000000}' > "$S/posting.json"
SRVS=""
trap 'kill $SRVS 2>/dev/null' EXIT
# start_candidate DATA PORT [ENV=VALUE...]: one fixture server on a fresh data
# dir with generous posting params.
start_candidate() {
  local data=$1 port=$2; shift 2
  env "$@" NETWORK_RATE=100000/10000 GRAPH_UNIVERSE_TTL=60s DATA_DIR="$data" SERVICE_ID=swarmmemo.com ALLOW_INSECURE_LOCAL=true LISTEN_ADDR=127.0.0.1:$port "$BIN" > "$data.server.log" 2>&1 &
  SRVS="$SRVS $!"
  for _ in $(seq 1 40); do curl -sf "http://127.0.0.1:$port/api/messages?limit=1" >/dev/null && break; sleep 0.25; done
  DATA_DIR=$data SERVICE_ID=swarmmemo.com "$BIN" params set posting "$S/posting.json" --reason "browser suite fixture" >/dev/null 2>&1 || echo "posting params not set ($port)"
}

PASS=0; FAIL=0; FAILED=""
# run_suite FILE PORT DATA: one suite, one PASS/FAIL line.
run_suite() {
  local t=$1 port=$2 data=$3 name out rc
  name=$(basename "$t")
  out=$(PLAYWRIGHT_MODULE=${PLAYWRIGHT_MODULE:-} CHROMIUM_PATH=${CHROMIUM_PATH:-} \
    SWARMMEMO_TEST_URL="http://127.0.0.1:$port" SWARMMEMO_TEST_DATA="$data" \
    SWARMMEMO_TEST_BINARY="$BIN" "$NODE" "$t" 2>&1)
  rc=$?; echo "$out" > "$S/$name.log"
  if [ $rc -eq 0 ]; then PASS=$((PASS+1)); echo "  PASS $name"
  else FAIL=$((FAIL+1)); FAILED="$FAILED $name"; echo "  FAIL $name"; echo "$out" | tail -15 | sed 's/^/      /'; fi
}

# The suites to run: the named ones, or every one.
if [ $# -gt 0 ]; then SUITES=("$@"); else SUITES=(); for t in internal/web/*.cjs; do SUITES+=("$(basename "$t")"); done; fi
main=(); read=()
for name in "${SUITES[@]}"; do
  [ "$name" = "references_test.cjs" ] && continue
  case " $READ_SUITES " in *" $name "*) read+=("$name") ;; *) main+=("$name") ;; esac
done

if [ ${#main[@]} -gt 0 ]; then
  DATA=$(mktemp -d "$S/data.XXXX") && PORT=$(free_port) || exit 1
  start_candidate "$DATA" "$PORT"
  echo "== browser suites (port $PORT) =="
  for name in "${main[@]}"; do run_suite "internal/web/$name" "$PORT" "$DATA"; done
fi
if [ ${#read[@]} -gt 0 ]; then
  RDATA=$(mktemp -d "$S/data-read.XXXX") && RPORT=$(free_port) || exit 1
  start_candidate "$RDATA" "$RPORT" INBOX_ENTRIES=read
  echo "== browser suites, INBOX_ENTRIES=read (port $RPORT) =="
  for name in "${read[@]}"; do run_suite "internal/web/$name" "$RPORT" "$RDATA"; done
fi
echo "== browser suites: $PASS passed, $FAIL failed ==$FAILED"
[ "$FAIL" -eq 0 ]
