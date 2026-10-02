# linchange — implementation plan

Full-featured mail + groupware server. Go, sqlx, PostgreSQL. IMAP / POP3 / SMTP
listeners, LDAP/AD and local users, personal + workgroup calendars, an Angular +
Tailwind webmail — a real mail client (inbox, compose, folders, search), not just
an admin console — plus a calendar UI and an admin section, and Exchange
compatibility (Autodiscover, EWS, ActiveSync).

MAPI/HTTP (the protocol Outlook for Windows speaks natively) is in scope as the
final phase (P11) and is implemented in Go. No mature Go library exists for it,
so it is hand-written against the [MS-OXCMAPIHTTP] Open Specification, using
gromox (grommunio/gromox, C++) as the reference server. See "MAPI/HTTP (P11)"
below.

---

## Locked decisions

| Question | Decision |
| --- | --- |
| Inbound SMTP | The Go server also listens on `:25` and acts as a full inbound MTA. Submission (`:587`/`:465`) remains authenticated-only. |
| Exchange scope | Autodiscover + EWS + ActiveSync (P10), then MAPI/HTTP (P11, last). |
| Message storage | Content-addressed blob store on disk (SHA-256) + all metadata in PostgreSQL. |
| AD user with off-domain address | Login allowed **only** for members of the configured AD `domain_admins_group_dn`, and only as a `domain_admin` (no email functionality). Everyone else in that situation is refused. |
| Configuration | A single YAML file passed as a CLI parameter (`--config`). Required. |

### Operational consequence of self-hosted `:25`

Outbound deliverability depends on infrastructure this codebase cannot control.
Nothing we send will be accepted unless all of the following are true:

- the host's port 25 is unblocked outbound,
- PTR (reverse DNS) for the sending IP resolves back to `server.hostname`,
- `server.hostname` has an A/AAAA record pointing at `server.public_ip`,
- SPF, DKIM and DMARC records are actually published in DNS.

A DNS-record wizard in the admin panel generates the exact records to publish.
Assume a warm-up period (weeks) before bulk external mail is accepted.

---

## Identity model (the load-bearing decision)

A single `users` table with `address citext` as primary key, so usernames are
full email addresses and are unique by construction. A `source` enum collapses
every special case into a row rather than special-casing it in each subsystem:

| `source` | Owns a password | Has mail | Notes |
| --- | --- | --- | --- |
| `local` | yes (argon2id) | yes | TOTP optional; password reset via secondary email |
| `ldap` | no — AD bind | yes, **only if the address domain is hosted here** | TOTP enforced locally; AD does not provide it |
| `domain_admin` | local or LDAP | **no** | Web login works, mail protocols are refused |
| `shared` | no — granted access | yes | Shared mailbox |

Consequences worth stating explicitly:

- A shared mailbox is a `users` row with `source='shared'` **plus** a row in
  `shared_mailboxes`, so `alice@example.com#support` is a single
  login-resolution rule in one function, shared by webmail, IMAP, POP3, EWS and
  EAS.
- A domain admin that is not a mail user needs no separate table; the same
  "can this principal hold mail" predicate governs all five protocols.
- The off-domain AD rule is one branch in the LDAP provider, not a new concept.

### An alias is not a shared mailbox

These are two separate features and the schema keeps them apart. Conflating
them is the mistake that made `shared_mailbox_grants` reference `users`
directly, which allowed a team's access to be granted against an ordinary
person's private mailbox.

| | Alias | Shared mailbox |
| --- | --- | --- |
| What it is | a second address that resolves to a mailbox that already exists | a mailbox that holds mail in its own right |
| Address | the alias, e.g. `helpdesk@example.com` | the mailbox, e.g. `support@example.com` |
| Folders and messages | none; it forwards into the target | its own, with its own quota |
| Members | none | a grant list, per member, with a level |
| Deleting it | changes nothing already received | deletes the mail it held |
| Used for | a person reachable by several names | a team queue, ticket address, or role account |

The two compose: an alias may target a shared mailbox, which is how a team gets
an address reaching several people without any of them taking a second personal
address. `aliases.target_address` referencing a `shared_mailboxes` row is
supported and tested.

Both are exposed over the same protocols, because both resolve to a mailbox:
an alias is expanded at delivery time and never appears to a client as a
separate destination.

`policy` is the single package that answers "may principal P do action A",
used by every protocol server and the HTTP API. Nothing else makes that
decision.

---

## Verified dependency set

Versions confirmed live against `proxy.golang.org` / `registry.npmjs.org`.

| Concern | Module | Version | Rationale |
| --- | --- | --- | --- |
| IMAP server | `github.com/emersion/go-imap/v2` (`imapserver`) | `v2.0.0-beta.8` | Canonical library with a clean `Session` interface; ships IDLE, MOVE, NAMESPACE, SASL. `SessionIMAP4rev2` requires NAMESPACE+MOVE, which we implement. |
| SMTP | `github.com/emersion/go-smtp` | `v0.25.0` | Actively maintained. `Backend`/`Session`/`AuthSession`/`LMTPSession` cover both submission and LMTP. |
| MIME | `github.com/emersion/go-message` | `v0.18.2` | RFC 5322 parse and build. |
| DKIM/SPF/DMARC | `github.com/emersion/go-msgauth` | `v0.7.0` | `dkim` sign+verify, `spf`, `dmarc` packages. |
| SASL | `github.com/emersion/go-sasl` | pseudo-version | PLAIN, LOGIN, SCRAM. |
| ORM | `github.com/jmoiron/sqlx` | `v1.4.0` | As requested. |
| DB driver | `github.com/jackc/pgx/v5` (`pgx/v5/stdlib`) | `v5.11.0` | Drives sqlx. |
| LDAP/AD | `github.com/go-ldap/ldap/v3` | `v3.4.14` | Active maintenance. |
| TOTP | `github.com/pquerna/otp` | `v1.5.0` | TOTP + recovery codes. |
| Passwords | `golang.org/x/crypto` | `v0.57.0` | argon2id, bcrypt fallback. |
| iCalendar | `github.com/arran4/golang-ical` | `v0.3.7` | RFC 5545 parse/serialise incl. RRULE. |
| Recurrence | `github.com/teambition/rrule-go` | `v1.8.2` | Expands RRULE into concrete occurrences. |
| Config | `gopkg.in/yaml.v3` | `v3.0.1` | Backs the `--config` requirement. |
| HTTP | `github.com/go-chi/chi/v5` | — | Router. |
| Scheduler | `github.com/robfig/cron/v3` | `v3.0.1` | Queue retries, LDAP sync, retention. |
| ACME/TLS | `github.com/caddyserver/certmagic` | `v0.25.4` | Optional Let's Encrypt. |
| Metrics | `github.com/prometheus/client_golang` | — | `/metrics`. |
| ActiveSync WBXML | `github.com/hstern/go-activesync/wbxml` | `v1.1.0` | Reusable WBXML 1.3 codec with all 25 EAS code pages. Reusing it saves ~1.5k LOC of codec work. |
| Frontend | Angular | `22.2.0` | |
| CSS | Tailwind CSS | `4.3.3` | v4 is CSS-first (`@theme` in CSS, no `tailwind.config.js`). |
| Calendar UI | `@fullcalendar/angular` | `7.1.0` | Plus `@fullcalendar/rrule`, `@fullcalendar/resource-timegrid` for workgroup booking. |

### Libraries that do not exist and are hand-written

- **POP3 server.** No mature Go implementation. RFC 1939 is small; the full
  command set plus `STLS` and `UIDL` is ~400 lines.
- **MAPI/HTTP.** No Go implementation exists; hand-written as the last phase
  (P11) against [MS-OXCMAPIHTTP], with gromox as the reference. See
  "MAPI/HTTP (P11)" below.
- **IMAP `QUOTA` extension.** Not present in `go-imap/v2`'s `imapserver`.
  Quotas are enforced server-side at delivery and APPEND and surfaced in the
  admin panel and webmail. Advertising `QUOTA` to IMAP clients requires a small
  patch against `go-imap/v2`; this is a known gap, not an oversight.

---

## Repository layout

```
cmd/linchange/main.go        single binary; --config is required
config/                      yaml.v3 -> typed structs, defaults, validation, ${ENV} interpolation
migrations/                  embedded, versioned SQL
internal/
  store/                     sqlx repositories, one file per aggregate
  blobstore/                 content-addressed SHA-256 store on disk
  policy/                    the one place that authorises every action
  auth/                      provider chain: local | ldap | shared | domain_admin
  mail/                      RFC 5322 parse/build, envelope, threading, quota, send caps
  smtpd/                     :25 inbound MTA and :587/:465 submission (same lib, different backend)
  imapd/                     imapserver.Session backed by store; IDLE via LISTEN/NOTIFY
  pop3d/                     hand-written RFC 1939
  lmtpd/                     go-smtp LMTPSession; free, and makes integration testing real
  delivery/                  persistent Postgres outbox, workers, backoff, DSN bounces
  inbound/                   SPF/DKIM/DMARC evaluation, greylisting, spam scoring
  httpapi/                   chi router: rest/ | ews/ | eas/ | autodiscover/
  mapihttp/                  P11: MAPI/HTTP (Outlook-native), hand-written vs [MS-OXCMAPIHTTP]
  admin/                     domain-admin service layer, impersonation, audit
  ldapsync/                  periodic AD reconcile
 web/                         Angular 22 + Tailwind 4 mail client (webmail + calendar + admin)
```

## Data model

```
domains             name (citext PK), status, dkim_selector, quota defaults, created_at
users               address (citext PK), source, display_name, enabled, mailbox_enabled,
                    quota_bytes, mailbox_enabled, created_at
credentials         user_id PK, argon2id params + hash           -- local only
ldap_links          provider, user_id PK, dn, last_synced_at
user_secondary_emails  user_id, email                            -- password reset target
user_totp           user_id PK, secret_encrypted, enabled, recovery_codes[], created_at
workgroups          name, domain_id
workgroup_members   workgroup_id, user_id, role
aliases             address (citext PK), target_kind, target_id
shared_mailboxes   address (citext PK, FK users), display_name, colour, open_access
shared_mailbox_grants  shared_address (FK shared_mailboxes), member_address, level
outbox_recipients  (outbox_id, position), address, kind, state, attempts
mailboxes           id, user_id, name, uidvalidity, uidnext, special_use, subscribed
messages            id, mailbox_id, uid, blob_sha256, size, internaldate, flags[],
                    thread_key, subject, from_addr, snippet, has_attachments
message_addresses   message_id, header_name, address, position    -- IMAP SEARCH, threading
calendars           id, owner_user_id NULL, workgroup_id NULL, name, color, timezone
events              id, calendar_id, uid, ical_blob, dtstart, dtend, rrule, seq,
                    dtstamp, status, organizer
attendees           event_id, address, role, partstat
sessions            id, user_id, refresh_token_hash, ip, user_agent, created_at, expires_at
reset_tokens        id, user_id, token_hash, expires_at, used_at
outbox              id, envelope_from, envelope_to[], blob_sha256, size, attempts,
                    next_attempt_at, state, last_error, dsn_at
greylist            sender_ip, sender, rcpt, first_seen_at, last_seen_at, expires_at
send_counters       user_id, day, count
impersonation_grants  actor_user_id, target_user_id, granted_at, expires_at
audit_log           actor_user_id, action, target_type, target_id, impersonated_user_id, ip, ts, detail
dkim_keys           domain_id, selector, private_key_encrypted, public_key, created_at
ldap_sync_state     provider, last_run_at, last_error
```

## Protocol surfaces

| Listener | Behaviour |
| --- | --- |
| `:25` | Inbound MTA. No auth. Accepts only addresses in local DB domains. Greylisting, SPF + DKIM-verify + DMARC alignment, spam score routing to INBOX/Junk, size and recipient caps, HELO validation. |
| `:587` | Submission, STARTTLS. `AUTH PLAIN/LOGIN` over TLS only. `policy.EnforceSenderIdentity` rejects any `MAIL FROM` that is not the authenticated principal or a verified alias of it. `policy.CheckDailySendLimit` applies the X/day cap. |
| `:465` | Submission, implicit TLS. Identical policy to `:587`. |
| `:143` / `:993` | IMAP. Full store-backed session. `ENABLE`/`IDLE` fed by Postgres `LISTEN/NOTIFY`. `MOVE`, `NAMESPACE`, `UNSELECT`, `THREAD`, `SORT` served from indexed `message_addresses`. |
| `:110` / `:995` | POP3. INBOX-only maildrop, `DELE`-on-`QUIT`, `STLS`, `UIDL`, `TOP`. |
| `:24` (loopback) | LMTP. Used by tests and dev; also lets an external MTA front us later. |
| `:8080` / `:8443` | HTTP: REST API, admin panel, webmail, calendar, Autodiscover, EWS, ActiveSync. |

Exchange endpoints are built on the same store and the same `policy` package, so
there is exactly one definition of what a principal may do.

### Exchange endpoints

- **Autodiscover** (MS-OXDISCO POX) — advertises IMAP, SMTP and EAS. Lets
  Outlook for Windows build a working profile.
- **EWS** — `FindItem`, `GetItem`, `CreateItem`, `UpdateItem`, `DeleteItem`,
  `SendItem`, calendar items, `GetUserAvailability`, `ResolveNames`.
- **ActiveSync** — `Provision`, `FolderSync`, `Sync`, `Ping`, `SendMail`,
  `MoveItems`, reusing the `hstern/go-activesync/wbxml` codec.
- **Shared mailboxes** in Exchange use the standard delegation model
  (`shared_mailbox_grants`), not the `#` suffix, which is an IMAP/POP3/login
  convention only.

### MAPI/HTTP (P11)

The last phase. MAPI/HTTP is what Outlook for Windows speaks natively — the
protocol real users will actually hit — so it is worth the cost even though it
is the largest single piece of work.

- **Specification.** [MS-OXCMAPIHTTP] (Microsoft Open Specification) is the
  normative definition of the wire protocol; [MS-OXCFRMS] covers the
  Client-Driven Search subset. Both live on learn.microsoft.com.
- **Reference implementation.** gromox (`github.com/grommunio/gromox`) is a C++
  groupware server that implements MAPI/HTTP (plus RPC/HTTP, EWS, IMAP, POP3).
  It is the working reference for message framing, property/bulk marshalling,
  and the request/response lifecycle. Its developer docs
  (`docs.grommunio.com/dev/gromox/`) collect MS-OXO notes and a MAPI property
  reference.
- **Approach.** Hand-written in Go — no Go MAPI/HTTP server library exists.
  Build the MAPI property + bulk-marshalling layer first, then the HTTP
  transport (the MAPI/HTTP request verbs and framing), mapping every request
  onto the same `store` and `policy` the other protocols use so authorisation
  stays in one place. Reuse the blob store for message bodies. Cross-check
  behaviour against gromox while developing.

## Phases

Status is updated as each phase lands, so this file stays the record of what
exists rather than a wish list.

| # | Deliverable | Verification | Status |
| --- | --- | --- | --- |
| P0 | config → typed/validated; sqlx + pgx; embedded migrations; blobstore; compose; `main` | `go build`, `go test ./...`, boots against real Postgres | done |
| P1 | auth chain; argon2id; TOTP + recovery; reset via secondary email; JWT; chi skeleton; Angular + Tailwind shell with login and TOTP screens | end-to-end browser login | done |
| P2 | domains; mailboxes; aliases; shared mailboxes; quotas; admin API; impersonation + audit; admin panel | CRUD, quota and impersonation tests | ✅ DONE |
| P3 | submission policy; DKIM; outbox with retry and DSN bounces | real send to an external SMTP sink | ✅ DONE |
| P4 | IMAP server | Thunderbird + `imapclient` conformance run | ✅ DONE |
| P5 | POP3 server | `openssl s_client` command transcripts | pending |
| P6 | inbound `:25`; SPF/DKIM/DMARC; greylisting; junk routing | inbound fixture plus live test | pending |
| P7 | webmail: folders, list, read, compose, send, search, threading, attachments, aliases, shared mailboxes, rules | browser E2E | pending |
| P8 | LDAP/AD provider; sync; off-domain → admin-only rule | LDAP fixture | pending |
| P9 | calendars: personal + workgroup; recurrence; iMIP REQUEST/REPLY/CANCEL; free/busy; FullCalendar UI | ICS round-trip and expansion tests | pending |
| P10 | Autodiscover; EWS; ActiveSync | interop traces | pending |
| P11 | MAPI/HTTP (Outlook-native), hand-written in Go | Outlook client connects; cross-checked against gromox | pending (last) |

### P0 notes

Things established while landing P0 that later phases depend on:

- `config` expands `${VAR}` on the **parsed YAML tree**, not on raw text, so a
  `${VAR}` inside a comment documenting an unconfigured LDAP provider is not a
  startup error. Only string values are expanded; keys are not.
- `config/linchange.example.yaml` is covered by `config/example_test.go`, so a
  shipped example that does not validate fails the build.
- The pgx `database/sql` driver is registered by a blank import in
  `internal/store`, because sqlx needs it and no compiler catches its absence.
- `make test-integration` fails if any test reports SKIP. The store tests
  previously skipped silently on a credential mismatch while still reporting
  `ok`, which looked like passing integration coverage.
- PostgreSQL 18 stores data under `/var/lib/postgresql`, not
  `/var/lib/postgresql/data`; the compose volume mounts the parent directory.

### Schema corrections made before P2

`0001` was corrected in place while it was still unapplied to any real
database, and each correction is now pinned by a test in
`internal/store/{schema,shared}_test.go`:

- `aliases.target_id` was a single `bigint`, which could not express "point
  this address at Alice". Replaced with `target_address citext` for a person or
  shared mailbox and `workgroup_id bigint` for a workgroup, with a check
  enforcing that exactly one is set and agrees with `target_kind`.
- `outbox` held recipients and their per-recipient header kinds as two
  parallel `text[]`/`enum[]` arrays that nothing kept aligned, so partial
  delivery could not be recorded. Replaced with an `outbox_recipients` table
  carrying position, kind, state and attempts.
- `shared_mailbox_grants.shared_address` referenced `users`, so a team could be
  granted access to an ordinary person's private mailbox. It now references a
  dedicated `shared_mailboxes` table, which is what makes "is a shared mailbox"
  an explicit declaration rather than an inference from `users.source`.
- `users.mailbox_enabled` defaults to true and nothing pinned `domain_admin` to
  false, so a domain admin could have owned folders, opened IMAP sessions and
  been a shared mailbox member. Added a check constraint.
- An empty envelope recipient could be queued; now rejected.

## Configuration

`--config <path.yaml>` is required. Missing, unreadable, or invalid config is a
fatal startup error with a clear message — the server never runs on defaults
alone. `${VAR}` inside any string is interpolated from the environment so
secrets need not live in the file.

```yaml
server:
  hostname: mail.example.com
  public_ip: 203.0.113.10
  data_dir: /var/lib/linchange
  trusted_proxies: [10.0.0.0/8]

database:
  url: postgres://linchange:${DB_PASSWORD}@localhost:5432/linchange
  max_open: 25
  max_idle: 5
  max_lifetime: 30m

listeners:
  smtp_inbound:         { addr: ":25",  max_message_bytes: 52428800 }
  smtp_submission:      { addr: ":587" }
  smtp_submission_tls:  { addr: ":465" }
  imap:                 { addr: ":143" }
  imaps:                { addr: ":993" }
  pop3:                 { addr: ":110" }
  pop3s:                { addr: ":995" }
  lmtp:                 { addr: "127.0.0.1:24" }
  http:                 { addr: ":8080" }
  https:                { addr: ":8443" }

tls:
  cert_file: ""
  key_file: ""
  acme:
    enabled: false
    email: ""
    domains: []
    cache_dir: /var/lib/linchange/acme

auth:
  local:
    argon2: { memory_kib: 65536, iterations: 3, parallelism: 4, salt_len: 16 }
    min_password_length: 12
  jwt:
    secret: ${JWT_SECRET}
    issuer: linchange
    access_ttl: 15m
    refresh_ttl: 720h
  totp:
    enabled: true
    issuer: linchange
    window: 1
    recovery_codes: 10
  reset_password:
    enabled: true
    token_ttl: 30m
  rate_limit:
    per_ip: 10
    window: 15m
  session:
    idle_timeout: 30m
    absolute_timeout: 12h
  impersonation:
    allowed: true
    require_reason: true

ldap:
  providers:
    - name: corp
      url: ldaps://dc1.corp.example.com:636
      start_tls: false
      bind_dn: "CN=svc-linchange,OU=Service,DC=corp,DC=example,DC=com"
      bind_password: ${LDAP_BIND_PASSWORD}
      base_dn: "OU=Users,DC=corp,DC=example,DC=com"
      user_filter: "(&(objectClass=user)(mail={{username}}))"
      attributes:
        mail: [mail, proxyAddresses, targetAddress]
        display_name: [displayName, cn]
        member_of: [memberOf]
      # AD users whose address is off-domain get a domain_admin (no mail)
      # account if and only if they are in this group. Config is required.
      domain_admins_group_dn: "CN=Domain Admins,CN=Users,DC=corp,DC=example,DC=com"
      mailbox_quota_bytes: 10737418240
  sync:
    interval: 15m
    disable_missing_after: 72h
  cache_ttl: 5m

mail:
  submission:
    require_tls: true
    allow_insecure_auth: false
    enforce_sender_identity: true
    daily_send_limit: 500
  inbound:
    enabled: true
    greylisting: { enabled: true, delay: 5m, expiry: 4h, retry_window: 30m }
    spam: { junk_threshold: 5.0, reject_threshold: 12.0 }
    reject_unknown_recipients: true
  outbound:
    concurrency: 4
    per_domain_concurrency: 2
    retry: { max_attempts: 12, backoff_base: 5m, backoff_max: 12h }
    dsn: { enabled: true, delay: 4h }
  dkim:
    selector_prefix: s
    key_bits: 2048
  default_quota_bytes: 10737418240
  max_quota_bytes: 107374182400

exchange:
  enabled: true
  autodiscover: { path: /autodiscover/autodiscover.xml }
  ews: { path: /ews/exchange.asmx }
  eas: { path: /eas }

logging:
  level: info
  format: json
  file: ""
  audit: true
```

## Known risks, addressed as they arise

1. **IMAP `QUOTA` is absent from `go-imap/v2`.** Enforced server-side; surfacing
   it to clients needs a patch to that library.
2. **`go-imap/v2` is a beta tag.** It is the de-facto standard IMAP library for
   Go and is what maddy and gomox build on, but the API can still move. Pinned
   in `go.mod`; upgrades are deliberate.
3. **TOTP for AD users is local state**, not AD state. A database restore
   invalidates enrolments; recovery codes are the escape hatch.
4. **Threading needs persisted `Message-ID`/`References`**, not recomputation
   from headers on every read — hence the `message_addresses` table.
5. **Angular 22 and FullCalendar 7 are both very new majors.** Versions pinned
   exactly, lockfile committed.
6. **`:25` reputation.** See "Operational consequence" above.
