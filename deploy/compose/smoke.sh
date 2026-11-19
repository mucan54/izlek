#!/usr/bin/env bash
# Compose kurulumunun duman testi: derle, kur, başlat, bir event gönder, sorguda gör,
# kanaryayı bir kez koştur, UI'ın sunulduğunu doğrula. Sonunda her şeyi (birim dahil) siler.
#
#   deploy/compose/smoke.sh            # CI ve elle doğrulama
#   KEEP=1 deploy/compose/smoke.sh     # hata ayıklamak için çalışır bırak
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"

export COMPOSE_PROJECT_NAME="${COMPOSE_PROJECT_NAME:-izlek-smoke}"
export LA_QUERY_INTERNAL_SECRET="${LA_QUERY_INTERNAL_SECRET:-$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')}"
export LA_COOKIE_SECURE=false
dc() { docker compose "$@"; }

cleanup() {
  code=$?
  if [ "$code" -ne 0 ]; then
    echo "== FAILED (exit $code); service logs:"
    dc --profile canary logs --no-color --tail 40 || true
  fi
  [ -n "${KEEP:-}" ] || dc --profile canary down -v --remove-orphans > /dev/null 2>&1 || true
  exit "$code"
}
trap cleanup EXIT

echo "== build"
dc --profile canary build

echo "== bootstrap"
boot="$(dc run --rm -T control bootstrap --email smoke@example.com --password 'smoke-password-1' --org Smoke --project Smoke --json | tail -n 1)"
field() { printf '%s' "$boot" | python3 -c "import sys, json; print(json.load(sys.stdin)['$1'])"; }
TOKEN="$(field token)"; SECRET="$(field secret)"; PID="$(field project_id)"

echo "== up"
dc up -d
wait_http() { # ad, url
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

echo "== canary"
dc --profile canary run --rm -T -e LA_CANARY_TOKEN="$TOKEN" -e LA_CANARY_QUERY_KEY="$SECRET" canary -once -batches 3 -timeout 3m -poll 2s

echo "== ui and flags"
curl -fsS http://127.0.0.1:8090/ | grep -qi '<!doctype html' || { echo "   UI is not served"; exit 1; }
curl -fsS "http://127.0.0.1:8080/flags/$TOKEN.json" | grep -q '"flags"' || { echo "   ruleset is not served"; exit 1; }

echo "== no listener failed to start"
if dc logs --no-color 2>&1 | grep -iE "could not start|address already in use|permission denied"; then
  echo "   a service reported a startup problem"; exit 1
fi

echo "== graceful stop"
dc stop -t 90 ingest > /dev/null
[ "$(docker inspect -f '{{.State.ExitCode}}' "$(dc ps -aq ingest)")" = 0 ] || { echo "   ingestion did not exit cleanly"; exit 1; }
echo "compose smoke test passed"
