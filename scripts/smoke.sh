#!/bin/sh
# Joins the console server list many times with the fake console and fails if
# any join doesn't reach the menu and the transfer. Used by CI; run it locally
# with `sh scripts/smoke.sh` after `make build fakeconsole`.
set -eu
RUNS=${RUNS:-40}
BIN=${BIN:-./blockparty}
FAKE=${FAKE:-./fakeconsole}
work=$(mktemp -d)
pid=
cleanup() {
  if [ -n "$pid" ]; then kill "$pid" 2>/dev/null || true; wait "$pid" 2>/dev/null || true; fi
  rm -rf "$work"
}
trap cleanup EXIT

cp deploy/servers.example.json "$work/servers.json"
LIST_IP=127.0.0.1 AUTH_OFF=true DNS_ENABLED=false CONNECTION=both SIGNALING=auto \
EXTRA_SIGNALING_PORTS=none SERVERS_FILE="$work/servers.json" DATA_DIR="$work" \
  "$BIN" >"$work/server.log" 2>&1 &
pid=$!

# Wait for the listeners before the first join.
tries=0
until grep -q "NetherNet listener ready" "$work/server.log"; do
  tries=$((tries + 1))
  if [ $tries -gt 60 ] || ! kill -0 "$pid" 2>/dev/null; then
    echo "server list didn't start:"
    cat "$work/server.log"
    exit 1
  fi
  sleep 0.5
done

failed=0
for mode in raknet http https; do
  ok=0
  retried=0
  i=0
  while [ $i -lt "$RUNS" ]; do
    i=$((i + 1))
    if "$FAKE" -mode "$mode" -pick 1 2>&1 | grep -q PASS; then
      ok=$((ok + 1))
    # A real console tries again when its first packets get lost (it happens
    # now and then on a busy CI machine); a join that fails twice is a bug.
    elif "$FAKE" -mode "$mode" -pick 1 2>&1 | grep -q PASS; then
      ok=$((ok + 1))
      retried=$((retried + 1))
    fi
  done
  if [ $retried -gt 0 ]; then
    echo "$mode: $ok of $RUNS joins passed ($retried needed a second try)"
  else
    echo "$mode: $ok of $RUNS joins passed"
  fi
  [ "$ok" -eq "$RUNS" ] || failed=1
done

# Connect by address, check the saved server shows up, then remove it.
steps_ok=1
"$FAKE" -press Connect -connect play.example.com -port 19200 -name "Smoke Test" 2>&1 | grep -q "PASS: transferred to play.example.com port 19200" || steps_ok=0
"$FAKE" -expect "Smoke Test" -press "Smoke Test" 2>&1 | grep -q "PASS: transferred to play.example.com port 19200" || steps_ok=0
"$FAKE" -press Remove -remove "Smoke Test" -expect "Smoke Test" 2>&1 | grep -q "PASS" || steps_ok=0
if [ $steps_ok -eq 1 ]; then echo "player servers: connect, save and remove passed"; else echo "player servers: FAILED"; failed=1; fi

# A server built with -race reports data races in its log.
if grep -q "DATA RACE" "$work/server.log"; then
  echo "data race found"
  failed=1
fi

if [ $failed -ne 0 ]; then
  echo "--- server log ---"
  cat "$work/server.log"
  exit 1
fi
