#!/usr/bin/env python3
"""End-to-end test of the Exchange ActiveSync endpoint, written as a client.

This drives a complete ActiveSync session the way a real client drives one:
negotiate, provision, sync folders, pull mail, send mail, and then check that
the mail it sent arrived back in the mailbox. It encodes every request from
the published [MS-ASWBXML] code pages (aswbxml_tags.py), not from the server's
own tables, so a request this script can build and a response it can decode
are requests and responses the published protocol can carry.

Each step is reported on its own. A step that fails says what the server sent
instead, and the decoder's anomaly report (unknown tokens, missing content
bits) is printed alongside, because "the server answered, but in a dialect no
client could read" is a finding, not a pass.

    python3 e2e/eas/eas_e2e.py --base-url http://localhost:58080 \
        --user admin@localhost --password 'DemoAdmin123!'

Exit status is 0 when every required step passed.
"""

from __future__ import annotations

import argparse
import base64
import email.message
import email.policy
import os
import smtplib
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid
from email.utils import formatdate, make_msgid

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import aswbxml
from aswbxml import Node, el

WIRE = "application/vnd.ms-sync.wbxml"
# The versions this client offers, highest first. The server advertises its own
# set in OPTIONS, and step_options picks the highest both support -- which is
# what a real client does, rather than assuming the server speaks the newest.
OFFERED_VERSIONS = ("16.1", "16.0", "14.1", "14.0", "12.1", "12.0", "2.5")
NEGOTIATED_FALLBACK = "12.1"


class Fail(Exception):
    """A step's expectation did not hold."""


class Skip(Exception):
    """A step cannot run, e.g. a prerequisite step failed."""


# ---------------------------------------------------------------------------
# HTTP and the client session
# ---------------------------------------------------------------------------


class HTTPResult:
    def __init__(self, status: int, headers, body: bytes):
        self.status = status
        # Header names arrive in whatever case the server used; a lookup that
        # cares would report "no MS-ASProtocolVersions header" for the
        # server's "MS-ASProtocolversions".
        self.headers = {k.lower(): v for k, v in headers.items()}
        self.body = body

    def header(self, name: str) -> str:
        return self.headers.get(name.lower(), "")

    def __str__(self) -> str:
        preview = self.body[:120].hex(" ") if self.body else "<empty>"
        return f"HTTP {self.status} ({len(self.body)} bytes: {preview})"


class CommandResult:
    """One command: the HTTP answer plus, when the body was WBXML, the tree."""

    def __init__(self, http: HTTPResult, doc=None, report=None):
        self.http = http
        self.doc = doc
        self.report = report

    @property
    def status(self) -> int:
        return self.http.status

    @property
    def anomalies(self) -> list:
        return self.report.anomalies if self.report else []

    def tree(self, cmd: str) -> Node:
        """The decoded document, refusing anything that is not the answer."""
        if self.doc is None:
            text = self.http.body.decode("utf-8", "replace").strip()[:200]
            raise Fail(f"{cmd}: HTTP {self.http.status} answered with plain "
                       f"text instead of WBXML: {text!r}")
        return self.doc

    def __str__(self) -> str:
        if self.doc is None:
            return str(self.http)
        notes = f", anomalies: {'; '.join(self.anomalies)}" if self.anomalies else ""
        return f"HTTP {self.status} <{self.doc.ns}:{self.doc.name}>{notes}"


class Client:
    """A minimal ActiveSync client: Basic auth, WBXML bodies, policy key."""

    def __init__(self, base: str, user: str, password: str,
                 device_id: str, device_type: str, timeout: float):
        self.base = base.rstrip("/")
        self.user = user
        self.device_id = device_id
        self.device_type = device_type
        self.timeout = timeout
        self.policy_key = "0"
        # What the client asks the server to speak, until OPTIONS negotiates
        # something both sides have.
        self.version = NEGOTIATED_FALLBACK
        token = base64.b64encode(f"{user}:{password}".encode()).decode()
        self._auth = f"Basic {token}"

    # --- transport ---------------------------------------------------------

    def _request(self, method: str, url: str, body: bytes | None = None,
                 headers: dict | None = None) -> HTTPResult:
        request = urllib.request.Request(url, data=body, method=method)
        request.add_header("Authorization", self._auth)
        request.add_header("User-Agent", "linchange-e2e/1.0 (ActiveSync client)")
        for key, value in (headers or {}).items():
            request.add_header(key, value)
        try:
            with urllib.request.urlopen(request, timeout=self.timeout) as reply:
                return HTTPResult(reply.status, dict(reply.headers), reply.read())
        except urllib.error.HTTPError as err:
            return HTTPResult(err.code, dict(err.headers), err.read())
        except (urllib.error.URLError, OSError, TimeoutError) as err:
            raise Fail(f"{method} {url} failed to connect: {err}") from err

    def _command_url(self, cmd: str) -> str:
        return (f"{self.base}/eas?Cmd={cmd}"
                f"&User={urllib.parse.quote(self.user)}"
                f"&DeviceId={self.device_id}&DeviceType={self.device_type}")

    def command(self, cmd: str, doc: Node) -> CommandResult:
        headers = {
            "Content-Type": WIRE,
            "MS-ASProtocolVersion": self.version,
            "X-MS-PolicyKey": self.policy_key,
        }
        http = self._request("POST", self._command_url(cmd),
                             aswbxml.encode(doc), headers)
        tree, report = None, None
        if http.body:
            try:
                tree, report = aswbxml.decode(http.body)
            except aswbxml.CodecError as err:
                if http.status == 200:
                    raise Fail(
                        f"{cmd}: the response is not readable WBXML ({err}); "
                        f"header {http.body[:8].hex(' ')}") from err
                # A non-200 answer is plain text by design; report it as such.
        return CommandResult(http, tree, report)

    def options(self) -> HTTPResult:
        return self._request("OPTIONS", self._command_url("Sync"))

    def get_root(self) -> HTTPResult:
        return self._request("GET", self._command_url("Sync"))


# ---------------------------------------------------------------------------
# One session's state, handed from step to step
# ---------------------------------------------------------------------------


class Session:
    def __init__(self, args):
        self.client = Client(args.base_url, args.user, args.password,
                             args.device_id, args.device_type, args.timeout)
        self.user = args.user
        self.smtp_host = args.smtp_host
        self.smtp_port = args.smtp_port
        self.nonce = uuid.uuid4().hex[:8]
        self.inbox_id: str | None = None
        self.trash_id: str | None = None
        self.sent_id: str | None = None
        self.folder_key = "0"
        self.sync_key = "0"
        self.policy_key_final = ""
        self.injected_subjects = []      # subjects SMTP-delivered to the mailbox
        self.injected_ids: dict = {}     # subject -> EAS ServerId
        self.moved_id: str | None = None

    def inbox(self) -> str:
        if not self.inbox_id:
            raise Skip("no Inbox ServerId: FolderSync did not succeed")
        return self.inbox_id


def expect(condition, message):
    if not condition:
        raise Fail(message)


def status_in(node: Node, allowed, what: str):
    value = node.int_at("Status", -1)
    expect(value in allowed,
           f"{what}: Status is {value}, expected one of {sorted(allowed)}")
    return value


# ---------------------------------------------------------------------------
# Steps
# ---------------------------------------------------------------------------


def step_options(s: Session) -> str:
    reply = s.client.options()
    expect(reply.status == 200, f"OPTIONS returned {reply.status}")
    versions = reply.header("MS-ASProtocolVersions")
    expect(versions, "no MS-ASProtocolVersions header")
    expect(any(v.strip() in ("12.1", "14.0", "14.1", "16.0") for v in versions.split(",")),
           f"advertised protocol versions {versions!r} stop before 12.1")
    commands = reply.header("MS-ASProtocolCommands")
    expect(commands, "no MS-ASProtocolCommands header")
    offered = {c.strip() for c in commands.split(",")}
    for needed in ("Sync", "FolderSync", "Provision", "SendMail", "GetItemEstimate",
                   "Settings", "Ping"):
        expect(needed in offered, f"OPTIONS does not advertise {needed} (got {sorted(offered)})")
    # A client speaks the highest version both sides have, so the rest of the
    # run tests what a real device would have negotiated.
    advertised = {v.strip() for v in versions.split(",")}
    common = [v for v in OFFERED_VERSIONS if v in advertised]
    expect(common, f"no protocol version in common: client offers "
                   f"{OFFERED_VERSIONS}, server advertises {versions!r}")
    s.client.version = common[0]
    return (f"versions {versions}; {len(offered)} commands; "
            f"negotiated {s.client.version}")


def step_service_root(s: Session) -> str:
    reply = s.client.get_root()
    expect(reply.status == 200, f"GET /eas returned {reply.status}")
    return "GET /eas answers 200"


def step_provision_initial(s: Session) -> str:
    """Phase 2 of MS-ASPROV: ask for the policy."""
    request = el("Provision", "Provision",
                 el("Settings", "DeviceInformation",
                    el("Settings", "Set",
                       el("Settings", "Model", "linchange-e2e"),
                       el("Settings", "FriendlyName", "linchange e2e client"),
                       el("Settings", "OS", "Linux"))),
                 el("Provision", "Policies",
                    el("Provision", "Policy",
                       el("Provision", "PolicyType", "MS-EAS-Provisioning-WBXML"))))
    reply = s.client.command("Provision", request)
    doc = reply.tree("Provision")
    expect(doc.name == "Provision", f"expected <Provision>, got {doc.name}")
    status_in(doc, {1}, "Provision")
    policy = (doc.find("Policies") or Node("", "Policies")).find("Policy")
    expect(policy is not None, "no Policies/Policy in the response")
    expect(policy.text_of("PolicyType").strip() == "MS-EAS-Provisioning-WBXML",
           f"PolicyType is {policy.text_of('PolicyType')!r}, "
           f"expected 'MS-EAS-Provisioning-WBXML'")
    key = policy.text_of("PolicyKey").strip()
    expect(key, "the policy download carries no PolicyKey")
    status_in(policy, {1}, "Policies/Policy")
    data = policy.find("Data")
    expect(data is not None, "the policy carries no Data")
    expect(data.descendant("EASProvisionDoc") is not None,
           "policy Data does not contain EASProvisionDoc")
    s.client.policy_key = key
    fields = len([n for n in data.descendant("EASProvisionDoc").children])
    note = "; ".join(f"{n}={v}" for n, v in reply.anomalies[:3])
    return f"policy key {key}, {fields} settings" + (f" [{note}]" if note else "")


def step_provision_ack(s: Session) -> str:
    """Phase 3: acknowledge the policy, receive the final key."""
    expect(s.client.policy_key != "0", "no temporary policy key from phase 2")
    request = el("Provision", "Provision",
                 el("Provision", "Policies",
                    el("Provision", "Policy",
                       el("Provision", "PolicyType", "MS-EAS-Provisioning-WBXML"),
                       el("Provision", "PolicyKey", s.client.policy_key),
                       el("Provision", "Status", "1"))))
    reply = s.client.command("Provision", request)
    doc = reply.tree("Provision")
    status_in(doc, {1}, "Provision (acknowledgement)")
    policy = doc.find("Policies").find("Policy") if doc.find("Policies") else None
    expect(policy is not None, "no Policies/Policy in the acknowledgement")
    final = policy.text_of("PolicyKey").strip()
    expect(final, "the acknowledgement carries no final PolicyKey")
    s.policy_key_final = final
    s.client.policy_key = final
    return f"final policy key {final}"


def step_foldersync_initial(s: Session) -> str:
    reply = s.client.command("FolderSync", el("FolderHierarchy", "FolderSync",
                                             el("FolderHierarchy", "SyncKey", "0")))
    doc = reply.tree("FolderSync")
    expect(doc.name == "FolderSync", f"expected <FolderSync>, got {doc.name}")
    status_in(doc, {1}, "FolderSync")
    changes = doc.find("Changes")
    expect(changes is not None, "no Changes in the FolderSync response")
    adds = changes.findall("Add")
    expect(adds, "the folder list came back empty")
    for add in adds:
        server_id = add.text_of("ServerId")
        name = add.text_of("DisplayName")
        kind = add.int_at("Type", -1)
        expect(server_id and name and kind > 0,
               f"folder Add lacks ServerId/DisplayName/Type: {add!r}")
        if kind == 2:
            s.inbox_id = server_id
        if kind == 4:
            s.trash_id = server_id
        if kind == 5:
            s.sent_id = server_id
    key = doc.text_of("SyncKey").strip()
    expect(key and key != "0", "FolderSync returned no usable SyncKey")
    s.folder_key = key
    types = sorted(a.int_at("Type", 0) for a in adds)
    return f"{len(adds)} folders (types {types}), key {key}"


def step_foldersync_incremental(s: Session) -> str:
    expect(s.folder_key != "0", "no FolderSync key from the initial sync")
    reply = s.client.command("FolderSync", el("FolderHierarchy", "FolderSync",
                                             el("FolderHierarchy", "SyncKey", s.folder_key)))
    doc = reply.tree("FolderSync")
    status_in(doc, {1}, "FolderSync (incremental)")
    changes = doc.find("Changes")
    expect(changes is not None, "an incremental FolderSync has no Changes")
    count = changes.int_at("Count", -1)
    expect(count == len(changes.findall("Add")),
           f"Changes says Count {count} but carries {len(changes.findall('Add'))} Adds")
    return f"{count} changes at key {doc.text_of('SyncKey')}"


def step_smtp_injection(s: Session) -> str:
    """Mail arrives over SMTP, so Sync has something real to hand out."""
    subjects = [f"linchange e2e {s.nonce} alpha",
                f"linchange e2e {s.nonce} beta"]
    try:
        with smtplib.SMTP(s.smtp_host, s.smtp_port, timeout=20) as smtp:
            for i, subject in enumerate(subjects):
                message = email.message.EmailMessage()
                message["From"] = f"sender{i}@example.com"
                message["To"] = s.user
                message["Subject"] = subject
                message["Date"] = formatdate(localtime=True)
                message["Message-ID"] = make_msgid(domain="example.com")
                message.set_content(f"body of message {i} of {s.nonce}\n")
                smtp.sendmail(f"sender{i}@example.com", [s.user], message.as_bytes())
    except (OSError, smtplib.SMTPException) as err:
        raise Fail(f"SMTP delivery to {s.smtp_host}:{s.smtp_port} failed: {err}") from err
    s.injected_subjects = subjects
    return f"2 messages delivered to {s.user} via {s.smtp_host}:{s.smtp_port}"


def step_item_estimate(s: Session) -> str:
    request = el("GetItemEstimate", "GetItemEstimate",
                 el("GetItemEstimate", "Collections",
                    el("GetItemEstimate", "Collection",
                       el("AirSync", "SyncKey", s.sync_key),
                       el("GetItemEstimate", "CollectionId", s.inbox()))))
    reply = s.client.command("GetItemEstimate", request)
    doc = reply.tree("GetItemEstimate")
    status_in(doc, {1}, "GetItemEstimate")
    collection = doc.descendant("Collection")
    expect(collection is not None, "no Collection in the estimate response")
    estimate = collection.int_at("Estimate", -1)
    expect(estimate >= 0, "the estimate carries no Estimate value")
    if s.injected_subjects:
        expect(estimate >= len(s.injected_subjects),
               f"estimate is {estimate}, below the {len(s.injected_subjects)} messages known to be there")
    return f"estimate {estimate}"


def _collection(reply: CommandResult, cmd: str) -> Node:
    doc = reply.tree(cmd)
    collections = doc.descendant("Collections")
    expect(collections is not None, f"{cmd}: no Collections in the response")
    first = collections.find("Collection")
    expect(first is not None, f"{cmd}: no Collection in Collections")
    return first


def step_sync_initial(s: Session) -> str:
    request = _sync_request(s, "0", get_changes=False)
    reply = s.client.command("Sync", request)
    collection = _collection(reply, "Sync")
    status_in(collection, {1}, "Sync")
    key = collection.text_of("SyncKey").strip()
    expect(key and key != "0", "Sync returned no usable SyncKey")
    s.sync_key = key

    commands = collection.find("Commands")
    expect(commands is not None,
           "the Sync response has no Commands (the protocol delivers changes "
           "as Commands/Add|Change|Delete)")
    adds = commands.findall("Add")
    expect(adds, "the Sync response delivered no items")

    by_subject = {}
    for add in adds:
        server_id = add.text_of("ServerId")
        data = add.find("ApplicationData")
        expect(data is not None,
               f"item {server_id} is not wrapped in ApplicationData")
        subject = data.text_of("Subject")
        expect(subject, f"item {server_id} carries no Subject")
        body = data.find("Body")
        if body is not None and body.text_of("Data"):
            by_subject[subject] = (server_id, body.text_of("Data"))
        else:
            by_subject[subject] = (server_id, None)

    for subject in s.injected_subjects:
        expect(subject in by_subject,
               f"SMTP-delivered message {subject!r} is missing from Sync "
               f"(subjects: {sorted(by_subject)})")
        server_id, body = by_subject[subject]
        s.injected_ids[subject] = server_id
        expect(body, f"item {server_id} ({subject!r}) carries no airsyncbase:Body/Data")

    unexpected = [a.text_of("ServerId") for a in adds
                  if a.text_of("ServerId") not in set(s.injected_ids.values())]
    note = f"; also {len(unexpected)} other items" if unexpected else ""
    notes = "; ".join(reply.anomalies[:4])
    return (f"{len(adds)} items, key {key}, "
            f"{len(s.injected_subjects)} delivered messages found"
            + note + (f" [anomalies: {notes}]" if notes else ""))


def _sync_request(s: Session, key: str, get_changes: bool = True,
                  collection_id: str | None = None) -> Node:
    collection = [
        el("AirSync", "SyncKey", key),
        el("AirSync", "CollectionId", collection_id or s.inbox()),
        el("AirSync", "DeletesAsMoves"),
    ]
    if get_changes:
        collection.append(el("AirSync", "GetChanges"))
    collection += [
        el("AirSync", "WindowSize", "512"),
        el("AirSync", "Options",
           el("AirSyncBase", "BodyPreference",
              el("AirSyncBase", "Type", "1"),
              el("AirSyncBase", "TruncationSize", "10240"))),
    ]
    return el("AirSync", "Sync",
              el("AirSync", "Collections",
                 el("AirSync", "Collection", *collection)))


def step_sync_incremental(s: Session) -> str:
    expect(s.sync_key != "0", "no Sync key from the initial sync")
    request = _sync_request(s, s.sync_key)
    reply = s.client.command("Sync", request)
    collection = _collection(reply, "Sync")
    status_in(collection, {1}, "Sync (incremental)")
    key = collection.text_of("SyncKey").strip()
    expect(key, "the incremental Sync returned no SyncKey")
    s.sync_key = key
    commands = collection.find("Commands")
    changed = len(commands.children) if commands is not None else 0
    return f"key {key}, {changed} changes"


def step_sendmail(s: Session) -> str:
    subject = f"linchange e2e {s.nonce} sent"
    message = email.message.EmailMessage()
    message["From"] = s.user
    message["To"] = s.user
    message["Subject"] = subject
    message["Date"] = formatdate(localtime=True)
    message["Message-ID"] = make_msgid(domain="example.com")
    message.set_content("sent from the ActiveSync end-to-end test\n")
    request = el("ComposeMail", "SendMail",
                 el("ComposeMail", "SaveInSentItems"),
                 el("ComposeMail", "Mime", message.as_bytes()))
    reply = s.client.command("SendMail", request)
    expect(reply.status == 200, f"SendMail returned {reply.status}")
    if reply.doc is None:
        expect(not reply.http.body, "SendMail returned a non-WBXML body")
        outcome = "empty 200"
    else:
        expect(reply.doc.name == "SendMail", f"unexpected reply {reply.doc.name}")
        status_in(reply.doc, {1}, "SendMail")
        outcome = f"SendMail/Status {reply.doc.text_of('Status')}"
    s.sent_subject = subject
    return f"accepted ({outcome})"


def step_sync_after_send(s: Session) -> str:
    expect(getattr(s, "sent_subject", None), "SendMail did not run")
    # Sending is asynchronous: the message goes out through the submission
    # path and comes back through delivery a few seconds later. A device
    # waiting for its own message just syncs again, so the step does too.
    deadline = time.monotonic() + 30.0
    while True:
        request = _sync_request(s, s.sync_key)
        reply = s.client.command("Sync", request)
        collection = _collection(reply, "Sync")
        status_in(collection, {1}, "Sync (after send)")
        key = collection.text_of("SyncKey").strip()
        expect(key, "Sync after send returned no SyncKey")
        s.sync_key = key
        commands = collection.find("Commands")
        subjects = []
        if commands is not None:
            for add in commands.findall("Add"):
                subjects.append(add.find("ApplicationData").text_of("Subject")
                                if add.find("ApplicationData") else "")
        if s.sent_subject in subjects:
            return f"the sent message is in the mailbox at key {key}"
        if time.monotonic() > deadline:
            expect(False,
                   f"the message sent over EAS never arrived in the mailbox "
                   f"(subjects: {subjects})")
        time.sleep(2.0)


def step_sendmail_failing(s: Session) -> str:
    """A send that cannot succeed has to fail out loud.

    ActiveSync accepts the message: at the moment it is submitted nobody knows
    the recipients are unreachable. Delivery finds out, and the server owes the
    sender a report naming each recipient that did not get the message and why.
    Two kinds of failure are sent together so the report has to cover both: an
    address on this server's own domain that does not exist, and a domain that
    exists nowhere at all.
    """
    domain = s.user.split("@", 1)[1]
    s.bounce_target = f"nobody-{s.nonce}@{domain}"
    s.bounce_remote = f"nobody-{s.nonce}@linchange-does-not-exist.invalid"
    subject = f"linchange e2e {s.nonce} undeliverable"

    message = email.message.EmailMessage()
    message["From"] = s.user
    message["To"] = s.bounce_target
    message["Cc"] = s.bounce_remote
    message["Subject"] = subject
    message["Date"] = formatdate(localtime=True)
    message["Message-ID"] = make_msgid(domain="example.com")
    message.set_content("this message has no reachable recipients\n")

    request = el("ComposeMail", "SendMail",
                 el("ComposeMail", "SaveInSentItems"),
                 el("ComposeMail", "Mime", message.as_bytes()))
    reply = s.client.command("SendMail", request)
    expect(reply.status == 200, f"SendMail returned {reply.status}")
    if reply.doc is not None:
        status_in(reply.doc, {1}, "SendMail (failing send)")
    s.bounce_subject = subject
    return f"accepted for {s.bounce_target} and {s.bounce_remote}"


def step_bounce_report(s: Session) -> str:
    """The failed send comes back to the sender as a delivery report.

    A bounce that is generated but never delivered is as useless to a user as
    no bounce at all, so the report is read out of the mailbox the same way a
    phone reads it: a Sync of the Inbox.
    """
    expect(getattr(s, "bounce_subject", None), "the failing send did not run")
    deadline = time.monotonic() + 60.0
    while True:
        request = _sync_request(s, s.sync_key)
        reply = s.client.command("Sync", request)
        collection = _collection(reply, "Sync (delivery report)")
        status_in(collection, {1}, "Sync (delivery report)")
        key = collection.text_of("SyncKey").strip()
        expect(key, "Sync for the delivery report returned no SyncKey")
        s.sync_key = key

        reports = []
        commands = collection.find("Commands")
        if commands is not None:
            for add in commands.findall("Add"):
                data = add.find("ApplicationData")
                if data is None:
                    continue
                subject = data.text_of("Subject")
                if "Delivery Status Notification" not in subject:
                    continue
                body = data.find("Body")
                reports.append((subject, body.text_of("Data") if body is not None else ""))

        if reports:
            subject, body = reports[-1]
            expect(s.bounce_target in body,
                   f"the delivery report does not name the recipient that failed "
                   f"({s.bounce_target!r}); body was:\n{body}")
            expect(s.bounce_remote in body,
                   f"the delivery report does not name the unreachable domain "
                   f"({s.bounce_remote!r}); body was:\n{body}")
            expect(s.bounce_subject in body,
                   f"the report does not say which message failed "
                   f"({s.bounce_subject!r}); body was:\n{body}")
            return (f"report in the Inbox ({subject!r}) naming "
                    f"{s.bounce_target} and {s.bounce_remote} at key {key}")

        if time.monotonic() > deadline:
            expect(False, f"no delivery report for the failed send arrived in the "
                         f"Inbox within 60 seconds (sync key {s.sync_key})")
        time.sleep(2.0)


def step_settings(s: Session) -> str:
    request = el("Settings", "Settings",
                 el("Settings", "DeviceInformation",
                    el("Settings", "Set",
                       el("Settings", "Model", "linchange-e2e"),
                       el("Settings", "FriendlyName", "linchange e2e client"),
                       el("Settings", "OS", "Linux"))))
    reply = s.client.command("Settings", request)
    doc = reply.tree("Settings")
    status_in(doc, {1}, "Settings")
    set_node = doc.descendant("Set")
    expect(set_node is not None, "no DeviceInformation/Set in the response")
    status_in(set_node, {1}, "Settings DeviceInformation/Set")
    return "device information stored"


def step_ping(s: Session) -> str:
    request = el("Ping", "Ping",
                 el("Ping", "HeartbeatInterval", "60"),
                 el("Ping", "Folders",
                    el("Ping", "Folder",
                       el("Ping", "Id", s.inbox()),
                       el("Ping", "Class", "Email"))))
    reply = s.client.command("Ping", request)
    doc = reply.tree("Ping")
    status = status_in(doc, {1, 2}, "Ping")
    if status == 2:
        return "changes reported (status 2)"
    return "status 1, no changes"


def step_folder_crud(s: Session) -> str:
    name = f"e2e-{s.nonce}"
    created = el("FolderHierarchy", "FolderCreate",
                 el("FolderHierarchy", "SyncKey", s.folder_key),
                 el("FolderHierarchy", "DisplayName", name),
                 el("FolderHierarchy", "Type", "12"),  # user-created mail folder
                 el("FolderHierarchy", "ParentId", s.inbox()))
    reply = s.client.command("FolderCreate", created)
    doc = reply.tree("FolderCreate")
    status_in(doc, {1}, "FolderCreate")
    server_id = doc.text_of("ServerId").strip()
    expect(server_id, "FolderCreate returned no ServerId")
    key = doc.text_of("SyncKey").strip()
    if key:
        s.folder_key = key

    renamed = el("FolderHierarchy", "FolderUpdate",
                 el("FolderHierarchy", "SyncKey", s.folder_key),
                 el("FolderHierarchy", "ServerId", server_id),
                 el("FolderHierarchy", "DisplayName", f"{name}-renamed"))
    reply = s.client.command("FolderUpdate", renamed)
    doc = reply.tree("FolderUpdate")
    status_in(doc, {1}, "FolderUpdate")
    key = doc.text_of("SyncKey").strip()
    if key:
        s.folder_key = key

    removed = el("FolderHierarchy", "FolderDelete",
                 el("FolderHierarchy", "SyncKey", s.folder_key),
                 el("FolderHierarchy", "ServerId", server_id))
    reply = s.client.command("FolderDelete", removed)
    doc = reply.tree("FolderDelete")
    status_in(doc, {1}, "FolderDelete")
    key = doc.text_of("SyncKey").strip()
    if key:
        s.folder_key = key
    return f"created {server_id!r}, renamed it, deleted it"


def step_sync_sent_folder(s: Session) -> str:
    """The message a client sends over EAS has to appear in its Sent folder.

    This is what a phone does after sending: the Sent collection is synced and
    the copy that was filed is pulled down. A server that stores the copy but
    never lists it makes the folder look empty forever.
    """
    expect(getattr(s, "sent_subject", None), "SendMail did not run")
    expect(s.sent_id, "no Sent ServerId: FolderSync did not report a Sent folder (type 5)")
    request = _sync_request(s, "0", collection_id=s.sent_id)
    reply = s.client.command("Sync", request)
    collection = _collection(reply, "Sync (sent folder)")
    status_in(collection, {1}, "Sync (sent folder)")
    key = collection.text_of("SyncKey").strip()
    expect(key, "Sync of the Sent folder returned no SyncKey")
    commands = collection.find("Commands")
    subjects = []
    if commands is not None:
        for add in commands.findall("Add"):
            subjects.append(add.find("ApplicationData").text_of("Subject")
                            if add.find("ApplicationData") else "")
    expect(s.sent_subject in subjects,
           f"the sent message is stored but does not appear in a Sync of the "
           f"Sent folder (subjects: {subjects})")
    return f"the sent message is in the Sent folder ({s.sent_id}) at key {key}"


def step_calendar_folder_crud(s: Session) -> str:
    """FolderCreate with the calendar type has to create a real calendar.

    MS-ASCMD reserves type 13 for a user-created Calendar folder. A client
    creating one then syncs it as a calendar, so the server has to hand back a
    calendar collection -- one of the cal-prefixed ids -- not a mailbox that
    merely answers to the name.
    """
    name = f"e2e-cal-{s.nonce}"
    created = el("FolderHierarchy", "FolderCreate",
                 el("FolderHierarchy", "SyncKey", s.folder_key),
                 el("FolderHierarchy", "DisplayName", name),
                 el("FolderHierarchy", "Type", "13"),
                 el("FolderHierarchy", "ParentId", "0"))
    reply = s.client.command("FolderCreate", created)
    doc = reply.tree("FolderCreate")
    status_in(doc, {1}, "FolderCreate (calendar)")
    server_id = doc.text_of("ServerId").strip()
    expect(server_id.startswith("cal"),
           f"a calendar FolderCreate must answer with a calendar ServerId, "
           f"got {server_id!r}")
    key = doc.text_of("SyncKey").strip()
    if key:
        s.folder_key = key

    # The new calendar is a working collection: its initial sync succeeds.
    sync = _sync_request(s, "0", collection_id=server_id)
    reply = s.client.command("Sync", sync)
    collection = _collection(reply, "Sync (new calendar)")
    status_in(collection, {1}, "Sync (new calendar)")

    removed = el("FolderHierarchy", "FolderDelete",
                 el("FolderHierarchy", "SyncKey", s.folder_key),
                 el("FolderHierarchy", "ServerId", server_id))
    reply = s.client.command("FolderDelete", removed)
    doc = reply.tree("FolderDelete")
    status_in(doc, {1}, "FolderDelete (calendar)")
    key = doc.text_of("SyncKey").strip()
    if key:
        s.folder_key = key
    return f"created calendar {server_id!r}, synced it, deleted it"


def step_autodiscover_outlook(s: Session) -> str:
    """The probe desktop Outlook 2021 sends while adding an account.

    Outlook does not speak ActiveSync, but its discovery probe is the first
    thing a person setting up an account meets: it posts this XML to
    /autodiscover/autodiscover.xml and reads the server's identity from the
    answer before it decides which protocol to use. A response that does not
    identify the deployment leaves the setup screen with nothing to show.
    """
    import xml.etree.ElementTree as ET
    url = f"{s.client.base}/autodiscover/autodiscover.xml"
    payload = (
        '<?xml version="1.0" encoding="utf-8"?>'
        '<Autodiscover xmlns="http://schemas.microsoft.com/exchange/autodiscover/requestschema/2006">'
        '<Request><EMailAddress>' + s.user + '</EMailAddress>'
        '<AcceptableResponseSchema>'
        'http://schemas.microsoft.com/exchange/autodiscover/responseschema/2006'
        '</AcceptableResponseSchema></Request></Autodiscover>'
    ).encode()
    request = urllib.request.Request(
        url, data=payload, method="POST",
        headers={"Content-Type": "text/xml"})
    try:
        with urllib.request.urlopen(request, timeout=s.client.timeout) as reply:
            status, body = reply.status, reply.read()
    except urllib.error.HTTPError as err:
        status, body = err.code, err.read()
    expect(status == 200,
           f"the Outlook autodiscover probe answered HTTP {status}: "
           f"{body[:200]!r} -- this is what Outlook reports as the account "
           f"failing to be found")
    root = ET.fromstring(body)
    values = {el.tag.rsplit("}", 1)[-1]: (el.text or "").strip()
              for el in root.iter()}
    deployment = values.get("DeploymentId", "")
    expect(deployment == "linchange",
           f"the autodiscover answer does not identify the server: "
           f"DeploymentId={deployment!r}")
    display = values.get("DisplayName", "")
    expect(display, "the autodiscover answer carries no DisplayName to show")
    account = values.get("AccountType", "")
    expect(account == "email", f"AccountType={account!r}, want 'email'")
    return (f"server identified as {deployment!r} ({display}), "
            f"account type {account!r}")


def step_itemoperations_fetch(s: Session) -> str:
    if not s.injected_ids:
        raise Skip("no synced item to fetch")
    server_id = next(iter(s.injected_ids.values()))
    subject = next(iter(s.injected_ids))
    request = el("ItemOperations", "ItemOperations",
                 el("ItemOperations", "Fetch",
                    el("ItemOperations", "Store", "Mailbox"),
                    el("AirSync", "CollectionId", s.inbox()),
                    el("AirSync", "ServerId", server_id),
                    el("ItemOperations", "Options",
                       el("AirSyncBase", "BodyPreference",
                          el("AirSyncBase", "Type", "1"),
                          el("AirSyncBase", "TruncationSize", "10240")))))
    reply = s.client.command("ItemOperations", request)
    doc = reply.tree("ItemOperations")
    status_in(doc, {1}, "ItemOperations")
    fetch = doc.descendant("Fetch")
    expect(fetch is not None, "no Response/Fetch in the ItemOperations answer")
    status_in(fetch, {1}, "ItemOperations Fetch")
    properties = fetch.find("Properties")
    expect(properties is not None, "the fetch carries no Properties")
    expect(properties.text_of("Subject") == subject,
           f"the fetched Subject is {properties.text_of('Subject')!r}, "
           f"expected {subject!r}")
    body = properties.find("Body")
    expect(body is not None and body.text_of("Data"),
           "the fetched item carries no body")
    return f"fetched {server_id} with subject and body"


def step_move_items(s: Session) -> str:
    if not s.injected_ids or not s.trash_id:
        raise Skip("no item or no Trash folder to move between")
    server_id = next(iter(s.injected_ids.values()))
    request = el("Move", "MoveItems",
                 el("Move", "Move",
                    el("Move", "SrcMsgId", server_id),
                    el("Move", "SrcFldId", s.inbox()),
                    el("Move", "DstFldId", s.trash_id)))
    reply = s.client.command("MoveItems", request)
    doc = reply.tree("MoveItems")
    response = doc.descendant("Response")
    expect(response is not None, "no Response in the MoveItems answer")
    expect(response.text_of("SrcMsgId") == server_id,
           f"the response names SrcMsgId {response.text_of('SrcMsgId')!r}, "
           f"expected {server_id!r}")
    status_in(response, {1, 3}, "MoveItems Response")
    s.moved_id = server_id
    return f"moved {server_id} to Trash"


def step_search(s: Session) -> str:
    """Search finds the mail that is in the mailbox, in the folder named.

    A client types a word into its search box and expects the messages that
    contain it back, with their properties. The server used to answer every
    Search with an empty list; this step proves it now reads the query and
    returns what matches, and that a query that matches nothing says so.
    """
    if not s.injected_subjects:
        raise Skip("no delivered messages to search for")
    term = s.injected_subjects[0]

    request = el("Search", "Search",
                 el("Search", "Store",
                    el("Search", "Name", "Mailbox"),
                    el("Search", "Query",
                       el("Search", "And",
                          el("AirSync", "CollectionId", s.inbox()),
                          el("Search", "FreeText", term))),
                    el("Search", "Options",
                       el("Search", "Range", "0-9"))))
    reply = s.client.command("Search", request)
    doc = reply.tree("Search")
    status_in(doc, {1}, "Search")
    store = doc.descendant("Store")
    expect(store is not None, "the Search answer carries no Store (it is "
                             "nested under Response)")
    status_in(store, {1}, "Search Store")
    matched = store.int_at("Total", -1)
    expect(matched >= 1, f"the Search for {term!r} matched nothing (Total {matched})")
    results = store.findall("Result")
    expect(results, f"Total is {matched} but the answer carries no Result elements")
    subject = results[0].text_at("Subject")
    expect(subject == term,
           f"the Search result carries Subject {subject!r}, expected {term!r}")

    # A term nothing contains must be an honest zero, not an error and not a
    # accidental hit.
    request = el("Search", "Search",
                 el("Search", "Store",
                    el("Search", "Name", "Mailbox"),
                    el("Search", "Query",
                       el("Search", "And",
                          el("AirSync", "CollectionId", s.inbox()),
                          el("Search", "FreeText", f"no-such-term-{s.nonce}")))))
    reply = s.client.command("Search", request)
    doc = reply.tree("Search")
    status_in(doc, {1}, "Search (no match)")
    store = doc.descendant("Store")
    status_in(store, {1}, "Search Store (no match)")
    total = store.int_at("Total", 0)
    expect(total == 0, f"a query that matched nothing reported Total {total}")
    return f"found {term!r} (Total {matched}, {len(results)} result(s))"


def _sync_command_collection(s: Session, key: str, collection_id: str,
                             commands: list) -> Node:
    collection = [
        el("AirSync", "SyncKey", key),
        el("AirSync", "CollectionId", collection_id),
        el("AirSync", "GetChanges"),
    ]
    if commands:
        collection.append(el("AirSync", "Commands", *commands))
    return el("AirSync", "Sync",
              el("AirSync", "Collections", el("AirSync", "Collection", *collection)))


def step_sync_delete(s: Session) -> str:
    """Sync Delete moves a message to Trash, and the next sync says so.

    A client that deletes a message expects the server to put it in Trash;
    the server used to ignore deletes outright, so the item came back on the
    next sync. This step deletes an item, checks the sync reports it gone,
    and finds it in the Trash folder.
    """
    if not s.injected_ids or not s.trash_id:
        raise Skip("no synced item or no Trash folder")
    subject = next(sub for sub, sid in s.injected_ids.items()
                   if sid != s.moved_id)
    server_id = s.injected_ids[subject]

    request = _sync_command_collection(
        s, s.sync_key, s.inbox(),
        [el("AirSync", "Delete", el("AirSync", "ServerId", server_id))])
    reply = s.client.command("Sync", request)
    collection = _collection(reply, "Sync (delete)")
    status_in(collection, {1}, "Sync (delete)")
    key = collection.text_of("SyncKey").strip()
    if key:
        s.sync_key = key
    commands = collection.find("Commands")
    deletes = commands.findall("Delete") if commands else []
    expect(any(d.text_of("ServerId") == server_id for d in deletes),
           f"the Inbox sync does not report {server_id} deleted "
           f"(deletes: {[d.text_of('ServerId') for d in deletes]})")

    reply = s.client.command("Sync", _sync_request(s, "0", collection_id=s.trash_id))
    collection = _collection(reply, "Sync (Trash)")
    status_in(collection, {1}, "Sync (Trash)")
    commands = collection.find("Commands")
    adds = commands.findall("Add") if commands else []
    expect(any(a.text_of("ServerId") == server_id for a in adds),
           f"the deleted item {server_id} is not in Trash "
           f"(Trash holds: {[a.text_of('ServerId') for a in adds]})")
    del s.injected_ids[subject]
    return f"deleted {server_id} ({subject!r}) from Inbox; Trash holds it"


def step_calendar_meeting(s: Session) -> str:
    """A Sync Add creates a real event, and MeetingResponse answers it.

    A phone creates an appointment by adding it to a calendar collection,
    and answers a meeting invitation with the UserResponse element
    (MS-ASCMD 2.2.3.100): 1 accept, 2 decline, 3 tentative. The server used
    to read the answer from a nested MeetingResponse element that real
    clients do not send, so every response was silently dropped.
    """
    name = f"e2e-meeting-{s.nonce}"
    created = el("FolderHierarchy", "FolderCreate",
                 el("FolderHierarchy", "SyncKey", s.folder_key),
                 el("FolderHierarchy", "DisplayName", name),
                 el("FolderHierarchy", "Type", "13"),
                 el("FolderHierarchy", "ParentId", "0"))
    reply = s.client.command("FolderCreate", created)
    doc = reply.tree("FolderCreate")
    status_in(doc, {1}, "FolderCreate (meeting calendar)")
    cal_id = doc.text_of("ServerId").strip()
    key = doc.text_of("SyncKey").strip()
    if key:
        s.folder_key = key

    reply = s.client.command("Sync", _sync_request(s, "0", collection_id=cal_id))
    collection = _collection(reply, "Sync (meeting calendar)")
    status_in(collection, {1}, "Sync (meeting calendar)")
    cal_key = collection.text_of("SyncKey").strip()

    subject = f"E2e kickoff {s.nonce}"
    add = el("AirSync", "Add",
             el("AirSync", "ApplicationData",
                el("Calendar", "Subject", subject),
                el("Calendar", "StartTime", "20261115T090000"),
                el("Calendar", "EndTime", "20261115T100000"),
                el("Calendar", "Location", "Room 3")))
    request = _sync_command_collection(s, cal_key, cal_id, [add])
    reply = s.client.command("Sync", request)
    collection = _collection(reply, "Sync (calendar add)")
    status_in(collection, {1}, "Sync (calendar add)")
    commands = collection.find("Commands")
    adds = commands.findall("Add") if commands else []
    expect(adds, "the calendar Sync did not answer with the event it created")
    event_id = adds[0].text_of("ServerId")
    data = adds[0].find("ApplicationData")
    expect(data is not None and data.text_of("Subject") == subject,
           f"the created event carries Subject {data.text_of('Subject')!r}, "
           f"expected {subject!r}")

    response = el("MeetingResponse", "MeetingResponse",
                  el("MeetingResponse", "Request",
                     el("MeetingResponse", "CalendarId", event_id),
                     el("MeetingResponse", "UserResponse", "1")))
    reply = s.client.command("MeetingResponse", response)
    doc = reply.tree("MeetingResponse")
    status_in(doc, {1}, "MeetingResponse")
    result = doc.find("Result")
    expect(result is not None, "MeetingResponse carries no Result")
    status_in(result, {1}, "MeetingResponse Result")
    echoed = result.text_of("CalendarId").strip()
    expect(echoed == event_id,
           f"the MeetingResponse echoed CalendarId {echoed!r}, expected {event_id!r}")

    removed = el("FolderHierarchy", "FolderDelete",
                 el("FolderHierarchy", "SyncKey", s.folder_key),
                 el("FolderHierarchy", "ServerId", cal_id))
    reply = s.client.command("FolderDelete", removed)
    doc = reply.tree("FolderDelete")
    status_in(doc, {1}, "FolderDelete (meeting calendar)")
    key = doc.text_of("SyncKey").strip()
    if key:
        s.folder_key = key
    return (f"created event {event_id} on calendar {cal_id}, accepted it via "
            f"UserResponse, deleted the calendar")


def _autodiscover(user: str, url: str, timeout: float) -> dict:
    body = (
        '<?xml version="1.0" encoding="utf-8"?>'
        '<Autodiscover xmlns="http://schemas.microsoft.com/exchange/autodiscover/request/2006">'
        "<Request>"
        f"<EMailAddress>{user}</EMailAddress>"
        "<AcceptableResponseSchema>"
        "http://schemas.microsoft.com/exchange/autodiscover/responseschema/2006a"
        "</AcceptableResponseSchema>"
        "</Request></Autodiscover>"
    ).encode()
    request = urllib.request.Request(
        url, data=body, method="POST",
        headers={"Content-Type": "text/xml", "User-Agent": "linchange-e2e/1.0"})
    with urllib.request.urlopen(request, timeout=timeout) as reply:
        return {"status": reply.status, "body": reply.read()}


def step_autodiscover(s: Session, user_url: str) -> str:
    """A client that is told where ActiveSync lives must be able to reach it."""
    import xml.etree.ElementTree as ET
    try:
        body = _autodiscover(s.user, user_url, s.client.timeout)
    except (urllib.error.URLError, OSError) as err:
        raise Fail(f"autodiscover could not be reached: {err}") from err
    expect(body["status"] == 200,
           f"autodiscover answered HTTP {body['status']}")
    root = ET.fromstring(body["body"])
    urls = [(el.tag.rsplit("}", 1)[-1], el.text.strip())
            for el in root.iter() if el.tag.rsplit("}", 1)[-1] in ("ASUrl", "EwsUrl")]
    expect(urls, "the autodiscover response advertises no ASUrl or EwsUrl")
    # Two ASUrls exist: EWS's, and ActiveSync's (the EXPR block). An ActiveSync
    # client follows the latter, so that is the one that has to answer.
    eas = [u for tag, u in urls if tag == "ASUrl" and u.rstrip("/").endswith("/eas")]
    probes = eas or [u for tag, u in urls if tag == "ASUrl"]
    for url in probes + [u for tag, u in urls if tag == "EwsUrl"]:
        try:
            probe = urllib.request.Request(url, method="GET",
                                           headers={"Authorization": s.client._auth})
            with urllib.request.urlopen(probe, timeout=s.client.timeout) as reply:
                reach = reply.status
        except urllib.error.HTTPError as err:
            reach = err.code
        except (urllib.error.URLError, OSError, TimeoutError) as err:
            raise Fail(
                f"autodiscover advertises {url} but that endpoint cannot "
                f"be reached ({err}) — this is what a client reports as "
                f"'server not found'") from err
        expect(reach < 500, f"the advertised URL {url} answered {reach}")
    return ", ".join(f"{tag}={url} (reachable)" for tag, url in sorted(urls))


# ---------------------------------------------------------------------------
# Runner
# ---------------------------------------------------------------------------

STEPS = [
    ("OPTIONS negotiation", step_options, True),
    ("service root (GET /eas)", step_service_root, True),
    ("Provision: policy download", step_provision_initial, True),
    ("Provision: policy acknowledgement", step_provision_ack, True),
    ("FolderSync: initial folder list", step_foldersync_initial, True),
    ("FolderSync: incremental", step_foldersync_incremental, True),
    ("SMTP: deliver mail to the mailbox", step_smtp_injection, True),
    ("GetItemEstimate: inbox", step_item_estimate, True),
    ("Sync: initial mailbox contents", step_sync_initial, True),
    ("Sync: incremental", step_sync_incremental, True),
    ("Search: find delivered mail", step_search, True),
    ("SendMail: MIME submission", step_sendmail, True),
    ("Sync: sent mail arrives back", step_sync_after_send, True),
    ("Sync: sent folder holds the sent copy", step_sync_sent_folder, True),
    ("SendMail: a send that cannot succeed", step_sendmail_failing, True),
    ("Delivery report: the bounce reaches the sender", step_bounce_report, True),
    ("Settings: device information", step_settings, True),
    ("Ping: heartbeat", step_ping, True),
    ("FolderCreate/FolderUpdate/FolderDelete", step_folder_crud, True),
    ("FolderCreate: calendar folder", step_calendar_folder_crud, True),
    ("Sync: calendar add and MeetingResponse", step_calendar_meeting, True),
    ("ItemOperations: fetch an item", step_itemoperations_fetch, False),
    ("MoveItems: move to Trash", step_move_items, False),
    ("Sync: delete moves mail to Trash", step_sync_delete, False),
]


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--base-url", default=os.environ.get("EAS_BASE_URL", "http://localhost:58080"))
    parser.add_argument("--user", default=os.environ.get("EAS_USER", "admin@localhost"))
    parser.add_argument("--password", default=os.environ.get("EAS_PASSWORD", "DemoAdmin123!"))
    parser.add_argument("--smtp-host", default=os.environ.get("EAS_SMTP_HOST", "localhost"))
    parser.add_argument("--smtp-port", type=int, default=int(os.environ.get("EAS_SMTP_PORT", "5025")))
    parser.add_argument("--device-id", default="e2e000000000001")
    parser.add_argument("--device-type", default="Windows")
    parser.add_argument("--timeout", type=float, default=30.0)
    args = parser.parse_args()

    steps = list(STEPS)
    steps.append(("autodiscover: advertised endpoint is reachable",
                  lambda s: step_autodiscover(s, args.base_url.rstrip("/") +
                                              "/autodiscover/autodiscover.xml"), True))
    steps.append(("autodiscover: Outlook 2021 identity probe",
                  step_autodiscover_outlook, True))

    print(f"linchange Exchange ActiveSync end-to-end")
    print(f"  target  {args.base_url}")
    print(f"  user    {args.user}   device {args.device_type}/{args.device_id}")
    print(f"  smtp    {args.smtp_host}:{args.smtp_port}")
    print()

    session = Session(args)
    results = []
    for name, run, required in steps:
        try:
            detail = run(session) or ""
            verdict = "PASS"
        except Skip as err:
            detail, verdict = str(err), "SKIP"
        except Fail as err:
            detail, verdict = str(err), "FAIL"
        except Exception as err:  # a crash is a finding too
            detail, verdict = f"unexpected error: {err.__class__.__name__}: {err}", "FAIL"
        if verdict == "FAIL" and not required:
            verdict = "FAIL*"
        results.append((verdict, name, detail))
        print(f"  {verdict:<5} {name}")
        if detail:
            print(f"        {detail}")

    passed = sum(1 for v, _, _ in results if v == "PASS")
    failed = [r for r in results if r[0].startswith("FAIL")]
    print()
    print(f"{passed} passed, {len(failed)} failed, {len(results) - passed - len(failed)} skipped"
          "   (* = advisory step)")
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
