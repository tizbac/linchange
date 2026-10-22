# Calls: STUN, TURN and the ports they need

Meeting calls are WebRTC: the browsers talk to each other directly, and
linchange carries only the signalling. Two browsers behind NATs cannot always
find a direct path, so the server also runs the fallback — a TURN relay — and
answers STUN on the same socket. This is part of linchange by default, so a
single install is enough; no external chat or relay service is required.

## Ports

| Port | Protocol | Purpose |
| --- | --- | --- |
| `3478` | UDP | STUN and TURN |
| `3478` | TCP | TURN over TCP (for clients whose network blocks UDP) |
| `49152`–`49251` | UDP | TURN relay allocations; every port in the range must be reachable |

The relay range is `webrtc.turn.relay_min_port`–`relay_max_port`. It is 100
ports by default, enough for dozens of simultaneous calls. Widening it helps
only if calls are being refused for lack of a free relay port; narrowing it
makes the firewall rule smaller.

If the server is behind a NAT, forward those ports to it and set
`webrtc.turn.external_ip` (or `server.public_ip`) to the public address. The
relay advertises that address to the browsers; a bind address here would tell
them to connect somewhere they cannot reach.

The HTTP/HTTPS port of the web app is required as before, and so is outbound
access from the browser to the STUN/TURN port.

## Configuration

The built-in server is on by default:

```yaml
webrtc:
  turn:
    enabled: true
    realm: linchange
    listen: { addr: ":3478" }
    relay_min_port: 49152
    relay_max_port: 49251
    # Empty falls back to server.public_ip.
    external_ip: ""
    # Empty uses external_ip.
    host: ""
    # Empty generates a random secret at startup, which invalidates issued
    # credentials on restart. Set a stable value to survive restarts.
    secret: ""
    credential_ttl: 2h
```

Credentials are short-lived: the browser receives a username that is an expiry
timestamp and a password that is an HMAC of it under `secret`. The secret never
reaches the browser, and expired credentials are refused even if their
signature is valid.

## Using an external STUN/TURN server

Set `webrtc.ice_servers`; it replaces the built-in server, which then does not
bind its ports. A STUN entry needs only `urls`; a TURN entry also needs the
long-term `username` and `credential`.

```yaml
webrtc:
  ice_servers:
    - urls:
        - "stun:turn.example.com:3478"
        - "turn:turn.example.com:3478?transport=udp"
        - "turn:turn.example.com:3478?transport=tcp"
      username: "linchange"
      credential: "${TURN_PASSWORD}"
  # No need to run the built-in server when an external one is named.
  turn:
    enabled: false
```

The browser fetches this list from `GET /api/v1/webrtc/ice` after signing in, so
changing the configuration does not require a client change.

## Docker

`docker-compose.yml` publishes the production ports above. The demo profile
uses `53478` and the relay range `49252–49352` so it can run beside a
development instance; both are set in `config/linchange.demo.yaml` and must stay
in step with the compose file.

## Troubleshooting

- **Calls connect on a LAN but not across the internet.** The relay address is
  wrong: set `webrtc.turn.external_ip` to the public address and make sure the
  UDP ports are forwarded.
- **The call is refused only for some users.** Their network blocks UDP. The
  built-in server also listens on TCP 3478, which is advertised as a transport;
  check that `3478/tcp` is reachable.
- **No relay port is free.** Widen `relay_min_port`–`relay_max_port` and open
  the new range.
