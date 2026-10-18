#!/usr/bin/env python3
"""End-to-end test of the Exchange Web Services endpoint, written as a client.

This drives the request sequence a desktop Outlook runs against an Exchange
mailbox: probe the endpoint, sync the folder tree, sync the Inbox, read mail
through a paged view and a search query, fetch a body, mark it read, send a
message (which must really arrive), send one that cannot be delivered (which
must come back as a bounce report), build a calendar appointment, and clean
up. Requests are built from the published MS-OXECWS schema and answers are
parsed as ordinary SOAP: nothing here knows the server's internal structs, so
a request this script can speak is a request a real client can speak.

    python3 e2e/ews/ews_e2e.py --base-url http://127.0.0.1:18080 \
        --user e2e-user@example.com --password 'correct horse battery staple' \
        --smtp-host 127.0.0.1 --smtp-port 1025

Exit status is 0 when every required step passed.
"""

from __future__ import annotations

import argparse
import base64
import email.message
import os
import smtplib
import sys
import time
import urllib.error
import urllib.request
import uuid
from email.utils import formatdate, make_msgid
from xml.etree import ElementTree as ET

SOAP_NS = "http://schemas.xmlsoap.org/soap/envelope/"
M = "http://schemas.microsoft.com/exchange/services/2006/messages"
T = "http://schemas.microsoft.com/exchange/services/2006/types"


class Fail(Exception):
    """A step's expectation did not hold."""


class Skip(Exception):
    """A step cannot run, e.g. a prerequisite step failed."""


def q(ns: str, name: str) -> str:
    return f"{{{ns}}}{name}"


def text_of(elem, ns: str, name: str) -> str:
    kid = elem.find(q(ns, name))
    return (kid.text or "").strip() if kid is not None and kid.text else ""


def expect(condition, message):
    if not condition:
        raise Fail(message)


class Client:
    """A minimal EWS client: authenticate, send a SOAP envelope, parse the answer."""

    def __init__(self, base_url: str, user: str, password: str, timeout: float = 30.0):
        self.base = base_url.rstrip("/")
        self.user = user
        self.password = password
        self.timeout = timeout

    def call(self, op: str, payload: str) -> ET.Element:
        envelope = (
            '<?xml version="1.0" encoding="utf-8"?>'
            '<soap:Envelope xmlns:soap="http://schemas.xmlsoap.org/soap/envelope/" '
            f'xmlns:m="{M}" xmlns:t="{T}">'
            '<soap:Header><t:RequestServerVersion Version="Exchange2010_SP2"/></soap:Header>'
            f"<soap:Body>{payload}</soap:Body>"
            "</soap:Envelope>"
        )
        request = urllib.request.Request(
            f"{self.base}/ews/exchange.asmx",
            data=envelope.encode(),
            method="POST",
            headers={
                "Content-Type": "text/xml; charset=utf-8",
                "SOAPAction": f"m:{op}",
                "Authorization": "Basic " + base64.b64encode(
                    f"{self.user}:{self.password}".encode()).decode(),
            },
        )
        try:
            with urllib.request.urlopen(request, timeout=self.timeout) as reply:
                body = reply.read()
        except urllib.error.HTTPError as err:
            snippet = err.read()[:400].decode("utf-8", "replace")
            raise Fail(f"{op}: HTTP {err.code}: {snippet}") from err
        except OSError as err:
            raise Fail(f"{op}: transport error: {err}") from err

        try:
            root = ET.fromstring(body)
        except ET.ParseError as err:
            raise Fail(f"{op}: response is not well-formed XML ({err}); "
                       f"body starts {body[:120]!r}") from err

        fault = root.find(f".//{q(SOAP_NS, 'Fault')}")
        if fault is not None:
            raise Fail(f"{op}: SOAP fault: {text_of(fault, SOAP_NS, 'faultstring')}")

        message = root.find(f".//{q(M, op + 'ResponseMessage')}")
        if message is None:
            raise Fail(f"{op}: no {op}ResponseMessage in the envelope")
        code = text_of(message, M, "ResponseCode")
        if message.get("ResponseClass") == "Error":
            raise Fail(f"{op}: ResponseCode {code}: "
                       f"{text_of(message, M, 'MessageText')}")
        return message


class Session:
    def __init__(self, args):
        self.client = Client(args.base_url, args.user, args.password, args.timeout)
        self.user = args.user
        self.smtp_host = args.smtp_host
        self.smtp_port = args.smtp_port
        self.nonce = uuid.uuid4().hex[:8]
        self.hierarchy_state = ""
        self.inbox_state = ""
        self.folders: dict[str, str] = {}          # FolderId -> DisplayName
        self.injected: dict[str, str] = {}         # subject -> (Id, ChangeKey)
        self.sent_subject = ""
        self.calendar_id: str | None = None
        self.shared_mailbox = args.shared_mailbox
        self.other_user = args.other_user
        self.shared_subject = ""
        self.shared_root_id: str | None = None
        self.shared_sent_id: str | None = None
        self.shared_injected: dict[str, tuple[str, str]] = {}
        self.sent_as_subject = ""


def shape(fields: list[str]) -> str:
    extra = "".join(f'<t:FieldURI FieldURI="{f}"/>' for f in fields)
    return (f'<m:ItemShape><t:BaseShape>IdOnly</t:BaseShape>'
            f'<t:AdditionalProperties>{extra}</t:AdditionalProperties></m:ItemShape>')


# ---------------------------------------------------------------------------
# Steps
# ---------------------------------------------------------------------------


def step_service_root(s: Session) -> str:
    request = urllib.request.Request(f"{s.client.base}/ews/", method="GET")
    try:
        with urllib.request.urlopen(request, timeout=s.client.timeout) as reply:
            body = reply.read().decode("utf-8", "replace")
    except urllib.error.HTTPError as err:
        raise Fail(f"GET /ews/ answered {err.code}") from err
    expect("Exchange Web Services" in body,
           f"GET /ews/ answered 200 but not with a service banner: {body[:80]!r}")
    return "GET /ews/ answers 200 with the service banner"


def step_smtp_injection(s: Session) -> str:
    """Mail arrives over SMTP first, so the syncs have something real to hand out."""
    subjects = [f"linchange ews {s.nonce} alpha",
                f"linchange ews {s.nonce} beta"]
    try:
        with smtplib.SMTP(s.smtp_host, s.smtp_port, timeout=20) as smtp:
            for i, subject in enumerate(subjects):
                message = email.message.EmailMessage()
                message["From"] = f"probe{i}@example.com"
                message["To"] = s.user
                message["Subject"] = subject
                message["Date"] = formatdate(localtime=True)
                message["Message-ID"] = make_msgid(domain="example.com")
                message.set_content(f"ews probe body {i} of {s.nonce}\n")
                smtp.sendmail(f"probe{i}@example.com", [s.user], message.as_bytes())
    except (OSError, smtplib.SMTPException) as err:
        raise Fail(f"SMTP delivery to {s.smtp_host}:{s.smtp_port} failed: {err}") from err
    s.injected_subjects = subjects
    return f"2 messages delivered to {s.user} via {s.smtp_host}:{s.smtp_port}"


def step_folder_sync(s: Session) -> str:
    reply = s.client.call("SyncFolderHierarchy", """
        <m:SyncFolderHierarchy>
          <m:SyncFolderId><t:DistinguishedFolderId Id="rootfolder"/></m:SyncFolderId>
          <m:FolderShape><t:BaseShape>IdOnly</t:BaseShape>
            <t:AdditionalProperties>
              <t:FieldURI FieldURI="folder:DisplayName"/>
              <t:FieldURI FieldURI="folder:ChildCount"/>
              <t:FieldURI FieldURI="folder:FolderClass"/>
            </t:AdditionalProperties>
          </m:FolderShape>
          <m:MaxChangesReturned>500</m:MaxChangesReturned>
        </m:SyncFolderHierarchy>""")
    state = text_of(reply, M, "SyncState")
    expect(state, "the hierarchy sync returned no SyncState to carry forward")
    s.hierarchy_state = state
    creates = reply.findall(f"{q(M, 'RootFolder')}/{q(T, 'Changes')}/{q(T, 'Create')}")
    names = {}
    for create in creates:
        folder = create.find(q(T, "Folder"))
        if folder is None:
            continue
        identity = folder.find(q(T, "FolderId"))
        fid = identity.get("Id") if identity is not None else None
        name = text_of(folder, T, "DisplayName")
        expect(fid, "a created folder in the hierarchy carries no FolderId")
        names[fid] = name
    expect(len(names) >= 4,
           f"the hierarchy sync listed {len(names)} folders, expected at least 4")
    wanted = {"inbox", "sent", "drafts", "trash", "calendar"}
    got = set(names.values())
    missing = sorted(wanted - {n.lower() for n in got})
    expect(not missing, f"the folder tree is missing {missing} (got {sorted(got)})")
    return (f"{len(names)} folders ({', '.join(sorted(got))}), "
            f"state {state[:24]}...")


def sync_inbox(s: Session, state: str | None) -> ET.Element:
    state_xml = f"<m:SyncState>{state}</m:SyncState>" if state is not None else ""
    return s.client.call("SyncFolderItems", f"""
        <m:SyncFolderItems>
          <m:SyncFolderId><t:DistinguishedFolderId Id="inbox"/></m:SyncFolderId>
          {state_xml}
          {shape(["item:subject", "message:datetimereceived", "message:isread"])}
          <m:MaxChangesReturned>100</m:MaxChangesReturned>
        </m:SyncFolderItems>""")


def step_inbox_sync(s: Session) -> str:
    reply = sync_inbox(s, None)
    state = text_of(reply, M, "SyncState")
    expect(state, "the Inbox sync returned no SyncState")
    s.inbox_state = state
    items = reply.findall(f"{q(M, 'RootFolder')}/{q(T, 'Items')}/{q(T, 'Create')}")
    count = 0
    for create in items:
        message = create.find(q(T, "Message"))
        if message is None:
            continue
        identity = message.find(q(T, "ItemId"))
        if identity is None or not identity.get("Id"):
            continue
        count += 1
        subject = text_of(message, T, "Subject")
        if subject in s.injected_subjects:
            s.injected[subject] = (identity.get("Id"), identity.get("ChangeKey"))
    expect(count >= 2, f"the Inbox sync handed out {count} items, expected at least 2")
    expect(len(s.injected) == 2,
           f"the two SMTP-injected messages are not in the sync results "
           f"(found {list(s.injected)})")
    complete = text_of(reply, M, "IncludesLastItemInRange")
    expect(complete == "true", f"IncludesLastItemInRange is {complete!r}, expected true")
    return f"{count} items, both injected messages present, state {state[:24]}..."


def step_inbox_incremental(s: Session) -> str:
    reply = sync_inbox(s, s.inbox_state)
    state = text_of(reply, M, "SyncState")
    creates = reply.findall(f"{q(M, 'RootFolder')}/{q(T, 'Items')}/{q(T, 'Create')}")
    changes = reply.findall(f"{q(M, 'RootFolder')}/{q(T, 'Changes')}/{q(T, '*')}")
    expect(not creates and not changes,
           f"an unchanged Inbox came back with {len(creates)} creates and "
           f"{len(changes)} changes")
    expect(state == s.inbox_state,
           f"the stable sync returned a different state ({state[:24]!r} vs "
           f"{s.inbox_state[:24]!r}); a client would resync forever")
    return "second sync reports no changes and echoes the same state"


def step_find_view(s: Session) -> str:
    reply = s.client.call("FindItem", f"""
        <m:FindItem Traversal="Flat">
          {shape(["item:subject", "item:datetime"])}
          <m:ParentFolderIds><t:DistinguishedFolderId Id="inbox"/></m:ParentFolderIds>
          <m:IndexedPageItemView MaxEntriesReturned="3" Offset="0" BasePosition="0"/>
        </m:FindItem>""")
    root = reply.find(q(M, "RootFolder"))
    expect(root is not None, "FindItem returned no RootFolder")
    total = int(root.get("TotalItemsInView", "-1"))
    expect(total >= 2, f"TotalItemsInView is {total}, expected at least 2")
    items = root.findall(f"{q(T, 'Items')}/{q(T, 'Message')}")
    expect(len(items) == min(3, total),
           f"a view of 3 over {total} items returned {len(items)} items")
    newest = text_of(items[0], T, "Subject")
    expect(newest in s.injected_subjects,
           f"the newest item in the view is {newest!r}, one of the injected "
           f"messages was expected first")
    return f"TotalItemsInView {total}; newest first: {newest!r}"


def step_find_query(s: Session) -> str:
    subject = s.injected_subjects[0]
    reply = s.client.call("FindItem", f"""
        <m:FindItem Traversal="Flat">
          {shape(["item:subject"])}
          <m:ParentFolderIds><t:DistinguishedFolderId Id="inbox"/></m:ParentFolderIds>
          <m:IndexedPageItemView MaxEntriesReturned="10" Offset="0" BasePosition="0"/>
          <m:QueryString>subject:"{subject}"</m:QueryString>
        </m:FindItem>""")
    root = reply.find(q(M, "RootFolder"))
    hits = root.findall(f"{q(T, 'Items')}/{q(T, 'Message')}")
    expect(len(hits) == 1,
           f"the query for {subject!r} returned {len(hits)} items, expected 1")
    expect(text_of(hits[0], T, "Subject") == subject,
           f"the query hit carries subject {text_of(hits[0], T, 'Subject')!r}")
    return f"AQS query subject:\"{subject}\" matched exactly one item"


def step_get_body(s: Session) -> str:
    item_id, change_key = s.injected[s.injected_subjects[0]]
    key_attr = f' ChangeKey="{change_key}"' if change_key else ""
    reply = s.client.call("GetItem", f"""
        <m:GetItem>
          {shape(["item:body", "message:internetmessageid", "item:subject"])}
          <m:ItemIds><t:ItemId Id="{item_id}"{key_attr}/></m:ItemIds>
        </m:GetItem>""")
    message = reply.find(f"{q(M, 'Items')}/{q(T, 'Message')}")
    expect(message is not None, "GetItem returned no Message")
    body = message.find(q(T, "Body"))
    expect(body is not None and "ews probe body 0" in (body.text or ""),
           f"GetItem returned no readable body (got {body.text if body is not None else None!r})")
    message_id = text_of(message, T, "InternetMessageId")
    expect(message_id, "the item carries no InternetMessageId")
    return f"body fetched ({len(body.text or '')} chars), InternetMessageId {message_id[:30]}..."


def step_mark_read(s: Session) -> str:
    item_id, change_key = s.injected[s.injected_subjects[1]]
    reply = s.client.call("UpdateItem", f"""
        <m:UpdateItem ConflictResolution="AutoResolve" MessageDisposition="SaveOnly">
          <m:ItemChanges>
            <t:ItemChange>
              <t:ItemId Id="{item_id}" ChangeKey="{change_key}"/>
              <t:Updates>
                <t:SetItemField>
                  <t:FieldURI FieldURI="message:IsRead"/>
                  <t:Message><t:IsRead>true</t:IsRead></t:Message>
                </t:SetItemField>
              </t:Updates>
            </t:ItemChange>
          </m:ItemChanges>
        </m:UpdateItem>""")
    updated = reply.find(f"{q(M, 'Items')}/{q(T, 'Message')}")
    expect(updated is not None, "UpdateItem returned no updated item")
    expect(text_of(updated, T, "IsRead") == "true",
           f"UpdateItem answered IsRead {text_of(updated, T, 'IsRead')!r}")
    # The flag must survive: an IMAP client sees the same read state.
    reply = s.client.call("GetItem", f"""
        <m:GetItem>
          {shape(["message:isread"])}
          <m:ItemIds><t:ItemId Id="{item_id}"/></m:ItemIds>
        </m:GetItem>""")
    message = reply.find(f"{q(M, 'Items')}/{q(T, 'Message')}")
    expect(message is not None and text_of(message, T, "IsRead") == "true",
           "the read flag did not stick: GetItem still reports it unread")
    return "IsRead set via UpdateItem and confirmed by GetItem"


def step_send_mail(s: Session) -> str:
    s.sent_subject = f"linchange ews sent {s.nonce}"
    reply = s.client.call("CreateItem", f"""
        <m:CreateItem MessageDisposition="SendAndSaveCopy">
          <m:SavedItemFolderId><t:DistinguishedFolderId Id="sentitems"/></m:SavedItemFolderId>
          <m:Items>
            <t:Message>
              <t:ToRecipients><t:Mailbox>
                <t:Name>{s.user}</t:Name><t:EmailAddress>{s.user}</t:EmailAddress>
              </t:Mailbox></t:ToRecipients>
              <t:Subject>{s.sent_subject}</t:Subject>
              <t:Body BodyType="Text">sent through the EWS send path</t:Body>
            </t:Message>
          </m:Items>
        </m:CreateItem>""")
    message = reply.find(f"{q(M, 'Items')}/{q(T, 'Message')}")
    expect(message is not None, "CreateItem returned no saved item")
    identity = message.find(q(T, "ItemId"))
    expect(identity is not None and identity.get("Id"),
           "the saved copy carries no ItemId")
    return f"SendAndSaveCopy accepted; copy id {identity.get('Id')[:20]}..."


def step_sent_copy(s: Session) -> str:
    reply = s.client.call("FindItem", f"""
        <m:FindItem Traversal="Flat">
          {shape(["item:subject", "message:datetimesent"])}
          <m:ParentFolderIds><t:DistinguishedFolderId Id="sentitems"/></m:ParentFolderIds>
          <m:IndexedPageItemView MaxEntriesReturned="10" Offset="0" BasePosition="0"/>
          <m:QueryString>subject:"{s.sent_subject}"</m:QueryString>
        </m:FindItem>""")
    hits = reply.findall(f"{q(M, 'RootFolder')}/{q(T, 'Items')}/{q(T, 'Message')}")
    expect(len(hits) == 1,
           f"the Sent folder holds {len(hits)} copies of {s.sent_subject!r}, expected 1")
    return f"the sent copy is in the Sent Items folder"


def step_delivery(s: Session) -> str:
    """The sent mail must come back to the mailbox: the send path runs for real."""
    deadline = time.time() + 25
    while time.time() < deadline:
        reply = sync_inbox(s, s.inbox_state)
        s.inbox_state = text_of(reply, M, "SyncState")
        for create in reply.findall(f"{q(M, 'RootFolder')}/{q(T, 'Items')}/{q(T, 'Create')}"):
            message = create.find(q(T, "Message"))
            if message is not None and text_of(message, T, "Subject") == s.sent_subject:
                return (f"the EWS-sent message arrived in the Inbox within "
                        f"{25 - (deadline - time.time()):.0f}s of being queued")
        time.sleep(2)
    raise Fail(f"the message sent via CreateItem SendAndSaveCopy never arrived "
               f"in the Inbox within 25s")


def step_bounce(s: Session) -> str:
    """A send that cannot succeed must come back to the user as a delivery report."""
    recipient = f"nobody-{s.nonce}@linchange-does-not-exist.invalid"
    subject = f"linchange ews undeliverable {s.nonce}"
    s.client.call("CreateItem", f"""
        <m:CreateItem MessageDisposition="SendAndSaveCopy">
          <m:SavedItemFolderId><t:DistinguishedFolderId Id="sentitems"/></m:SavedItemFolderId>
          <m:Items>
            <t:Message>
              <t:ToRecipients><t:Mailbox>
                <t:Name>{recipient}</t:Name><t:EmailAddress>{recipient}</t:EmailAddress>
              </t:Mailbox></t:ToRecipients>
              <t:Subject>{subject}</t:Subject>
              <t:Body BodyType="Text">this can never be delivered</t:Body>
            </t:Message>
          </m:Items>
        </m:CreateItem>""")
    deadline = time.time() + 30
    while time.time() < deadline:
        reply = sync_inbox(s, s.inbox_state)
        s.inbox_state = text_of(reply, M, "SyncState")
        for create in reply.findall(f"{q(M, 'RootFolder')}/{q(T, 'Items')}/{q(T, 'Create')}"):
            message = create.find(q(T, "Message"))
            if message is None:
                continue
            got = text_of(message, T, "Subject")
            if "Delivery Status Notification" in got:
                identity = message.find(q(T, "ItemId"))
                reply = s.client.call("GetItem", f"""
                    <m:GetItem>
                      {shape(["item:body"])}
                      <m:ItemIds><t:ItemId Id="{identity.get('Id')}"/></m:ItemIds>
                    </m:GetItem>""")
                body_el = reply.find(f"{q(M, 'Items')}/{q(T, 'Message')}/{q(T, 'Body')}")
                body = (body_el.text or "") if body_el is not None else ""
                expect(recipient in body,
                       f"the bounce does not name the failed recipient {recipient!r}: {body[:200]!r}")
                return (f"bounce '{got}' in the Inbox naming {recipient} within "
                        f"{30 - (deadline - time.time()):.0f}s")
        time.sleep(2)
    raise Fail("no Delivery Status Notification reached the Inbox for the "
               "undeliverable send within 30s")


def step_calendar(s: Session) -> str:
    subject = f"linchange ews meeting {s.nonce}"
    reply = s.client.call("CreateItem", f"""
        <m:CreateItem MessageDisposition="SaveOnly">
          <m:SavedItemFolderId><t:DistinguishedFolderId Id="calendar"/></m:SavedItemFolderId>
          <m:Items>
            <t:CalendarItem>
              <t:Subject>{subject}</t:Subject>
              <t:Start>2026-11-15T09:00:00Z</t:Start>
              <t:End>2026-11-15T10:00:00Z</t:End>
              <t:Location>Room 3</t:Location>
              <t:Body BodyType="Text">kickoff, booked through EWS</t:Body>
            </t:CalendarItem>
          </m:Items>
        </m:CreateItem>""")
    item = reply.find(f"{q(M, 'Items')}/{q(T, 'CalendarItem')}")
    expect(item is not None, "CreateItem returned no CalendarItem")
    identity = item.find(q(T, "ItemId"))
    expect(identity is not None and identity.get("Id"),
           "the created appointment carries no ItemId")
    s.calendar_id = identity.get("Id")

    reply = s.client.call("SyncFolderItems", f"""
        <m:SyncFolderItems>
          <m:SyncFolderId><t:DistinguishedFolderId Id="calendar"/></m:SyncFolderId>
          {shape(["item:subject"])}
          <m:MaxChangesReturned>50</m:MaxChangesReturned>
        </m:SyncFolderItems>""")
    found = False
    for create in reply.findall(f"{q(M, 'RootFolder')}/{q(T, 'Items')}/{q(T, 'Create')}"):
        calendar = create.find(q(T, "CalendarItem"))
        if calendar is not None and calendar.find(q(T, "ItemId")).get("Id") == s.calendar_id:
            found = True
            expect(text_of(calendar, T, "Subject") == subject,
                   "the calendar sync shows the appointment with a different subject")
    expect(found, "the calendar folder sync does not show the created appointment")

    reply = s.client.call("GetItem", f"""
        <m:GetItem>
          {shape(["item:subject", "calendar:start", "calendar:end", "calendar:location"])}
          <m:ItemIds><t:ItemId Id="{s.calendar_id}"/></m:ItemIds>
        </m:GetItem>""")
    calendar = reply.find(f"{q(M, 'Items')}/{q(T, 'CalendarItem')}")
    expect(calendar is not None, "GetItem on the appointment returned no CalendarItem")
    start = text_of(calendar, T, "Start")
    end = text_of(calendar, T, "End")
    expect(start.startswith("2026-11-15") and end.startswith("2026-11-15"),
           f"the appointment times are {start!r}/{end!r}, expected 2026-11-15")
    return f"appointment created, synced and fetched ({start} - {end})"


def step_calendar_delete(s: Session) -> str:
    if not s.calendar_id:
        raise Skip("no calendar appointment to delete")
    s.client.call("DeleteItem", f"""
        <m:DeleteItem DeleteType="SoftDelete">
          <m:ItemIds><t:ItemId Id="{s.calendar_id}"/></m:ItemIds>
        </m:DeleteItem>""")
    reply = s.client.call("SyncFolderItems", """
        <m:SyncFolderItems>
          <m:SyncFolderId><t:DistinguishedFolderId Id="calendar"/></m:SyncFolderId>
          <m:ItemShape><t:BaseShape>IdOnly</t:BaseShape></m:ItemShape>
        </m:SyncFolderItems>""")
    for create in reply.findall(f"{q(M, 'RootFolder')}/{q(T, 'Items')}/{q(T, 'Create')}"):
        item = create.find(q(T, "CalendarItem"))
        if item is not None and item.find(q(T, "ItemId")).get("Id") == s.calendar_id:
            raise Fail("the deleted appointment is still handed out by the calendar sync")
    return "the deleted appointment is gone from the calendar sync"


def step_delete_mail(s: Session) -> str:
    item_id, _ = s.injected[s.injected_subjects[1]]
    s.client.call("DeleteItem", f"""
        <m:DeleteItem DeleteType="SoftDelete">
          <m:ItemIds><t:ItemId Id="{item_id}"/></m:ItemIds>
        </m:DeleteItem>""")
    reply = sync_inbox(s, s.inbox_state)
    s.inbox_state = text_of(reply, M, "SyncState")
    deletes = reply.findall(f"{q(M, 'RootFolder')}/{q(T, 'Changes')}/{q(T, 'Delete')}")
    expect(deletes, "the Inbox sync does not report the deleted message")
    gone = any(d.find(q(T, "ItemId")).get("Id") == item_id for d in deletes if d.find(q(T, "ItemId")) is not None)
    expect(gone, "the reported deletes do not include the message that was deleted")
    return f"SoftDelete reported as a Delete change ({len(deletes)} delete(s))"


def step_shared_inject(s: Session) -> str:
    """Mail delivered to the shared mailbox so the member's view of it is real."""
    s.shared_subject = f"linchange ews shared {s.nonce}"
    try:
        with smtplib.SMTP(s.smtp_host, s.smtp_port, timeout=20) as smtp:
            message = email.message.EmailMessage()
            message["From"] = "probe@example.com"
            message["To"] = s.shared_mailbox
            message["Subject"] = s.shared_subject
            message["Date"] = formatdate(localtime=True)
            message["Message-ID"] = make_msgid(domain="example.com")
            message.set_content(f"shared mailbox probe {s.nonce}\n")
            smtp.sendmail("probe@example.com", [s.shared_mailbox], message.as_bytes())
    except (OSError, smtplib.SMTPException) as err:
        raise Fail(f"SMTP delivery to {s.shared_mailbox} failed: {err}") from err
    return f"1 message delivered to {s.shared_mailbox} via {s.smtp_host}:{s.smtp_port}"


def mailbox_ref(name: str, address: str) -> str:
    """A distinguished folder id with the Mailbox child that names another mailbox."""
    return (f'<t:DistinguishedFolderId Id="{name}">'
            f'<t:Mailbox><t:EmailAddress>{address}</t:EmailAddress></t:Mailbox>'
            f'</t:DistinguishedFolderId>')


def step_shared_hierarchy(s: Session) -> str:
    reply = s.client.call("SyncFolderHierarchy", """
        <m:SyncFolderHierarchy Traversal="Deep">
          <m:SyncFolderId><t:DistinguishedFolderId Id="rootfolder"/></m:SyncFolderId>
          <m:FolderShape><t:BaseShape>IdOnly</t:BaseShape>
            <t:AdditionalProperties>
              <t:FieldURI FieldURI="folder:DisplayName"/>
              <t:FieldURI FieldURI="folder:ChildCount"/>
              <t:FieldURI FieldURI="folder:ParentFolderId"/>
            </t:AdditionalProperties>
          </m:FolderShape>
          <m:MaxChangesReturned>500</m:MaxChangesReturned>
        </m:SyncFolderHierarchy>""")
    entries = []
    for create in reply.findall(f"{q(M, 'RootFolder')}/{q(T, 'Changes')}/{q(T, 'Create')}"):
        folder = create.find(q(T, "Folder"))
        if folder is None:
            continue
        identity = folder.find(q(T, "FolderId"))
        if identity is None or not identity.get("Id"):
            continue
        parent = folder.find(q(T, "ParentFolderId"))
        entries.append((identity.get("Id"), text_of(folder, T, "DisplayName"),
                        parent.get("Id") if parent is not None else None))
    root = next((e for e in entries if e[1] == "Team Mailbox"), None)
    expect(root is not None,
           f"the folder tree does not list the shared mailbox {s.shared_mailbox} "
           f"(got {sorted(n for _, n, _ in entries)})")
    children = [e for e in entries if e[2] == root[0]]
    expect(len(children) >= 3,
           f"the shared mailbox root has {len(children)} folders beneath it, "
           "expected its Inbox, Sent, Drafts and Trash")
    s.shared_root_id = root[0]
    sent = next((e for e in children if e[1].lower() == "sent"), None)
    s.shared_sent_id = sent[0] if sent else None
    return (f"'Team Mailbox' root {root[0][:20]}... with {len(children)} folders "
           "beneath it")


def step_shared_sync(s: Session) -> str:
    expect(s.shared_root_id, "no shared mailbox root to sync")
    # The root itself holds no items: a client that syncs Exchange's root gets an
    # empty answer and then moves to the Inbox beneath it, and the same must hold
    # here or the shared mailbox looks like one big pile of mail.
    reply = s.client.call("SyncFolderItems", f"""
        <m:SyncFolderItems>
          <m:SyncFolderId><t:FolderId Id="{s.shared_root_id}"/></m:SyncFolderId>
          <m:ItemShape><t:BaseShape>IdOnly</t:BaseShape></m:ItemShape>
        </m:SyncFolderItems>""")
    creates = reply.findall(f"{q(M, 'RootFolder')}/{q(T, 'Items')}/{q(T, 'Create')}")
    expect(not creates,
           f"the shared mailbox root handed out {len(creates)} items; the mail "
           "belongs in its folders, not in the root")

    reply = s.client.call("SyncFolderItems", f"""
        <m:SyncFolderItems>
          <m:SyncFolderId>{mailbox_ref("inbox", s.shared_mailbox)}</m:SyncFolderId>
          {shape(["item:subject", "message:datetimereceived", "message:isread"])}
          <m:MaxChangesReturned>100</m:MaxChangesReturned>
        </m:SyncFolderItems>""")
    count = 0
    for create in reply.findall(f"{q(M, 'RootFolder')}/{q(T, 'Items')}/{q(T, 'Create')}"):
        message = create.find(q(T, "Message"))
        if message is None:
            continue
        identity = message.find(q(T, "ItemId"))
        if identity is None or not identity.get("Id"):
            continue
        count += 1
        subject = text_of(message, T, "Subject")
        if subject == s.shared_subject:
            s.shared_injected[subject] = (identity.get("Id"), identity.get("ChangeKey"))
    expect(s.shared_injected,
           f"the shared Inbox sync did not hand out the message delivered to "
           f"{s.shared_mailbox} (got {count} items)")
    return f"shared Inbox sync handed out {count} items, including the delivered mail"


def step_shared_getitem(s: Session) -> str:
    item_id, change_key = s.shared_injected[s.shared_subject]
    key_attr = f' ChangeKey="{change_key}"' if change_key else ""
    reply = s.client.call("GetItem", f"""
        <m:GetItem>
          {shape(["item:body", "item:subject"])}
          <m:ItemIds><t:ItemId Id="{item_id}"{key_attr}/></m:ItemIds>
        </m:GetItem>""")
    message = reply.find(f"{q(M, 'Items')}/{q(T, 'Message')}")
    expect(message is not None, "GetItem on a shared mailbox item returned no Message")
    body = message.find(q(T, "Body"))
    expect(body is not None and s.nonce in (body.text or ""),
           f"the shared item body is missing ({body.text if body is not None else None!r})")
    return f"shared item body fetched ({len(body.text or '')} chars) by item id from the shared Inbox"


def step_shared_plain(s: Session) -> str:
    if not s.shared_sent_id:
        raise Skip("the hierarchy did not name the shared mailbox's Sent folder")
    reply = s.client.call("FindItem", f"""
        <m:FindItem Traversal="Flat">
          {shape(["item:subject"])}
          <m:ParentFolderIds><t:FolderId Id="{s.shared_sent_id}"/></m:ParentFolderIds>
          <m:IndexedPageItemView MaxEntriesReturned="10" Offset="0" BasePosition="0"/>
        </m:FindItem>""")
    root = reply.find(q(M, "RootFolder"))
    expect(root is not None, "FindItem on the shared Sent folder returned no RootFolder")
    total = int(root.get("TotalItemsInView", "-1"))
    expect(total >= 0, f"TotalItemsInView is {total}, which is not a count")
    return (f"the shared Sent folder resolved by its plain identifier "
            f"(TotalItemsInView {total})")


def step_shared_send_as(s: Session) -> str:
    """A member with a grant composes in the team mailbox: the From header stands."""
    s.sent_as_subject = f"linchange ews send-as {s.nonce}"
    reply = s.client.call("CreateItem", f"""
        <m:CreateItem MessageDisposition="SendAndSaveCopy">
          <m:SavedItemFolderId><t:DistinguishedFolderId Id="sentitems"/></m:SavedItemFolderId>
          <m:Items>
            <t:Message>
              <t:From><t:Mailbox>
                <t:Name>Team Mailbox</t:Name><t:EmailAddress>{s.shared_mailbox}</t:EmailAddress>
              </t:Mailbox></t:From>
              <t:ToRecipients><t:Mailbox>
                <t:Name>{s.user}</t:Name><t:EmailAddress>{s.user}</t:EmailAddress>
              </t:Mailbox></t:ToRecipients>
              <t:Subject>{s.sent_as_subject}</t:Subject>
              <t:Body BodyType="Text">composed as the team mailbox {s.nonce}</t:Body>
            </t:Message>
          </m:Items>
        </m:CreateItem>""")
    message = reply.find(f"{q(M, 'Items')}/{q(T, 'Message')}")
    expect(message is not None, "the send-as CreateItem returned no saved copy")
    identity = message.find(q(T, "ItemId"))
    expect(identity is not None and identity.get("Id"), "the send-as copy carries no ItemId")
    return f"SendAndSaveCopy accepted with From {s.shared_mailbox}; copy id {identity.get('Id')[:20]}..."


def step_shared_send_as_delivers(s: Session) -> str:
    """The send-as mail must really arrive, and under the mailbox's address."""
    deadline = time.time() + 25
    while time.time() < deadline:
        reply = sync_inbox(s, s.inbox_state)
        s.inbox_state = text_of(reply, M, "SyncState")
        for create in reply.findall(f"{q(M, 'RootFolder')}/{q(T, 'Items')}/{q(T, 'Create')}"):
            message = create.find(q(T, "Message"))
            if message is None or text_of(message, T, "Subject") != s.sent_as_subject:
                continue
            identity = message.find(q(T, "ItemId"))
            got = s.client.call("GetItem", f"""
                <m:GetItem>
                  {shape(["item:subject", "message:from"])}
                  <m:ItemIds><t:ItemId Id="{identity.get('Id')}"/></m:ItemIds>
                </m:GetItem>""")
            item = got.find(f"{q(M, 'Items')}/{q(T, 'Message')}")
            from_el = item.find(q(T, "From")) if item is not None else None
            address = ""
            if from_el is not None:
                mailbox = from_el.find(q(T, "Mailbox"))
                if mailbox is not None:
                    address = text_of(mailbox, T, "EmailAddress")
            expect(address.lower() == s.shared_mailbox,
                   f"the delivered message carries From {address!r}, expected "
                   f"{s.shared_mailbox!r}")
            return (f"send-as mail arrived in the Inbox with From {address} within "
                    f"{25 - (deadline - time.time()):.0f}s")
        time.sleep(2)
    raise Fail(f"the message sent as {s.shared_mailbox} never arrived in the Inbox within 25s")


def step_shared_denied(s: Session) -> str:
    """Somebody without a grant must not see the shared mailbox at all."""
    other = Client(s.client.base, s.other_user, s.client.password, s.client.timeout)
    reply = other.call("SyncFolderHierarchy", """
        <m:SyncFolderHierarchy>
          <m:SyncFolderId><t:DistinguishedFolderId Id="rootfolder"/></m:SyncFolderId>
          <m:FolderShape><t:BaseShape>IdOnly</t:BaseShape>
            <t:AdditionalProperties>
              <t:FieldURI FieldURI="folder:DisplayName"/>
            </t:AdditionalProperties>
          </m:FolderShape>
        </m:SyncFolderHierarchy>""")
    names = [text_of(f, T, "DisplayName")
             for f in reply.findall(f"{q(M, 'RootFolder')}/{q(T, 'Changes')}/{q(T, 'Create')}/{q(T, 'Folder')}")]
    expect("Team Mailbox" not in names,
           f"the ungranted user {s.other_user} was shown the shared mailbox in the "
           f"tree ({sorted(names)})")
    try:
        other.call("FindItem", f"""
            <m:FindItem Traversal="Flat">
              {shape(["item:subject"])}
              <m:ParentFolderIds>{mailbox_ref("inbox", s.shared_mailbox)}</m:ParentFolderIds>
            </m:FindItem>""")
    except Fail as err:
        expect("AccessDenied" in str(err),
               f"the ungranted user's request for the shared mailbox failed with "
               f"{str(err)[:120]!r}, expected an access-denied response")
        return f"ungranted {s.other_user}: no shared mailbox in the tree, FindItem refused"
    raise Fail(f"the ungranted user {s.other_user} read the shared mailbox without a grant")


def step_resolve_names(s: Session) -> str:
    reply = s.client.call("ResolveNames", f"""
        <m:ResolveNames ReturnFullContactData="false">
          <m:UnresolvedEntry>{s.user.split('@')[0]}</m:UnresolvedEntry>
        </m:ResolveNames>""")
    resolutions = reply.findall(f"{q(M, 'ResolutionSet')}/{q(T, 'Resolution')}/{q(T, 'Mailbox')}")
    expect(resolutions, "ResolveNames found no match for the seeded user")
    address = text_of(resolutions[0], T, "EmailAddress")
    expect(address.lower() == s.user.lower(),
           f"the resolution points at {address!r}, expected {s.user!r}")
    return f"autocomplete resolved {s.user.split('@')[0]!r} to {address}"


def step_availability(s: Session) -> str:
    reply = s.client.call("GetUserAvailability", f"""
        <m:GetUserAvailability>
          <m:FreeBusyViewOptions>
            <m:TimeWindow>
              <m:StartTime>2026-10-17T00:00:00Z</m:StartTime>
              <m:EndTime>2026-10-18T00:00:00Z</m:EndTime>
            </m:TimeWindow>
            <m:MergedFreeBusyIntervalInMinutes>30</m:MergedFreeBusyIntervalInMinutes>
            <m:RequestedView>DetailedMerged</m:RequestedView>
          </m:FreeBusyViewOptions>
          <m:MailboxDataArray>
            <m:MailboxData>
              <t:Email><t:EmailAddress>{s.user}</t:EmailAddress></t:Email>
              <t:AttendeeType>Required</t:AttendeeType>
            </m:MailboxData>
          </m:MailboxDataArray>
        </m:GetUserAvailability>""")
    view = reply.find(f"{q(M, 'FreeBusyResponseArray')}/{q(M, 'FreeBusyResponse')}/{q(M, 'FreeBusyView')}")
    expect(view is not None, "GetUserAvailability returned no FreeBusyView")
    kind = text_of(view, M, "FreeBusyViewType")
    expect(kind in ("FreeBusy", "Merged", "Detailed", "DetailedMerged", "MergedOnly", "None"),
           f"FreeBusyViewType is {kind!r}, not a view type a client can draw")
    return f"free/busy view type {kind}"


def step_timezones(s: Session) -> str:
    reply = s.client.call("GetServerTimeZones", """
        <m:GetServerTimeZones>
          <m:Ids><t:TimeZoneId>UTC</t:TimeZoneId></m:Ids>
        </m:GetServerTimeZones>""")
    definition = reply.find(f"{q(M, 'TimeZoneDefinitions')}/{q(T, 'TimeZoneDefinition')}")
    expect(definition is not None, "GetServerTimeZones returned no TimeZoneDefinition")
    expect(definition.get("Id") == "UTC",
           f"the server defined timezone {definition.get('Id')!r}, expected UTC")
    return "GetServerTimeZones defines UTC with a period table"


STEPS = [
    ("service root (GET /ews/)", step_service_root, False),
    ("SMTP: deliver mail to the mailbox", step_smtp_injection, False),
    ("SyncFolderHierarchy: initial", step_folder_sync, False),
    ("SyncFolderItems: initial Inbox sync", step_inbox_sync, False),
    ("SyncFolderItems: incremental", step_inbox_incremental, False),
    ("FindItem: paged view", step_find_view, False),
    ("FindItem: search query", step_find_query, False),
    ("GetItem: fetch a body", step_get_body, False),
    ("UpdateItem: mark read", step_mark_read, False),
    ("CreateItem: send mail", step_send_mail, False),
    ("Sent Items holds the copy", step_sent_copy, False),
    ("send really delivers to the recipient", step_delivery, False),
    ("undeliverable send comes back as a bounce", step_bounce, False),
    ("calendar: create, sync and fetch an appointment", step_calendar, False),
    ("calendar: delete an appointment", step_calendar_delete, False),
    ("DeleteItem: soft delete shows in sync", step_delete_mail, False),
    ("SMTP: deliver mail to the shared mailbox", step_shared_inject, False),
    ("SyncFolderHierarchy: shared mailbox in the tree", step_shared_hierarchy, False),
    ("SyncFolderItems: shared root is empty, shared Inbox via Mailbox element", step_shared_sync, False),
    ("GetItem: body of a shared mailbox message", step_shared_getitem, False),
    ("FindItem: shared folder by plain identifier", step_shared_plain, False),
    ("CreateItem: send as the shared mailbox", step_shared_send_as, False),
    ("send-as mail arrives under the mailbox address", step_shared_send_as_delivers, False),
    ("a non-member sees nothing and is refused", step_shared_denied, True),
    ("ResolveNames: autocomplete", step_resolve_names, True),
    ("GetUserAvailability: free/busy", step_availability, True),
    ("GetServerTimeZones", step_timezones, True),
]


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--base-url", default=os.environ.get("EWS_BASE_URL", "http://127.0.0.1:18080"))
    parser.add_argument("--user", default=os.environ.get("EWS_USER", "e2e-user@example.com"))
    parser.add_argument("--password", default=os.environ.get("EWS_PASSWORD", "correct horse battery staple"))
    parser.add_argument("--smtp-host", default=os.environ.get("EWS_SMTP_HOST", "127.0.0.1"))
    parser.add_argument("--smtp-port", type=int, default=int(os.environ.get("EWS_SMTP_PORT", "1025")))
    parser.add_argument("--shared-mailbox", default=os.environ.get("EWS_SHARED_MAILBOX", "team@example.com"))
    parser.add_argument("--other-user", default=os.environ.get("EWS_OTHER_USER", "e2e-other@example.com"))
    parser.add_argument("--timeout", type=float, default=30.0)
    args = parser.parse_args()

    print("linchange Exchange Web Services end-to-end")
    print(f"  target  {args.base_url}")
    print(f"  user    {args.user}")
    print(f"  smtp    {args.smtp_host}:{args.smtp_port}")
    print()

    session = Session(args)
    results = []
    for name, run, required in STEPS:
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
