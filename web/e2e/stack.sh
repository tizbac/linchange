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
  -e "s|^  public_url: .*|  public_url: http://127.0.0.1:${PORT}|" \
  linchange.dev.yaml > .e2e-linchange.yaml

# Autodiscover answers with the advertised ports, and the tests insist every
# URL it names can be reached, so the set has to describe the ports this stack
# actually publishes. The generated config already carries an advertised block
# (every port left at 0), so its values are rewritten in place: printing a
# second mapping under the same key is something a YAML parser refuses rather
# than something it merges.
awk -v port="$PORT" '
  function printAdvertised() {
    print "  advertised:"
    print "    imap: 1143"
    print "    imaps: 1993"
    print "    pop3: 1110"
    print "    pop3s: 1995"
    print "    smtp: 1025"
    print "    submission: 1587"
    print "    http: " port
  }
  BEGIN {
    ports["imap"] = 1143
    ports["imaps"] = 1993
    ports["pop3"] = 1110
    ports["pop3s"] = 1995
    ports["smtp"] = 1025
    ports["submission"] = 1587
    ports["http"] = port
  }
  /^exchange:/ { inexchange = 1 }
  inexchange && /^  advertised:/ { inblock = 1; seen = 1; print; next }
  inblock {
    if ($0 ~ /^    [a-z_]+: /) {
      key = $1
      sub(/:$/, "", key)
      if (key in ports) { print "    " key ": " ports[key]; next }
    } else {
      inblock = 0
    }
  }
  # A config without the block still gets one, before the next top-level key
  # closes the exchange section.
  inexchange && !seen && !inblock && /^[a-z]/ && !/^exchange:/ {
    printAdvertised()
    inexchange = 0
  }
  { print }
  END { if (inexchange && !seen) printAdvertised() }
' .e2e-linchange.yaml > .e2e-linchange.yaml.tmp \
  && mv .e2e-linchange.yaml.tmp .e2e-linchange.yaml

# Meeting invitations carry a join link built from meetings.public_url, and a
# config without one derives https://<hostname> — an address this stack does
# not serve, so a test that follows its own invitation lands on a dead page.
# The value is written under the heading (or a heading is added) rather than
# appended, because two public_url lines in one block are refused, not merged.
awk -v port="$PORT" '
  BEGIN { url = "  public_url: http://127.0.0.1:" port }
  /^meetings:/ { print; print url; inmeetings = 1; seen = 1; next }
  inmeetings && /^  public_url:/ { next }
  inmeetings && /^[^ ]/ { inmeetings = 0 }
  { print }
  END { if (!seen) { print "meetings:"; print url } }
' .e2e-linchange.yaml > .e2e-linchange.yaml.tmp \
  && mv .e2e-linchange.yaml.tmp .e2e-linchange.yaml

# Calls use the built-in STUN/TURN by default. Move it off 3478 so the e2e
# stack can run beside a development instance, and point the relay at loopback
# (this stack is only ever reached from the same machine). Any webrtc block the
# development config already has is replaced rather than duplicated.
awk '
  /^webrtc:/ { skip = 1; next }
  skip && /^[^ ]/ { skip = 0 }
  !skip { print }
' .e2e-linchange.yaml > .e2e-linchange.yaml.tmp \
  && mv .e2e-linchange.yaml.tmp .e2e-linchange.yaml
cat >> .e2e-linchange.yaml <<'YAML'
webrtc:
  turn:
    enabled: true
    realm: linchange
    listen: { addr: ":13478" }
    relay_min_port: 50000
    relay_max_port: 50100
    external_ip: 127.0.0.1
    secret: e2e-turn-secret
    credential_ttl: 1h
YAML

exec ./bin/linchange --config .e2e-linchange.yaml --web-dir web/dist/linchange-web/browser
