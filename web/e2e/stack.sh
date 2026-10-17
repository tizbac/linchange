#!/usr/bin/env bash
# Boots the linchange stack for the browser end-to-end tests: a fresh
# database, the server and the built web app. Every listener is moved off the
# standard ports so the tests can run alongside a normal `make dev` instance.
set -euo pipefail
cd "$(dirname "$0")/../.."

DB=${E2E_DATABASE:-linchange_e2e}
ADDR=${E2E_USER:-e2e-user@example.com}
PASS=${E2E_PASSWORD:-'correct horse battery staple'}
PORT=${E2E_PORT:-18080}

test -f .env || { echo "e2e: missing .env; run `make dev-config` first" >&2; exit 1; }
test -f linchange.dev.yaml || { echo "e2e: missing linchange.dev.yaml; run `make dev-config` first" >&2; exit 1; }
test -f web/dist/linchange-web/browser/index.html || { echo "e2e: missing web build; run `cd web && npm run build` first" >&2; exit 1; }

set -a; . ./.env; set +a

go build -o bin/linchange ./cmd/linchange
go run ./e2e/seed -config linchange.dev.yaml -database "$DB" -user "$ADDR" -password "$PASS"

# The generated config moves every listener to a test port, turns greylisting
# off (the tests deliver mail in one sitting) and makes a bounce land in
# seconds rather than four hours, so the send-failure step can watch it arrive.
sed -E \
  -e "s|^  url: .*|  url: postgres://linchange:linchange@127.0.0.1:55432/${DB}?sslmode=disable|" \
  -e "s|^  smtp_inbound: .*|  smtp_inbound: { addr: \":1025\" }|" \
  -e "s|^  smtp_submission: .*|  smtp_submission: { addr: \":1587\" }|" \
  -e "s|^  imap: .*|  imap: { addr: \":1143\" }|" \
  -e "s|^  pop3: .*|  pop3: { addr: \":1110\" }|" \
  -e "s|^  imaps: .*|  imaps: { addr: \":1993\" }|" \
  -e "s|^  pop3s: .*|  pop3s: { addr: \":1995\" }|" \
  -e "s|^  smtp_submission_tls: .*|  smtp_submission_tls: { addr: \":1465\" }|" \
  -e "s|^  http: .*|  http: { addr: \":${PORT}\" }|" \
  -e "/greylisting:/,/delay:/ s|^      enabled: .*|      enabled: false|" \
  -e "/^    dsn:/,/delay:/ s|^      delay: .*|      delay: 5s|" \
  linchange.dev.yaml > .e2e-linchange.yaml

# Autodiscover answers with the advertised ports, and the tests insist every
# URL it names can be reached, so the set has to describe the ports this stack
# actually publishes.
awk -v port="$PORT" '
  { print }
  /^exchange:/ {
    print "  advertised:"
    print "    imap: 1143"
    print "    imaps: 1993"
    print "    pop3: 1110"
    print "    pop3s: 1995"
    print "    smtp: 1025"
    print "    submission: 1587"
    print "    http: " port
  }' .e2e-linchange.yaml > .e2e-linchange.yaml.tmp \
  && mv .e2e-linchange.yaml.tmp .e2e-linchange.yaml

exec ./bin/linchange --config .e2e-linchange.yaml --web-dir web/dist/linchange-web/browser
