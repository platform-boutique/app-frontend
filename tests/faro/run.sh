#!/usr/bin/env bash
# Real-browser check of the Faro RUM wiring against a throwaway frontend container.
#
#   tests/faro/run.sh            (from anywhere, with the compose stack running)
#
# What it does: builds the image (frontend:faro-test), starts the container `faro-test-fe` on the compose network
# with the SAME environment as the compose frontend (read with `docker inspect`, nothing in compose is changed),
# published on 127.0.0.1:8091, plus a throwaway OTLP sink `faro-test-sink` (tests/faro/otlpsink), and drives
# it with Playwright (faro-check.mjs, fake collector on the host, port 8092). Passes:
#   1. full          Faro on + server telemetry on (sent to the sink)
#   2. no-telemetry  Faro on, telemetry off (exactly as compose has it)
#   3. faro-off      FARO_COLLECTOR_URL unset
#   4. a bad FARO_COLLECTOR_URL stops the process
# The containers are removed at the end, also on failure. It places ONE real order in the running shop (pass 1 only)
# (PLACE_ORDER=0 skips that). Needs: docker, Go (GOTOOLCHAIN=auto), Node, and Playwright with Chromium
# (PLAYWRIGHT_DIR = the directory whose node_modules has `playwright`; default: ../../local relative to the repo,
# the layout of the platform-demo checkout).
set -euo pipefail
cd "$(dirname "$0")/../.."
REPO="$PWD"
SRC_CONTAINER="${SRC_CONTAINER:-shop-local-frontend-1}"
NETWORK="${NETWORK:-shop-local_default}"
FE=faro-test-fe
SINK=faro-test-sink
COLLECTOR_PORT="${COLLECTOR_PORT:-8092}"
WORK="$(mktemp -d)"
export PLAYWRIGHT_DIR="${PLAYWRIGHT_DIR:-/home/stefanm/Desktop/platform-demo/local}"
cleanup() { docker rm -f "$FE" "$SINK" >/dev/null 2>&1 || true; rm -rf "$WORK"; }
trap cleanup EXIT

echo "== build image frontend:faro-test"
docker build -q -t frontend:faro-test . >/dev/null

echo "== build the OTLP sink"
CGO_ENABLED=0 GOOS=linux GOTOOLCHAIN=auto go build -o "$WORK/otlpsink" ./tests/faro/otlpsink
chmod 755 "$WORK" "$WORK/otlpsink"

# the compose frontend's environment (read only)
mapfile -t BASE_ENV < <(docker inspect "$SRC_CONTAINER" --format '{{range .Config.Env}}{{println .}}{{end}}' | grep -E '^[A-Z][A-Z0-9_]*=' | grep -vE '^(PATH|HOME|HOSTNAME)=')

start_fe() {  # start_fe [-e KEY=VALUE]...   (an extra variable replaces the base one of the same name)
  docker rm -f "$FE" >/dev/null 2>&1 || true
  local args=() skip=" "
  for e in "$@"; do [ "$e" = -e ] || skip+="${e%%=*} "; done
  for e in "${BASE_ENV[@]}"; do case "$skip" in *" ${e%%=*} "*) ;; *) args+=(-e "$e") ;; esac; done
  docker run -d --name "$FE" --network "$NETWORK" --memory 128m -p 127.0.0.1:8091:8080 "${args[@]}" "$@" frontend:faro-test >/dev/null
  for _ in $(seq 1 40); do
    curl -fsS -o /dev/null http://127.0.0.1:8091/_healthz 2>/dev/null && return 0
    sleep 1
  done
  docker logs "$FE" | tail -20; echo "frontend did not answer on 127.0.0.1:8091" >&2; return 1
}

run_check() {  # run_check <MODE>
  env -u ELECTRON_RUN_AS_NODE MODE="$1" BASE_URL=http://localhost:8091 COLLECTOR_PORT="$COLLECTOR_PORT" SINK_CONTAINER="$SINK" \
    node tests/faro/faro-check.mjs
}

rc=0
COLLECTOR_URL="http://localhost:${COLLECTOR_PORT}/collect"   # the BROWSER (on the host) posts here

echo; echo "== pass 1: Faro on, server telemetry on"
docker run -d --name "$SINK" --network "$NETWORK" --memory 64m -v "$WORK:/s:ro" alpine:3.24 /s/otlpsink >/dev/null
start_fe -e "FARO_COLLECTOR_URL=$COLLECTOR_URL" -e "OTEL_EXPORTER_OTLP_ENDPOINT=http://$SINK:4317" -e ENABLE_TRACING=1 && { run_check full || rc=1; } || rc=1

echo; echo "== pass 2: Faro on, server telemetry off (compose environment unchanged)"
start_fe -e "FARO_COLLECTOR_URL=$COLLECTOR_URL" && { PLACE_ORDER=0 run_check no-telemetry || rc=1; } || rc=1

echo; echo "== pass 3: Faro off"
start_fe && { PLACE_ORDER=0 run_check faro-off || rc=1; } || rc=1

echo; echo "== startup validation: a bad FARO_COLLECTOR_URL must stop the process"
docker rm -f "$FE" >/dev/null 2>&1 || true
bad=(); for e in "${BASE_ENV[@]}"; do bad+=(-e "$e"); done
out=$(docker run --rm --name "$FE" --network "$NETWORK" --memory 128m "${bad[@]}" -e FARO_COLLECTOR_URL=ftp://alloy/collect frontend:faro-test 2>&1 || true)
if grep -q 'FARO_COLLECTOR_URL .* must be an http' <<<"$out"; then echo "PASS  bad collector URL refused: $(grep -o 'FARO_COLLECTOR_URL[^}]*' <<<"$out" | head -1)"; else echo "FAIL  bad collector URL: $out"; rc=1; fi

exit $rc
