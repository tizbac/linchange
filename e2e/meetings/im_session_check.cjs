// Drives one half of a two-client check against the live OpenIM deployment,
// using a session the linchange server minted. Two phases, one script:
//
//   send:    log in as the organiser, open the meeting's room, send a marker
//            text message. The server acknowledging the send is what this
//            phase asserts - the sender's own SDK reads history
//            from its local cache, which a freshly created conversation has
//            not populated yet, so reading one's own message back immediately
//            would test the cache rather than the room.
//
//   receive: log in as a guest who was a member before this login, let the
//            initial sync pull the room's history, and find the organiser's
//            marker in it. This is the direction a real client reads: server
//            history through the official SDK.
//
// Usage: node im_session_check.cjs <session-file>
//   <session-file> is JSON: { userId, token, wsAddr, apiAddr, groupID,
//   mode: "send"|"receive", text, fromUserID }.
//
// Prints IM-OK on success and exits 0; any failure exits 1 with IM-FAIL and a
// reason, so the calling suite can report without parsing noise.
'use strict';

const fs = require('fs');
const path = require('path');

const repoRoot = path.resolve(__dirname, '..', '..');
const { getSDK, CbEvents, Platform, SessionType } = require(
  path.join(repoRoot, 'web', 'node_modules', '@openim/client-sdk'),
);

function fail(reason) {
  console.error(`IM-FAIL: ${reason}`);
  process.exit(1);
}

const sessionPath = process.argv[2];
if (!sessionPath) fail('usage: im_session_check.cjs <session-file>');
let session;
try {
  session = JSON.parse(fs.readFileSync(sessionPath, 'utf8'));
} catch (err) {
  fail(`reading session file: ${err.message}`);
}
for (const key of ['userId', 'token', 'wsAddr', 'apiAddr', 'groupID', 'mode', 'text']) {
  if (!session[key]) fail(`session file is missing ${key}`);
}

const sdk = getSDK();
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

async function ready() {
  // login() resolves once the handshake is written, not once the socket is
  // up; OnConnectSuccess is the proof the transport is ready, and
  // OnSyncServerFinish is the proof the SDK has pulled its initial state -
  // history read before that sync completes comes back empty, because the
  // SDK asks for a range it does not yet know exists. A timeout rather than
  // a hang: a sync that never finishes is a failure with a reason.
  const done = new Promise((resolve) => {
    const mark = () => resolve('ready');
    sdk.on(CbEvents.OnConnectSuccess, mark);
    sdk.on(CbEvents.OnSyncServerFinish, mark);
    setTimeout(() => resolve('timeout'), 20000);
  });
  await sdk.login({
    userID: session.userId,
    token: session.token,
    wsAddr: session.wsAddr,
    apiAddr: session.apiAddr,
    platformID: Platform.Web,
  });
  const how = await done;
  if (how !== 'ready') fail(`SDK never became ready (${how})`);
}

async function openRoom() {
  const conv = await sdk.getOneConversation({
    sourceID: session.groupID,
    sessionType: SessionType.Group,
  });
  if (!conv || conv.errCode !== 0 || !conv.data || !conv.data.conversationID) {
    fail(`getOneConversation: errCode=${conv && conv.errCode} errMsg=${conv && conv.errMsg}`);
  }
  return conv.data.conversationID;
}

async function send() {
  const created = await sdk.createTextMessage(session.text);
  if (!created || created.errCode !== 0 || !created.data) {
    fail(`createTextMessage: errCode=${created && created.errCode}`);
  }
  const sent = await sdk.sendMessage({
    recvID: '',
    groupID: session.groupID,
    message: created.data,
  });
  if (!sent || sent.errCode !== 0) {
    fail(`sendMessage: errCode=${sent && sent.errCode} errMsg=${sent && sent.errMsg}`);
  }
  if (!sent.data || !sent.data.clientMsgID) {
    fail(`the send did not return a message identity: ${JSON.stringify(sent.data)}`);
  }
  // The ack carries the SDK's local copy of the message, whose seq is only
  // filled in by the sync that the receive phase reads, so the cross-client
  // proof of the room is the guest finding this text in server history.
  console.log(`IM-OK sent clientMsgID=${sent.data.clientMsgID}`);
}

async function receive(conversationID) {
  // The guest's login sync pulls the room's history into the SDK cache; the
  // advanced history read then answers from it. Poll briefly so a sync that
  // lands a moment after the event does not read as a failure.
  for (let attempt = 0; attempt < 8; attempt++) {
    const history = await sdk.getAdvancedHistoryMessageList({
      conversationID,
      count: 50,
      startClientMsgID: '',
    });
    const list = (history && history.data && history.data.messageList) || [];
    const found = list.find(
      (m) => m.textElem && m.textElem.content === session.text && m.sendID === session.fromUserID,
    );
    if (found) {
      console.log(`IM-OK received seq=${found.seq}`);
      return;
    }
    await sleep(1000);
  }
  fail(`the organiser's marker never reached the guest's history`);
}

async function main() {
  await ready();
  const conversationID = await openRoom();
  if (session.mode === 'send') {
    await send();
  } else {
    await receive(conversationID);
  }
  try {
    await sdk.logout();
  } catch {
    // The check is over; a failed logout is not a failed check.
  }
  process.exit(0);
}

main().catch((err) => {
  const readable =
    err && typeof err === 'object'
      ? `errCode=${err.errCode} errMsg=${err.errMsg}`
      : String(err);
  fail(readable);
});
