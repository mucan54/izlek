#!/usr/bin/env bash
# Tek makinede, dosya deposuyla tüm servisleri çalıştırır (geliştirme ve deneme içindir).
#
#   deploy/local/run.sh                 # derle, ilk kurulumu yap, başlat; Ctrl-C ile durdur
#   LA_HOME=/var/tmp/izlek deploy/local/run.sh
#
# Gerekenler: Go 1.24+, Rust (engine/rust-toolchain.toml), Node 22 (yalnızca UI/SDK paketleri yoksa).
# Portlar: ingestion 8080 · sorgu 8081 · control plane + UI 8090.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
LA_HOME="${LA_HOME:-$ROOT/.local}"
BIN="$LA_HOME/bin"
PROFILE="${LA_PROFILE:-release}"   # release | debug
mkdir -p "$BIN" "$LA_HOME/store" "$LA_HOME/wal" "$LA_HOME/logs"

export LA_STORE_URL="file://$LA_HOME/store"
export LA_CONTROL_DB="$LA_HOME/control.db"
export LA_QUERY_URL="http://127.0.0.1:8081"
export LA_COOKIE_SECURE=false          # yalnızca yerel HTTP için
SECRET_FILE="$LA_HOME/internal.secret"
[ -s "$SECRET_FILE" ] || (umask 077; head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n' > "$SECRET_FILE")
export LA_QUERY_INTERNAL_SECRET="$(cat "$SECRET_FILE")"

echo "== derleme"
(cd "$ROOT" && CGO_ENABLED=0 go build -o "$BIN/" ./ingest/cmd/la-ingest ./control/cmd/la-control ./canary/cmd/la-canary)
if [ -z "${LA_SHIPPER_BIN:-}" ] || [ -z "${LA_QUERY_BIN:-}" ]; then
  flag=""; [ "$PROFILE" = release ] && flag="--release"
  (cd "$ROOT/engine" && cargo build $flag -p la-shipper -p la-query)
  LA_SHIPPER_BIN="$ROOT/engine/target/$PROFILE/la-shipper"
  LA_QUERY_BIN="$ROOT/engine/target/$PROFILE/la-query"
fi
if [ ! -f "$ROOT/ui/dist/index.html" ]; then (cd "$ROOT/ui" && npm ci && npm run build); fi
export LA_UI_DIR="$ROOT/ui/dist"

if [ ! -f "$LA_CONTROL_DB" ]; then
  echo "== ilk kurulum"
  EMAIL="${LA_ADMIN_EMAIL:-admin@example.com}"
  PASS="${LA_ADMIN_PASSWORD:-$(head -c 12 /dev/urandom | od -An -tx1 | tr -d ' \n')}"
  (umask 077; "$BIN/la-control" bootstrap --email "$EMAIL" --password "$PASS" --org "${LA_ORG:-Yerel}" --project "${LA_PROJECT:-Deneme}" > "$LA_HOME/bootstrap.txt")
  (umask 077; printf 'email=%s\npassword=%s\n' "$EMAIL" "$PASS" >> "$LA_HOME/bootstrap.txt")
  echo "   giriş bilgileri, yazma token'ı ve gizli anahtar: $LA_HOME/bootstrap.txt"
fi

# Servisler kendi oturumlarında çalışır: terminaldeki Ctrl-C yalnızca bu betiğe ulaşır,
# betik de her servise tek bir SIGTERM yollar (ingestion ikinci sinyalde yüklemeyi beklemeden çıkar).
DETACH=(); command -v setsid > /dev/null && DETACH=(setsid)
pids=()
run() { # ad, komut…
  local name="$1"; shift
  "${DETACH[@]}" "$@" >> "$LA_HOME/logs/$name.log" 2>&1 &
  pids+=("$!")
  echo "   $name (pid $!) → $LA_HOME/logs/$name.log"
}
stop() { # $1: INT | TERM | EXIT
  trap - INT TERM EXIT
  echo; echo "== durduruluyor"
  # setsid yoksa servisler Ctrl-C'yi terminalden zaten almıştır; ikinci sinyal yollanmaz.
  if [ "$1" != INT ] || [ ${#DETACH[@]} -gt 0 ]; then
    for pid in "${pids[@]}"; do kill -TERM "$pid" 2> /dev/null || true; done
  fi
  wait || true
}
trap 'stop INT; exit 0' INT
trap 'stop TERM; exit 0' TERM
trap 'stop EXIT' EXIT

echo "== servisler"
LA_LISTEN="${LA_LISTEN:-127.0.0.1:8080}" LA_NODE_ID=local-01 LA_WAL_DIR="$LA_HOME/wal" run ingest "$BIN/la-ingest"
LA_QUERY_LISTEN=127.0.0.1:8081 LA_QUERY_CACHE_DIR="$LA_HOME/qcache" LA_QUERY_CACHE_BYTES=$((2 << 30)) run query "$LA_QUERY_BIN"
LA_CONTROL_LISTEN="${LA_CONTROL_LISTEN:-127.0.0.1:8090}" run control "$BIN/la-control" serve
LA_METRICS_ADDR=127.0.0.1:9102 run shipper "$LA_SHIPPER_BIN" ship
LA_METRICS_ADDR=127.0.0.1:9103 run compactor "$LA_SHIPPER_BIN" compact

echo
echo "UI:        http://${LA_CONTROL_LISTEN:-127.0.0.1:8090}/"
echo "Ingestion: http://${LA_LISTEN:-127.0.0.1:8080}/   (SDK host)"
echo "Durdurmak için Ctrl-C."
wait -n || true
echo "bir servis beklenmedik şekilde çıktı; loglara bakın: $LA_HOME/logs/"
exit 1
