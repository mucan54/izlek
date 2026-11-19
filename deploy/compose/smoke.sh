#!/usr/bin/env bash
# Smoke test of a Compose deployment: build the images, start the stack, send events, see them
# in a query, check container health, the UI and the flag endpoint, stop ingestion cleanly.
# Everything (volumes included) is removed afterwards.
#
#   deploy/compose/smoke.sh                 # deploy/compose/docker-compose.yml
#   deploy/compose/smoke.sh coolify         # docker-compose.coolify.yml, the way Coolify would run it
#   KEEP=1 deploy/compose/smoke.sh          # leave it running for debugging
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"
MODE="${1:-compose}"

export COMPOSE_PROJECT_NAME="${COMPOSE_PROJECT_NAME:-izlek-smoke-$MODE}"
export LA_COOKIE_SECURE=false
rand() { head -c "$1" /dev/urandom | od -An -tx1 | tr -d ' \n'; }

case "$MODE" in
  compose)
    cd "$HERE"
    export COMPOSE_FILE="$HERE/docker-compose.yml"
    export COMPOSE_PROFILES=canary
    export LA_QUERY_INTERNAL_SECRET="${LA_QUERY_INTERNAL_SECRET:-$(rand 32)}"
    ;;
  coolify)
    # Coolify fills the SERVICE_* variables and routes domains to the services; here the
    # variables are set by hand and an override publishes the two public ports instead.
    cd "$ROOT"
    # exclude_from_hc is a Coolify extension: Coolify removes it before handing the file to
    # Docker, and plain `docker compose` rejects it. Do the same here.
    grep -v '^ *exclude_from_hc:' "$ROOT/docker-compose.coolify.yml" > "$ROOT/.coolify-smoke.yml"
    export COMPOSE_FILE="$ROOT/.coolify-smoke.yml:$ROOT/deploy/coolify/ci.override.yml"
    export ADMIN_EMAIL=smoke@example.com
    export SERVICE_PASSWORD_ADMIN="$(rand 16)"
    export SERVICE_PASSWORD_64_QUERYSECRET="$(rand 32)"
    export SERVICE_URL_CONTROL_8090=http://127.0.0.1:8090
    export SERVICE_URL_INGEST_8080=http://127.0.0.1:8080
    export LA_TRUSTED_PROXIES=0
    ;;
  *) echo "usage: $0 [compose|coolify]"; exit 2 ;;
esac
dc() { docker compose "$@"; }

cleanup() {
  code=$?
  if [ "$code" -ne 0 ]; then
    echo "== FAILED (exit $code)"
    dc ps -a || true
    dc logs --no-color --tail 40 || true
  fi
  [ -n "${KEEP:-}" ] || dc down -v --remove-orphans > /dev/null 2>&1 || true
  rm -f "$ROOT/.coolify-smoke.yml"
  exit "$code"
}
trap cleanup EXIT

echo "== build ($MODE)"
dc build

if [ "$MODE" = compose ]; then
  echo "== bootstrap"
  boot="$(dc run --rm -T control bootstrap --email smoke@example.com --password 'smoke-password-1' --org Smoke --project Smoke --json | tail -n 1)"
  echo "== up"
  dc up -d init ingest shipper compactor query control
else
  echo "== up (the stack bootstraps itself)"
  dc up -d
  # The bootstrap service prints key=value lines once, on the run that created the key.
  boot="$(dc logs --no-color --no-log-prefix bootstrap | python3 -c "
import sys, json
kv = dict(l.strip().split('=', 1) for l in sys.stdin if '=' in l and l.split('=', 1)[0] in ('project_id', 'token', 'secret'))
print(json.dumps(kv))")"
fi
field() { printf '%s' "$boot" | python3 -c "import sys, json; print(json.load(sys.stdin)['$1'])"; }
TOKEN="$(field token)"; SECRET="$(field secret)"; PID="$(field project_id)"

wait_http() { # name, url
  for _ in $(seq 1 90); do
    if [ "$(curl -s -o /dev/null -w '%{http_code}' "$2" || true)" = 200 ]; then echo "   $1 ready"; return 0; fi
    sleep 1
  done
  echo "   $1 did not become ready: $2"; return 1
}
wait_http ingest http://127.0.0.1:8080/readyz
wait_http control http://127.0.0.1:8090/readyz

echo "== send and query (through the control plane's query proxy)"
code="$(curl -s -o /dev/null -w '%{http_code}' -H 'Content-Type: application/json' http://127.0.0.1:8080/batch/ \
  -d "{\"api_key\":\"$TOKEN\",\"batch\":[{\"event\":\"smoke\",\"distinct_id\":\"smoke-1\"},{\"event\":\"smoke\",\"distinct_id\":\"smoke-2\"}]}")"
[ "$code" = 200 ] || { echo "   ingestion answered $code"; exit 1; }
from="$(date -u -d yesterday +%F)"; to="$(date -u -d tomorrow +%F)"
seen=0
for _ in $(seq 1 90); do
  seen="$(curl -s -H "Authorization: Bearer $SECRET" -H 'Content-Type: application/json' \
    "http://127.0.0.1:8090/api/projects/$PID/q/sql" \
    -d "{\"from\":\"$from\",\"to\":\"$to\",\"sql\":\"SELECT count(*) FROM events WHERE event = 'smoke'\"}" \
    | python3 -c "import sys, json; print(json.load(sys.stdin)['rows'][0][0])" 2> /dev/null || echo 0)"
  [ "$seen" = 2 ] && break
  sleep 2
done
[ "$seen" = 2 ] || { echo "   expected 2 events in the query service, saw $seen"; exit 1; }
echo "   2 events visible"

if [ "$MODE" = compose ]; then
  echo "== canary"
  dc run --rm -T -e LA_CANARY_TOKEN="$TOKEN" -e LA_CANARY_QUERY_KEY="$SECRET" canary -once -batches 3 -timeout 3m -poll 2s
else
  echo "== sign in with the bootstrapped admin"
  code="$(curl -s -o /dev/null -w '%{http_code}' -H 'Content-Type: application/json' http://127.0.0.1:8090/api/auth/login \
    -d "{\"email\":\"$ADMIN_EMAIL\",\"password\":\"$SERVICE_PASSWORD_ADMIN\"}")"
  [ "$code" = 200 ] || { echo "   login answered $code"; exit 1; }
  echo "== a second deploy changes nothing and still comes up"
  dc up -d --force-recreate bootstrap control > /dev/null
  wait_http control http://127.0.0.1:8090/readyz
  # The re-created bootstrap container found everything in place: it must not mint (or print) a new key.
  again="$(dc logs --no-color --no-log-prefix bootstrap | grep -c '^secret=' || true)"
  [ "$again" = 0 ] || { echo "   a second bootstrap run printed a secret key again"; exit 1; }
  code="$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $SECRET" -H 'Content-Type: application/json' \
    "http://127.0.0.1:8090/api/projects/$PID/q/sql" -d "{\"from\":\"$from\",\"to\":\"$to\",\"sql\":\"SELECT 1\"}")"
  [ "$code" = 200 ] || { echo "   the first key stopped working after a redeploy ($code)"; exit 1; }
fi

echo "== ui and flags"
curl -fsS http://127.0.0.1:8090/ | grep -qi '<!doctype html' || { echo "   UI is not served"; exit 1; }
curl -fsS "http://127.0.0.1:8080/flags/$TOKEN.json" | grep -q '"flags"' || { echo "   ruleset is not served"; exit 1; }

echo "== every long-running container reports healthy"
for svc in ingest shipper compactor query control; do
  for _ in $(seq 1 60); do
    state="$(docker inspect -f '{{.State.Health.Status}}' "$(dc ps -q "$svc")" 2> /dev/null || echo missing)"
    [ "$state" = healthy ] && break
    sleep 1
  done
  [ "$state" = healthy ] || { echo "   $svc is $state"; exit 1; }
done
echo "   all healthy"

echo "== no listener failed to start"
if dc logs --no-color 2>&1 | grep -iE "could not start|address already in use|permission denied"; then
  echo "   a service reported a startup problem"; exit 1
fi

echo "== graceful stop"
dc stop -t 90 ingest > /dev/null
[ "$(docker inspect -f '{{.State.ExitCode}}' "$(dc ps -aq ingest)")" = 0 ] || { echo "   ingestion did not exit cleanly"; exit 1; }
echo "compose smoke test passed ($MODE)"
