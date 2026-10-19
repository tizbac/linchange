import { Injectable, signal } from '@angular/core';
import {
  CbEvents,
  LoginStatus,
  MessageType,
  Platform,
  SessionType,
  getSDK,
  type CallbackEvent,
  type MessageItem,
} from '@openim/client-sdk';
import type { IMSession } from './meetings.models';

// getSDK() is typed as the SDK's method surface; the instance also carries its
// own identity state at runtime, which that narrow type leaves out. The fields
// are what an "already logged in as this account" check reads.
type SDK = ReturnType<typeof getSDK> & {
  readonly userID?: string;
  readonly loginStatus: LoginStatus;
};

// ChatState is what the room page shows where the conversation would be.
// 'unavailable' is a deployment without an OpenIM backend (the minting
// endpoint answered 503) - the invitation itself is fine, only the room is
// missing, and the page says exactly that.
export type ChatState = 'idle' | 'connecting' | 'connected' | 'unavailable' | 'error';

// OpenIMService owns the one OpenIM SDK instance for the tab and the single
// open meeting room.
//
// The SDK is a singleton by design (getSDK()), so this service is the only
// code allowed to talk to it: two pages logging in and out in turn would kick
// each other offline. The identity comes from the server on every open - a
// token the server minted, never one the browser assembled itself.
@Injectable({ providedIn: 'root' })
export class OpenIMService {
  private readonly sdk = getSDK() as SDK;

  readonly state = signal<ChatState>('idle');
  readonly messages = signal<MessageItem[]>([]);
  readonly selfId = signal('');
  readonly error = signal('');

  private groupID = '';
  private conversationID = '';
  private connectWaiters: Array<() => void> = [];
  private eventsBound = false;
  private opening: Promise<void> | null = null;

  // --- events ---

  // The handlers are bound once and filter against whatever room is open, so
  // switching meetings never leaves a stale listener appending to a list.
  private readonly handleNewMessage = (
    env: CallbackEvent<MessageItem>,
  ): void => {
    const msg = env.data;
    if (!msg || !this.groupID) return;
    if (msg.groupID && msg.groupID !== this.groupID) return;
    this.append(msg);
  };

  private readonly handleConnectSuccess = (): void => {
    this.state.set('connected');
    const waiters = this.connectWaiters;
    this.connectWaiters = [];
    for (const wake of waiters) wake();
  };

  private readonly handleConnectFailed = (): void => {
    this.state.set('error');
    this.error.set('The chat server could not be reached.');
    const waiters = this.connectWaiters;
    this.connectWaiters = [];
    for (const wake of waiters) wake();
  };

  private bindEvents(): void {
    if (this.eventsBound) return;
    this.eventsBound = true;
    this.sdk.on(CbEvents.OnRecvNewMessage, this.handleNewMessage);
    this.sdk.on(CbEvents.OnConnectSuccess, this.handleConnectSuccess);
    this.sdk.on(CbEvents.OnConnectFailed, this.handleConnectFailed);
  }

  // --- lifecycle ---

  // open logs the account in (once per account) and loads the meeting's room.
  // Concurrent callers share one opening: a double-click on "Open chat" is
  // not a reason to log in twice.
  async open(session: IMSession, groupID: string): Promise<void> {
    this.groupID = groupID;
    this.state.set('connecting');
    this.error.set('');
    if (this.opening) {
      await this.opening;
      return;
    }
    this.opening = this.doOpen(session);
    try {
      await this.opening;
    } finally {
      this.opening = null;
    }
  }

  private async doOpen(session: IMSession): Promise<void> {
    this.bindEvents();
    this.selfId.set(session.userId);

    const alreadyIn = this.sdk.userID === session.userId && this.sdk.loginStatus === LoginStatus.Logged;
    if (!alreadyIn) {
      const connected = this.waitForConnect(8000);
      await this.sdk.login({
        userID: session.userId,
        token: session.token,
        wsAddr: session.wsAddr,
        apiAddr: session.apiAddr,
        platformID: Platform.Web,
      });
      // login() settles once the handshake is written, not once the socket is
      // open; the first API call after it would race the connection. The wait
      // times out rather than hangs: a socket that never opens is reported by
      // OnConnectFailed and the error state says so.
      await connected;
    }
    if (this.state() === 'error') {
      return;
    }
    this.state.set('connected');

    const conv = await this.sdk.getOneConversation({
      sourceID: this.groupID,
      sessionType: SessionType.Group,
    });
    this.conversationID = conv.data.conversationID;
    await this.loadHistory();
    // Clearing the unread badge is decoration. On a room opened for the first
    // time the conversation is frequently not in the SDK's synced cache yet
    // ("conversation not exist"), and a badge is no reason to declare a
    // working chat broken.
    try {
      await this.sdk.markConversationMessageAsRead(this.conversationID);
    } catch {
      // The room reads fine whether or not the badge clears.
    }
  }

  private waitForConnect(timeoutMs: number): Promise<void> {
    return new Promise<void>((resolve) => {
      const deadline = setTimeout(resolve, timeoutMs);
      this.connectWaiters.push(() => {
        clearTimeout(deadline);
        resolve();
      });
    });
  }

  private async loadHistory(): Promise<void> {
    const res = await this.sdk.getAdvancedHistoryMessageList({
      conversationID: this.conversationID,
      count: 100,
      startClientMsgID: '',
    });
    const list = res.data?.messageList ?? [];
    // The server answers newest first; a conversation reads upward. Sorting
    // by the server clock rather than trusting the order keeps a resend from
    // a client with a skewed clock in its place anyway.
    const ascending = [...list].sort(
      (a, b) => a.sendTime - b.sendTime || a.createTime - b.createTime || a.clientMsgID.localeCompare(b.clientMsgID),
    );
    this.messages.set(ascending);
  }

  // send posts one line into the open room and shows it immediately: waiting
  // for a round trip to see your own message makes a chat feel broken.
  async send(text: string): Promise<void> {
    const body = text.trim();
    if (!body || !this.groupID) return;
    const created = await this.sdk.createTextMessage(body);
    const sent = await this.sdk.sendMessage({
      recvID: '',
      groupID: this.groupID,
      message: created.data,
    });
    this.append(sent.data ?? created.data);
  }

  // close logs this tab out of OpenIM. Called when the room page goes away:
  // a connection nobody is watching is a connection that will be kicked when
  // the token is next rotated.
  async close(): Promise<void> {
    this.groupID = '';
    this.conversationID = '';
    this.messages.set([]);
    if (this.sdk.loginStatus === LoginStatus.Logged) {
      try {
        await this.sdk.logout();
      } catch {
        // Already gone - the server dropping a session first is normal.
      }
    }
    this.state.set('idle');
  }

  private append(msg: MessageItem): void {
    if (!msg?.clientMsgID) return;
    this.messages.update((list) =>
      list.some((m) => m.clientMsgID === msg.clientMsgID) ? list : [...list, msg],
    );
  }
}

// messageText is a message as the room shows it. Only text carries words;
// anything else is announced by a placeholder rather than rendered as an
// empty bubble, so a reader knows something arrived that this page does not
// display.
export function messageText(msg: MessageItem): string {
  switch (msg.contentType) {
    case MessageType.TextMessage:
      return msg.textElem?.content ?? msg.content ?? '';
    case MessageType.AtTextMessage:
      return msg.atTextElem?.text ?? msg.textElem?.content ?? '';
    case MessageType.OANotification:
      return msg.notificationElem?.detail ?? '';
    default:
      return '';
  }
}

// messagePlaceholder names what arrived when there is no text to show.
export function messagePlaceholder(msg: MessageItem): string {
  if (messageText(msg)) return '';
  switch (msg.contentType) {
    case MessageType.PictureMessage:
      return 'a picture';
    case MessageType.VoiceMessage:
      return 'a voice message';
    case MessageType.VideoMessage:
      return 'a video';
    case MessageType.FileMessage:
      return 'a file';
    case MessageType.FaceMessage:
      return 'a sticker';
    case MessageType.OANotification:
    case MessageType.GroupCreated:
      return '';
    default:
      return 'a message this page does not display';
  }
}

// sdkError reads the failure the SDK rejects with: the same envelope the API
// resolves with, not an Error, so String(err) would print "[object Object]"
// at the reader and tell them nothing.
export function sdkError(err: unknown): string {
  if (err && typeof err === 'object') {
    const env = err as { errCode?: number; errMsg?: string; message?: string };
    if (env.errMsg) {
      return env.errCode ? `${env.errCode}: ${env.errMsg}` : env.errMsg;
    }
    if (env.message) return env.message;
  }
  return String(err);
}
