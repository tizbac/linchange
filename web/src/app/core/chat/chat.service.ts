import { HttpClient } from '@angular/common/http';
import { Injectable, inject, signal } from '@angular/core';
import { firstValueFrom } from 'rxjs';
import { AuthService } from '../auth/auth.service';

// The API lives on the same origin in production and is proxied by the dev
// server, so a relative path is all the app ever needs.
const API = '/api/v1';

export type ChatState = 'idle' | 'connecting' | 'connected' | 'error';

// ChatAccess is the optional invitation identity for a guest with no account.
// When it is set, the socket and history use the token-gated meeting routes
// instead of a signed-in session.
export interface ChatAccess {
  guestUid: string;
  guestTo: string;
  guestToken: string;
}

export interface ChatMessage {
  id: number;
  sender: string;
  body: string;
  createdAt: string;
  // clientId is set on a message this tab sent, so the optimistic copy can be
  // replaced by the stored one rather than shown twice.
  clientId?: string;
}

// ChatSignal is a WebRTC signalling frame relayed by the chat socket.
export interface ChatSignal {
  type: string;
  conversation: string;
  from: string;
  member?: string;
  peers?: string[];
  sdp?: unknown;
  candidate?: unknown;
  allowed?: boolean;
  speaker?: string;
  hands?: string[];
}

interface InboundFrame {
  type: string;
  conversation?: string;
  message?: ChatMessage;
  member?: string;
  from?: string;
  messageId?: number;
  peers?: string[];
  clientId?: string;
  sdp?: unknown;
  candidate?: unknown;
  allowed?: boolean;
  speaker?: string;
  hands?: string[];
}

// ChatService owns this tab's chat socket and the conversation it is showing.
//
// One conversation at a time is all a room needs, and keeping it that way makes
// the lifecycle obvious: opening replaces the connection, closing ends it. The
// durable history is fetched over REST when a conversation opens, so the socket
// only carries what happens from then on.
@Injectable({ providedIn: 'root' })
export class ChatService {
  private readonly http = inject(HttpClient);
  private readonly auth = inject(AuthService);

  readonly state = signal<ChatState>('idle');
  readonly error = signal('');
  // self is the address this tab speaks as: the signed-in user, or the guest
  // named by an invitation link.
  readonly self = signal('');
  readonly messages = signal<ChatMessage[]>([]);
  // typing holds the address of whoever is typing, or an empty string.
  readonly typing = signal('');
  // receipts maps a member to the highest message id they have read.
  readonly receipts = signal<Record<string, number>>({});

  private socket?: WebSocket;
  private conversation = '';
  private access?: ChatAccess;
  private closedByUs = false;
  private retry = 0;
  private retryTimer?: ReturnType<typeof setTimeout>;
  private typingTimer?: ReturnType<typeof setTimeout>;
  private lastTypingSent = 0;
  private readonly signalHandlers = new Set<(signal: ChatSignal) => void>();

  // open shows one conversation: it loads the history already stored and then
  // connects the socket for what happens next. It resolves once the socket is
  // open, so a caller that needs to send immediately (the call, before its
  // first offer) knows the channel is ready.
  async open(conversation: string, access?: ChatAccess): Promise<void> {
    this.close();
    this.conversation = conversation;
    this.access = access;
    this.self.set((access?.guestTo || this.auth.user() || '').toLowerCase());
    this.closedByUs = false;
    this.receipts.set({});
    this.messages.set([]);
    await this.loadHistory();
    try {
      await this.connect();
    } catch {
      // The state and error signals already say what happened; the caller can
      // read them rather than handling a second copy of the failure.
    }
  }

  // isOpenFor reports whether this tab already holds a live socket for the
  // conversation. Re-opening would drop the connection and, with it, any call
  // the tab is in, so callers check first.
  isOpenFor(conversation: string): boolean {
    return this.state() === 'connected' && this.conversation === conversation;
  }

  close(): void {
    this.closedByUs = true;
    this.conversation = '';
    this.access = undefined;
    if (this.retryTimer) {
      clearTimeout(this.retryTimer);
      this.retryTimer = undefined;
    }
    if (this.typingTimer) {
      clearTimeout(this.typingTimer);
      this.typingTimer = undefined;
    }
    this.socket?.close();
    this.socket = undefined;
    this.state.set('idle');
  }

  send(body: string): void {
    const text = body.trim();
    if (!text || !this.ready()) {
      return;
    }
    const clientId = newClientId();
    // The optimistic copy makes the line appear immediately; the stored copy
    // replaces it by clientId when the server echoes the message.
    const pending: ChatMessage = {
      id: 0,
      sender: this.self() || this.auth.user(),
      body: text,
      createdAt: new Date().toISOString(),
      clientId,
    };
    this.messages.update((list) => [...list, pending]);
    this.frame({ type: 'send', conversation: this.conversation, body: text, clientId });
  }

  // notifyTyping tells the room somebody is writing. It is throttled, so a
  // held-down key does not become a packet per character.
  notifyTyping(): void {
    const now = Date.now();
    if (now - this.lastTypingSent < 2000 || !this.ready()) {
      return;
    }
    this.lastTypingSent = now;
    this.frame({ type: 'typing', conversation: this.conversation });
  }

  markRead(messageID: number): void {
    if (!this.ready() || messageID <= 0) {
      return;
    }
    this.frame({ type: 'read', conversation: this.conversation, messageId: messageID });
  }

  // sendSignal relays one WebRTC frame to another member of the room.
  sendSignal(type: string, to: string, payload: { sdp?: unknown; candidate?: unknown }): void {
    if (!this.ready()) {
      return;
    }
    this.frame({ type, conversation: this.conversation, to, ...payload });
  }

  // sendControl sends a call-control frame: raising a hand, granting the
  // floor, and the like. They carry no media, only room policy.
  sendControl(type: string, payload: Record<string, unknown> = {}): void {
    if (!this.ready()) {
      return;
    }
    this.frame({ type, conversation: this.conversation, ...payload });
  }

  // onSignal registers a handler for relayed WebRTC frames and returns the
  // function that unregisters it.
  onSignal(handler: (signal: ChatSignal) => void): () => void {
    this.signalHandlers.add(handler);
    return () => this.signalHandlers.delete(handler);
  }

  // --- internals ---

  private ready(): boolean {
    return this.socket?.readyState === WebSocket.OPEN;
  }

  private async loadHistory(): Promise<void> {
    try {
      const url = this.access
        ? `${API}/meeting-invites/${encodeURIComponent(this.access.guestUid)}/chat?` +
          new URLSearchParams({ to: this.access.guestTo, token: this.access.guestToken }).toString()
        : `${API}/chat/conversations/${encodeURIComponent(this.conversation)}/messages`;
      const res = await firstValueFrom(this.http.get<{ messages: ChatMessage[] }>(url));
      this.messages.set(res.messages ?? []);
    } catch {
      // A history failure is not fatal: the socket can still deliver what
      // happens next, and the room should not refuse to open over it.
    }
  }

  private connect(): Promise<void> {
    const token = this.auth.state()?.accessToken ?? '';
    if ((!token && !this.access) || !this.conversation) {
      this.state.set('error');
      this.error.set(this.access ? 'This invitation link is no longer valid.' : 'Sign in again to open the chat.');
      return Promise.reject(new Error('not authorised'));
    }

    this.state.set('connecting');
    const scheme = window.location.protocol === 'https:' ? 'wss' : 'ws';
    const url = this.access
      ? `${scheme}://${window.location.host}${API}/meeting-invites/${encodeURIComponent(this.access.guestUid)}/ws?` +
        new URLSearchParams({ to: this.access.guestTo, token: this.access.guestToken }).toString()
      : `${scheme}://${window.location.host}${API}/chat/ws?access_token=${encodeURIComponent(token)}`;

    const socket = new WebSocket(url);
    this.socket = socket;

    return new Promise<void>((resolve, reject) => {
      let settled = false;
      socket.onopen = () => {
        this.retry = 0;
        this.error.set('');
        this.state.set('connected');
        settled = true;
        resolve();
      };
      socket.onmessage = (event) => {
        try {
          this.receive(JSON.parse(event.data) as InboundFrame);
        } catch {
          // A frame this client cannot read is ignored rather than closing a
          // working socket.
        }
      };
      socket.onclose = () => {
        this.socket = undefined;
        if (this.closedByUs) {
          if (!settled) reject(new Error('closed'));
          return;
        }
        this.state.set('error');
        this.error.set('The chat connection dropped. Reconnecting…');
        this.scheduleReconnect();
        if (!settled) reject(new Error('closed before it opened'));
      };
    });
  }

  private scheduleReconnect(): void {
    if (this.retryTimer || this.closedByUs) {
      return;
    }
    // 1s, 2s, 4s, then a steady 8s: quick enough that a blip is unnoticed,
    // slow enough that a server that is down is not hammered.
    const delay = Math.min(8000, 1000 * 2 ** this.retry);
    this.retry += 1;
    this.retryTimer = setTimeout(() => {
      this.retryTimer = undefined;
      if (!this.closedByUs) {
        // Reload the history as well: a reconnect may have missed messages.
        void this.loadHistory();
        void this.connect().catch(() => undefined);
      }
    }, delay);
  }

  private receive(frame: InboundFrame): void {
    if (isSignalFrame(frame.type)) {
      const signal: ChatSignal = {
        type: frame.type,
        conversation: frame.conversation ?? '',
        from: frame.from ?? '',
        member: frame.member,
        peers: frame.peers,
        sdp: frame.sdp,
        candidate: frame.candidate,
        allowed: frame.allowed,
        speaker: frame.speaker,
        hands: frame.hands,
      };
      for (const handler of this.signalHandlers) {
        handler(signal);
      }
      return;
    }

    // Frames for another room are ignored; the socket carries every room the
    // member belongs to.
    if (frame.conversation && frame.conversation !== this.conversation) {
      return;
    }

    switch (frame.type) {
      case 'message': {
        const message = frame.message;
        if (!message) {
          return;
        }
        this.messages.update((list) => {
          const pendingAt = frame.clientId
            ? list.findIndex((m) => m.clientId && m.clientId === frame.clientId)
            : -1;
          if (pendingAt >= 0) {
            const next = [...list];
            next[pendingAt] = message;
            return next;
          }
          if (list.some((m) => m.id === message.id)) {
            return list;
          }
          return [...list, message];
        });
        // Seeing a message is reading it.
        this.markRead(message.id);
        break;
      }
      case 'typing':
        if (frame.member && frame.member !== this.self().toLowerCase()) {
          this.typing.set(frame.member);
          if (this.typingTimer) {
            clearTimeout(this.typingTimer);
          }
          this.typingTimer = setTimeout(() => this.typing.set(''), 3000);
        }
        break;
      case 'read':
        if (frame.member && typeof frame.messageId === 'number') {
          this.receipts.update((current) => ({
            ...current,
            [frame.member as string]: Math.max(current[frame.member as string] ?? 0, frame.messageId as number),
          }));
        }
        break;
      case 'error':
        this.error.set('The chat request was refused.');
        break;
      default:
        // hello and anything newer than this client are ignored.
        break;
    }
  }

  private frame(payload: Record<string, unknown>): void {
    if (!this.ready()) {
      return;
    }
    this.socket?.send(JSON.stringify(payload));
  }
}

// isSignalFrame reports whether a frame belongs to the call rather than the
// transcript. WebRTC signalling and the webinar's hand/floor frames all go to
// the same listeners.
function isSignalFrame(type: string): boolean {
  return (
    type.startsWith('rtc-') ||
    type === 'hand-raise' ||
    type === 'hand-lower' ||
    type === 'allow-speak' ||
    type === 'speaker' ||
    type === 'hands' ||
    type === 'screen-start' ||
    type === 'screen-stop'
  );
}

function newClientId(): string {
  // crypto.randomUUID is available in every browser this app supports; the
  // fallback keeps a non-secure context (plain HTTP on a LAN) working too.
  if (typeof crypto !== 'undefined' && 'randomUUID' in crypto) {
    return crypto.randomUUID();
  }
  return `${Date.now()}-${Math.random().toString(16).slice(2)}`;
}
