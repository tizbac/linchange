import { NgClass } from '@angular/common';
import { HttpErrorResponse } from '@angular/common/http';
import {
  ChangeDetectionStrategy,
  Component,
  computed,
  effect,
  inject,
  signal,
  viewChild,
  type ElementRef,
  OnDestroy,
  OnInit,
} from '@angular/core';
import { FormsModule } from '@angular/forms';
import { ActivatedRoute, RouterLink } from '@angular/router';
import { AuthService } from '../core/auth/auth.service';
import { ChatAccess, ChatMessage, ChatService, ChatSignal } from '../core/chat/chat.service';
import { errorMessage } from '../core/http-error';
import { MeetingsApi } from '../core/meetings/meetings-api';
import { Meeting } from '../core/meetings/meetings.models';
import { formatWhen } from '../core/meetings/meetings.ui';

interface CallParticipant {
  member: string;
  self: boolean;
  stream: MediaStream;
}

@Component({
  selector: 'app-call-room',
  standalone: true,
  imports: [NgClass, FormsModule, RouterLink],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `<main class="flex h-screen flex-col bg-slate-950 text-slate-100">
  @if (loading()) {
    <div class="flex flex-1 items-center justify-center">
      <p class="text-sm text-slate-400">Joining the meeting…</p>
    </div>
  } @else if (pageError()) {
    <div class="flex flex-1 items-center justify-center p-6">
      <div class="max-w-md rounded-2xl border border-white/10 bg-slate-900 p-8 text-center">
        <p class="text-sm text-slate-300">{{ pageError() }}</p>
        <a
          [routerLink]="meeting() ? ['/meetings', meeting()!.uid] : ['/']"
          [queryParams]="guestParams()"
          class="mt-5 inline-block rounded-lg bg-brand-600 px-4 py-2 text-sm font-medium text-white hover:bg-brand-700"
        >
          Back
        </a>
      </div>
    </div>
  } @else if (meeting(); as m) {
    <header class="flex flex-wrap items-center justify-between gap-3 border-b border-white/10 px-4 py-3 sm:px-6">
      <div class="min-w-0">
        <h1 class="truncate text-lg font-semibold tracking-tight">{{ m.title }}</h1>
        <p class="mt-0.5 truncate text-xs text-slate-400">{{ whenText() }}</p>
      </div>
      <div class="flex flex-wrap items-center gap-3 text-xs text-slate-300">
        <span class="flex items-center gap-1.5">
          <span class="h-2 w-2 rounded-full bg-emerald-400"></span>
          {{ participants().length }} in call
        </span>
        @if (note()) {
          <span class="text-emerald-400">{{ note() }}</span>
        }
        @if (actionError()) {
          <span class="text-rose-400">{{ actionError() }}</span>
        }
        <button
          type="button"
          (click)="copyLink(m.joinUrl)"
          class="rounded-lg border border-white/15 px-3 py-1.5 text-xs font-medium hover:bg-white/5"
        >
          Copy link
        </button>
        <a
          [routerLink]="['/meetings', m.uid]"
          [queryParams]="guestParams()"
          class="rounded-lg border border-white/15 px-3 py-1.5 text-xs font-medium hover:bg-white/5"
        >
          Details
        </a>
      </div>
    </header>

    <div class="flex min-h-0 flex-1 flex-col lg:flex-row">
      <section class="flex min-h-0 flex-1 flex-col">
        @if (mediaNotice()) {
          <div class="mx-3 mt-3 flex flex-wrap items-center gap-2 rounded-xl border border-amber-400/30 bg-amber-400/10 px-3 py-2 text-xs text-amber-200 sm:mx-5">
            <span>{{ mediaNotice() }}</span>
            @if (inCall()) {
              <button type="button" (click)="retryMedia()" class="font-medium underline decoration-amber-400/60 hover:text-amber-100">
                Try camera and microphone
              </button>
            }
          </div>
        }
        @if (callError()) {
          <div class="mx-3 mt-3 rounded-xl border border-rose-400/30 bg-rose-400/10 px-3 py-2 text-xs text-rose-200 sm:mx-5">
            {{ callError() }}
          </div>
        }

        <div class="min-h-0 flex-1 overflow-y-auto p-3 sm:p-5" data-testid="call-grid">
          <div class="grid grid-cols-1 gap-3 sm:grid-cols-2 xl:grid-cols-3">
            @for (p of visibleParticipants(); track p.member) {
              <div
                data-testid="call-tile"
                class="relative aspect-video overflow-hidden rounded-xl bg-slate-800 ring-1 ring-white/10"
                [ngClass]="p.member === activeSpeaker() ? 'ring-2 ring-emerald-400' : ''"
              >
                @if (hasVideo(p)) {
                  <video
                    [srcObject]="p.stream"
                    autoplay
                    [muted]="p.self"
                    playsinline
                    class="h-full w-full object-cover"
                  ></video>
                } @else {
                  <div class="flex h-full w-full flex-col items-center justify-center gap-3">
                    <div class="flex h-20 w-20 items-center justify-center rounded-full bg-gradient-to-br from-brand-600 to-violet-700 text-2xl font-semibold text-white">
                      {{ initials(p.member) }}
                    </div>
                    <span class="text-xs text-slate-400">Camera off</span>
                  </div>
                  @if (!p.self) {
                    <audio [srcObject]="p.stream" autoplay></audio>
                  }
                }

                <div class="absolute inset-x-0 bottom-0 flex items-center justify-between gap-2 bg-gradient-to-t from-black/80 to-transparent px-3 pb-2 pt-8 text-xs">
                  <span class="truncate font-medium">{{ p.self ? 'You' : p.member }}</span>
                  <span class="flex shrink-0 items-center gap-1.5">
                    @if (p.member === floorSpeaker()) {
                      <span class="rounded bg-brand-500/30 px-1.5 py-0.5 text-[10px] font-medium text-brand-200">floor</span>
                    }
                    @if (!hasAudio(p)) {
                      <svg viewBox="0 0 24 24" class="h-3.5 w-3.5 text-rose-300" fill="none" stroke="currentColor" stroke-width="2" aria-label="Microphone off">
                        <path d="m3 3 18 18M9 9v5a3 3 0 0 0 5.1 2.1M15 9V6a3 3 0 0 0-5.8-1M5 11a7 7 0 0 0 11.5 5.4M12 18v3" />
                      </svg>
                    }
                  </span>
                </div>
              </div>
            }
          </div>
        </div>

        @if (isOrganizer() && m.oneWay && raisedHands().length > 0) {
          <div class="mx-3 mb-2 rounded-xl border border-amber-400/30 bg-amber-400/10 p-3 sm:mx-5" data-testid="raised-hands">
            <p class="text-xs font-semibold text-amber-200">Raised hands</p>
            @for (hand of raisedHands(); track hand) {
              <div class="mt-1 flex flex-wrap items-center justify-between gap-2">
                <span class="text-xs text-amber-100">{{ hand }}</span>
                <span class="flex gap-2">
                  <button type="button" (click)="allowSpeak(hand)" class="rounded-md bg-amber-500 px-2.5 py-1 text-[11px] font-medium text-slate-950 hover:bg-amber-400">
                    Allow to speak
                  </button>
                  @if (floorSpeaker() === hand) {
                    <button type="button" (click)="revokeSpeak(hand)" class="rounded-md border border-amber-400/40 px-2.5 py-1 text-[11px] font-medium text-amber-200 hover:bg-amber-400/10">
                      Mute
                    </button>
                  }
                </span>
              </div>
            }
          </div>
        }

        @if (m.oneWay) {
          <p class="mx-3 mb-1 text-center text-xs text-slate-400 sm:mx-5" data-testid="webinar-note">
            One-way conference: only the presenter speaks.
            @if (floorSpeaker()) {
              <span class="font-medium text-slate-200">{{ floorSpeaker() }}</span> has the floor.
            }
          </p>
        }

        <footer class="flex flex-wrap items-center justify-center gap-2 border-t border-white/10 px-4 py-3">
          @if (!inCall()) {
            <button type="button" (click)="joinCall()" class="rounded-xl bg-brand-600 px-5 py-2 text-sm font-semibold text-white hover:bg-brand-700">
              Join call
            </button>
          } @else {
            <button type="button" (click)="toggleMic()" class="flex min-w-16 flex-col items-center gap-1 rounded-xl px-3 py-2 text-[11px] font-medium text-slate-200 hover:bg-white/10">
              <svg viewBox="0 0 24 24" class="h-5 w-5" fill="none" stroke="currentColor" stroke-width="1.8">
                <rect x="9" y="3" width="6" height="11" rx="3" />
                <path d="M5 11a7 7 0 0 0 14 0M12 18v3" />
              </svg>
              {{ micOn() ? 'Mute' : 'Unmute' }}
            </button>
            <button type="button" (click)="toggleCamera()" class="flex min-w-16 flex-col items-center gap-1 rounded-xl px-3 py-2 text-[11px] font-medium text-slate-200 hover:bg-white/10">
              <svg viewBox="0 0 24 24" class="h-5 w-5" fill="none" stroke="currentColor" stroke-width="1.8">
                <rect x="3" y="6" width="12" height="12" rx="2" />
                <path d="m15 10 6-3v10l-6-3" />
              </svg>
              {{ cameraOn() ? 'Camera' : 'Camera off' }}
            </button>
          }
          @if (m.oneWay && !isOrganizer()) {
            <button type="button" (click)="toggleHand()" data-testid="raise-hand" class="flex min-w-16 flex-col items-center gap-1 rounded-xl px-3 py-2 text-[11px] font-medium text-amber-200 hover:bg-amber-400/10">
              <svg viewBox="0 0 24 24" class="h-5 w-5" fill="none" stroke="currentColor" stroke-width="1.8">
                <path d="M8 12V5a2 2 0 1 1 4 0v5M12 10V4a2 2 0 1 1 4 0v6M16 11V6a2 2 0 1 1 4 0v8a6 6 0 0 1-6 6h-2a6 6 0 0 1-6-6v-3a2 2 0 1 1 4 0" />
              </svg>
              {{ handRaised() ? 'Lower hand' : 'Raise hand' }}
            </button>
          }
          <button
            type="button"
            (click)="chatOpen.set(!chatOpen())"
            class="flex min-w-16 flex-col items-center gap-1 rounded-xl px-3 py-2 text-[11px] font-medium hover:bg-white/10"
            [ngClass]="chatOpen() ? 'bg-brand-600/20 text-brand-200' : 'text-slate-200'"
          >
            <svg viewBox="0 0 24 24" class="h-5 w-5" fill="none" stroke="currentColor" stroke-width="1.8">
              <path d="M21 12a8 8 0 0 1-8 8H7l-4 3v-7a8 8 0 1 1 18-4Z" />
            </svg>
            Chat
          </button>
          <button type="button" (click)="leaveCall()" class="flex min-w-20 flex-col items-center gap-1 rounded-xl bg-rose-600 px-4 py-2 text-[11px] font-semibold text-white hover:bg-rose-700">
            <svg viewBox="0 0 24 24" class="h-5 w-5" fill="none" stroke="currentColor" stroke-width="1.8">
              <path d="M4 14c5-5 11-5 16 0l-3 3-3-2-2 2c-2-1-3-2-4-2s-2 1-4 2l-2-2 2-3Z" />
            </svg>
            Leave
          </button>
        </footer>
      </section>

      @if (chatOpen() && m.chatConversation) {
        <aside class="flex h-80 w-full shrink-0 flex-col border-t border-white/10 bg-slate-900 lg:h-auto lg:w-80 lg:border-l lg:border-t-0">
          <div class="flex items-center justify-between border-b border-white/10 px-4 py-3">
            <h2 class="text-sm font-semibold">Chat</h2>
            <button type="button" (click)="chatOpen.set(false)" class="rounded-md px-2 py-1 text-lg leading-none text-slate-400 hover:bg-white/10 hover:text-white" aria-label="Collapse chat">
              ×
            </button>
          </div>

          @if (chatError()) {
            <div class="p-4 text-xs text-rose-300">
              <p>{{ chatError() }}</p>
              <button type="button" (click)="openChat()" class="mt-2 rounded-lg border border-white/15 px-3 py-1.5 font-medium text-slate-200 hover:bg-white/5">
                Try again
              </button>
            </div>
          } @else if (chat.state() === 'connected') {
            <div #chatLog id="chatLog" class="min-h-0 flex-1 overflow-y-auto bg-slate-950/40 p-3">
              @if (chat.messages().length === 0) {
                <p class="pt-16 text-center text-xs text-slate-500">No messages yet. Say something.</p>
              }
              @for (msg of chat.messages(); track msg.id || msg.clientId) {
                <div class="mb-3 flex gap-2" [ngClass]="isSelf(msg) ? 'flex-row-reverse' : ''">
                  <div class="flex h-7 w-7 shrink-0 items-center justify-center rounded-full bg-slate-700 text-[11px] font-medium text-slate-200">
                    {{ initial(msg) }}
                  </div>
                  <div class="min-w-0" [ngClass]="isSelf(msg) ? 'text-right' : ''">
                    <div class="text-[11px] text-slate-400">{{ msg.sender }}</div>
                    <div class="mt-0.5 inline-block rounded-lg px-3 py-1.5 text-sm whitespace-pre-wrap" [ngClass]="isSelf(msg) ? 'bg-brand-600 text-white' : 'bg-slate-800 text-slate-100'">
                      {{ msg.body }}
                    </div>
                    <div class="mt-0.5 text-[10px] text-slate-500">{{ timeOf(msg) }}</div>
                  </div>
                </div>
              }
              @if (chat.typing()) {
                <p class="text-[11px] italic text-slate-500">{{ chat.typing() }} is typing…</p>
              }
            </div>
            <form class="flex gap-2 border-t border-white/10 p-3" (ngSubmit)="sendChat()">
              <input
                type="text"
                [(ngModel)]="draftText"
                (ngModelChange)="chat.notifyTyping()"
                name="draftText"
                placeholder="Message the room"
                autocomplete="off"
                class="min-w-0 flex-1 rounded-lg border border-white/10 bg-slate-950 px-3 py-2 text-sm text-slate-100 placeholder:text-slate-500 focus:border-brand-500 focus:outline-none focus:ring-1 focus:ring-brand-500"
              />
              <button type="submit" [disabled]="chatBusy() || !draftText.trim()" class="rounded-lg bg-brand-600 px-3 py-2 text-sm font-medium text-white hover:bg-brand-700 disabled:opacity-50">
                Send
              </button>
            </form>
          } @else if (chat.state() === 'error') {
            <div class="p-4 text-xs text-rose-300">
              <p>{{ chat.error() || 'The chat connection could not be opened.' }}</p>
              <button type="button" (click)="openChat()" class="mt-2 rounded-lg border border-white/15 px-3 py-1.5 font-medium text-slate-200 hover:bg-white/5">
                Try again
              </button>
            </div>
          } @else {
            <p class="p-4 text-xs text-slate-400">Connecting to the chat…</p>
          }
        </aside>
      }
    </div>
  }
</main>
`,
})
export class CallRoomComponent implements OnInit, OnDestroy {
  private readonly route = inject(ActivatedRoute);
  private readonly api = inject(MeetingsApi);
  private readonly auth = inject(AuthService);

  readonly chat = inject(ChatService);

  readonly meeting = signal<Meeting | null>(null);
  readonly loading = signal(true);
  readonly mode = signal<'invitee' | 'member' | 'signin'>('member');
  readonly inviteeTo = signal('');
  readonly note = signal('');
  readonly actionError = signal('');
  readonly pageError = signal('');
  readonly chatBusy = signal(false);
  readonly chatError = signal('');
  readonly chatOpen = signal(true);

  // --- call state ---
  readonly inCall = signal(false);
  readonly callError = signal('');
  // mediaNotice is a soft status: a call works without a camera or microphone,
  // so a refused permission is information rather than an error.
  readonly mediaNotice = signal('');
  readonly micOn = signal(false);
  readonly cameraOn = signal(false);
  // participants is every connected participant, local included. The grid
  // shows at most four of them; the rest are heard but not seen.
  readonly participants = signal<CallParticipant[]>([]);
  readonly levels = signal<Record<string, number>>({});
  readonly activeSpeaker = signal('');
  readonly raisedHands = signal<string[]>([]);
  readonly floorSpeaker = signal('');
  readonly handRaised = signal(false);
  readonly floorGranted = signal(false);

  // visibleParticipants is the gallery. The local tile comes first and every
  // remote participant follows; unlike the old four-tile layout, a meeting
  // room shows the whole room.
  readonly visibleParticipants = computed(() => {
    const all = this.participants();
    return [...all.filter((p) => p.self), ...all.filter((p) => !p.self)];
  });

  private localStream?: MediaStream;
  private readonly peers = new Map<string, RTCPeerConnection>();
  private readonly remoteStreams = new Map<string, MediaStream>();
  private signalUnsub?: () => void;
  // A public STUN server is the last-resort fallback; normally the server's
  // /webrtc/ice endpoint supplies the built-in relay or an external one.
  private iceServers: RTCIceServer[] = [{ urls: 'stun:stun.l.google.com:19302' }];

  // Voice activity is measured with one WebAudio analyser per stream, polled
  // a few times a second. It decides which tile is "speaking" and moves it to
  // the front, which is what keeps a full room readable.
  private audioContext?: AudioContext;
  private readonly meters = new Map<string, AnalyserNode>();
  private meterTimer?: number;
  private readonly meterData = new Uint8Array(512);

  private uid = '';
  private inviteeToken = '';

  // The form's draft line is typed into rather than rendered as a signal;
  // ngModel keeps it current and the click handlers read it when they run.
  draftText = '';

  private readonly chatLog = viewChild<ElementRef<HTMLDivElement>>('chatLog');

  readonly isOrganizer = computed(() => {
    const m = this.meeting();
    const me = this.auth.user();
    return !!m && !!me && m.organizer.address.toLowerCase() === me.toLowerCase();
  });

  readonly whenText = computed(() => {
    const m = this.meeting();
    return m ? formatWhen(m.start, m.end, m.timezone) : '';
  });

  constructor() {
    // The conversation grows upward from its end; scrolling follows new
    // messages rather than the component re-rendering for them.
    effect(() => {
      const count = this.chat.messages().length;
      const el = this.chatLog()?.nativeElement;
      if (el && count > 0) {
        el.scrollTop = el.scrollHeight;
      }
    });
  }

  async ngOnInit(): Promise<void> {
    await this.auth.bootstrap();
    this.uid = this.route.snapshot.paramMap.get('uid') ?? '';
    const q = this.route.snapshot.queryParamMap;
    const to = q.get('to');
    const token = q.get('token');
    if (to && token) {
      this.mode.set('invitee');
      this.inviteeTo.set(to);
      this.inviteeToken = token;
    } else if (!this.auth.isAuthenticated()) {
      this.mode.set('signin');
      this.pageError.set('Sign in to open this meeting.');
      this.loading.set(false);
      return;
    }
    await this.reload();
    if (this.mode() !== 'signin' && this.meeting() && !this.meeting()!.cancelled) {
      // The room page is the conference: joining does not need another click.
      // Camera and microphone are optional, and joinCall falls back to a
      // receive-only connection when they are refused.
      await this.joinCall();
    }
  }

  async ngOnDestroy(): Promise<void> {
    // A call and its camera must not outlive the page.
    this.cleanupCall();
    this.chat.close();
  }

  async copyLink(url: string): Promise<void> {
    try {
      await navigator.clipboard.writeText(url);
      this.note.set('Room link copied.');
    } catch {
      this.actionError.set('The link could not be copied - select it from the box above.');
    }
  }

  // --- chat ---

  async openChat(): Promise<void> {
    const m = this.meeting();
    if (!m?.chatConversation) return;
    this.chatOpen.set(true);
    this.chatError.set('');
    // Opening the chat must not drop a call that is already using the same
    // socket: joining the call opens it, and this only fills in when it has
    // not been opened yet.
    if (!this.chat.isOpenFor(m.chatConversation)) {
      await this.chat.open(m.chatConversation, this.chatAccess());
    }
    if (this.chat.state() === 'error') {
      this.chatError.set(this.chat.error() || 'The chat connection could not be opened.');
    }
  }

  // chatAccess is the token identity an invitee uses instead of a session.
  private chatAccess(): ChatAccess | undefined {
    if (this.mode() !== 'invitee') return undefined;
    return {
      guestUid: this.uid,
      guestTo: this.inviteeTo(),
      guestToken: this.inviteeToken,
    };
  }

  async sendChat(): Promise<void> {
    const text = this.draftText.trim();
    if (!text || this.chatBusy()) return;
    this.chatBusy.set(true);
    this.chatError.set('');
    this.chat.send(text);
    this.draftText = '';
    this.chatBusy.set(false);
  }

  isSelf(msg: ChatMessage): boolean {
    return msg.sender.toLowerCase() === this.chat.self().toLowerCase();
  }

  initial(msg: ChatMessage): string {
    return (msg.sender || '?').trim().charAt(0).toUpperCase() || '?';
  }

  timeOf(msg: ChatMessage): string {
    return msg.createdAt
      ? new Date(msg.createdAt).toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit' })
      : '';
  }

  // --- call ---

  // joinCall opens the camera and microphone, then asks the server who is
  // already in the room. The server introduces the participants; each browser
  // then connects directly to every other one, because the media never travels
  // through linchange.
  //
  // In a one-way meeting only the presenter starts with a live microphone and
  // camera. A guest joins muted and asks for the floor by raising a hand.
  async joinCall(): Promise<void> {
    if (this.inCall()) return;
    this.callError.set('');
    this.mediaNotice.set('');

    // The call shares the chat socket: it carries both the WebRTC signalling
    // and the webinar's hand/floor frames. Joining the call therefore opens
    // the socket if the chat panel has not been opened yet.
    const meeting = this.meeting();
    if (!meeting?.chatConversation) {
      this.callError.set('This meeting has no room to join.');
      return;
    }
    const access = this.chatAccess();
    if (!this.chat.isOpenFor(meeting.chatConversation)) {
      await this.chat.open(meeting.chatConversation, access);
    }
    if (!this.chat.isOpenFor(meeting.chatConversation)) {
      this.callError.set(this.chat.error() || 'The call could not connect.');
      return;
    }

    // The server decides what ICE servers this deployment uses: its own
    // STUN/TURN by default, or an external pair named in configuration. A
    // failure here leaves the fallback in place rather than refusing the call.
    try {
      const ice = access
        ? await this.api.inviteIceServers(meeting.uid, access.guestTo, access.guestToken)
        : await this.api.iceServers();
      if (ice.iceServers?.length) {
        this.iceServers = ice.iceServers;
      }
    } catch {
      // Keep the fallback.
    }

    const webinar = meeting.oneWay === true;
    const presenter = this.isOrganizer();
    const stream = await this.acquireLocalMedia();
    this.localStream = stream;

    const audio = stream.getAudioTracks()[0];
    const video = stream.getVideoTracks()[0];
    if (webinar && !presenter) {
      stream.getAudioTracks().forEach((track) => { track.enabled = false; });
      stream.getVideoTracks().forEach((track) => { track.enabled = false; });
      this.micOn.set(false);
      this.cameraOn.set(false);
    } else {
      this.micOn.set(!!audio?.enabled);
      this.cameraOn.set(!!video?.enabled);
    }

    if (!webinar && !audio) {
      this.mediaNotice.set(
        video
          ? 'No microphone is available, so you joined listen-only. You can still chat.'
          : 'No camera or microphone is available, so you joined listen-only. You can still chat.',
      );
    } else if (!webinar && !video) {
      this.mediaNotice.set('No camera is available, so you joined with audio only.');
    }

    const me = (this.chat.self() || this.auth.user()).toLowerCase();
    this.inCall.set(true);
    this.handRaised.set(false);
    this.floorGranted.set(false);
    this.raisedHands.set([]);
    this.floorSpeaker.set('');
    this.participants.set([{ member: me, self: true, stream }]);
    this.addMeter(me, stream);

    this.signalUnsub = this.chat.onSignal((signal) => void this.onSignal(signal));
    this.chat.sendSignal('rtc-join', '', {});
  }

  // acquireLocalMedia asks for camera and microphone, but never makes them a
  // condition of joining. A browser with no devices, or a participant who
  // denies the prompt, gets an empty stream and a receive-only connection.
  private async acquireLocalMedia(): Promise<MediaStream> {
    if (!navigator.mediaDevices?.getUserMedia) {
      return new MediaStream();
    }
    try {
      return await navigator.mediaDevices.getUserMedia({ audio: true, video: true });
    } catch {
      // One of the two was refused or unavailable. Try each separately so a
      // working microphone is not lost to a missing camera, and vice versa.
    }
    try {
      return await navigator.mediaDevices.getUserMedia({ audio: true });
    } catch {
      // No microphone either; try the camera on its own.
    }
    try {
      return await navigator.mediaDevices.getUserMedia({ video: true });
    } catch {
      return new MediaStream();
    }
  }

  // retryMedia re-asks for devices after a denied or unavailable permission.
  // Rejoining is deliberate: a new stream and a fresh negotiation are less
  // error-prone than grafting tracks onto peers that were created without them.
  async retryMedia(): Promise<void> {
    if (!this.inCall()) return;
    this.leaveCall();
    await this.joinCall();
  }

  leaveCall(): void {
    if (this.inCall()) {
      this.chat.sendSignal('rtc-leave', '', {});
    }
    this.cleanupCall();
  }

  toggleMic(): void {
    // A guest in a webinar has no microphone to toggle: the floor is the
    // presenter's to give, and raising a hand is how they ask for it.
    if (this.meeting()?.oneWay && !this.isOrganizer() && !this.floorGranted()) {
      this.callError.set('Raise your hand to ask the presenter for the floor.');
      return;
    }
    const track = this.localStream?.getAudioTracks()[0];
    if (!track) {
      // No microphone was acquired. Ask again rather than pretending the
      // button did something.
      void this.retryMedia();
      return;
    }
    track.enabled = !track.enabled;
    this.micOn.set(track.enabled);
  }

  toggleCamera(): void {
    const track = this.localStream?.getVideoTracks()[0];
    if (!track) {
      void this.retryMedia();
      return;
    }
    track.enabled = !track.enabled;
    this.cameraOn.set(track.enabled);
  }

  hasVideo(p: CallParticipant): boolean {
    return p.stream.getVideoTracks().some((track) => track.readyState === 'live' && track.enabled);
  }

  toggleHand(): void {
    if (this.handRaised()) {
      this.chat.sendControl('hand-lower');
      this.handRaised.set(false);
      return;
    }
    this.chat.sendControl('hand-raise');
    this.handRaised.set(true);
  }

  // allowSpeak lets one guest talk. The server enforces that only the
  // presenter may call it and that only one guest holds the floor at a time.
  allowSpeak(member: string): void {
    this.chat.sendControl('allow-speak', { to: member });
  }

  revokeSpeak(member: string): void {
    this.chat.sendControl('revoke-speak', { to: member });
  }

  private async onSignal(signal: ChatSignal): Promise<void> {
    const me = (this.chat.self() || this.auth.user()).toLowerCase();
    try {
      switch (signal.type) {
        case 'rtc-peers':
          // This tab is the newcomer, so it offers to everyone already here.
          // The existing peers wait for that offer, which keeps both sides from
          // offering at once.
          for (const peer of signal.peers ?? []) {
            await this.offerTo(peer);
          }
          break;
        case 'rtc-offer':
          await this.answerOffer(signal);
          break;
        case 'rtc-answer': {
          const pc = this.peers.get(signal.from);
          if (pc && signal.sdp) {
            await pc.setRemoteDescription(signal.sdp as RTCSessionDescriptionInit);
          }
          break;
        }
        case 'rtc-ice': {
          const pc = this.peers.get(signal.from);
          if (pc && signal.candidate) {
            await pc.addIceCandidate(signal.candidate as RTCIceCandidateInit);
          }
          break;
        }
        case 'rtc-leave':
          if (signal.member) {
            this.dropPeer(signal.member);
          }
          break;
        case 'hand-raise':
          if (signal.member && signal.member !== me && !this.raisedHands().includes(signal.member)) {
            this.raisedHands.update((list) => [...list, signal.member as string]);
          }
          break;
        case 'hand-lower':
          this.raisedHands.update((list) => list.filter((m) => m !== signal.member));
          break;
        case 'allow-speak':
          // The server answers the guest it concerns: true enables the
          // microphone, false takes it away.
          if (signal.member === me) {
            this.floorGranted.set(signal.allowed === true);
            this.setMicEnabled(signal.allowed === true);
            if (signal.allowed) {
              this.handRaised.set(false);
            }
          }
          break;
        case 'speaker':
          this.floorSpeaker.set(signal.speaker ?? '');
          break;
        case 'hands':
          // A joiner is told who is already waiting and who has the floor.
          this.raisedHands.set(signal.hands ?? []);
          this.floorSpeaker.set(signal.speaker ?? '');
          break;
        // rtc-join is informational: the newcomer offers to us.
      }
    } catch {
      this.callError.set('A call connection could not be established.');
    }
  }

  private createPeer(remote: string, withMedia = true): RTCPeerConnection {
    const existing = this.peers.get(remote);
    if (existing) return existing;

    const pc = new RTCPeerConnection({ iceServers: this.iceServers });
    this.peers.set(remote, pc);

    if (withMedia) {
      this.attachLocalMedia(pc);
    }

    pc.onicecandidate = (event) => {
      if (event.candidate) {
        this.chat.sendSignal('rtc-ice', remote, { candidate: event.candidate.toJSON() });
      }
    };
    pc.ontrack = (event) => {
      // An answerer that attaches its tracks with replaceTrack() does not
      // associate them with a MediaStream, so the offering side receives
      // tracks whose event.streams is empty. Keep one stream per peer and add
      // each track as it arrives; audio and video can arrive separately.
      let stream = event.streams[0];
      if (stream) {
        this.remoteStreams.set(remote, stream);
      } else {
        stream = this.remoteStreams.get(remote) ?? new MediaStream();
        if (!stream.getTrackById(event.track.id)) {
          stream.addTrack(event.track);
        }
        this.remoteStreams.set(remote, stream);
      }
      this.participants.update((list) => [
        ...list.filter((p) => p.member !== remote),
        { member: remote, self: false, stream },
      ]);
      this.addMeter(remote, stream);
    };
    pc.onconnectionstatechange = () => {
      if (pc.connectionState === 'failed' || pc.connectionState === 'closed') {
        this.dropPeer(remote);
      }
    };
    return pc;
  }

  private async offerTo(remote: string): Promise<void> {
    const pc = this.createPeer(remote, true);
    const offer = await pc.createOffer();
    await pc.setLocalDescription(offer);
    this.chat.sendSignal('rtc-offer', remote, { sdp: offer });
  }

  private async answerOffer(signal: ChatSignal): Promise<void> {
    if (!signal.sdp) return;
    // The offer carries both audio and video m-lines even from a participant
    // with no devices. Answer it first, then attach whatever local media we
    // have to the transceivers the offer created.
    const pc = this.createPeer(signal.from, false);
    await pc.setRemoteDescription(signal.sdp as RTCSessionDescriptionInit);
    this.attachLocalMedia(pc);
    const answer = await pc.createAnswer();
    await pc.setLocalDescription(answer);
    this.chat.sendSignal('rtc-answer', signal.from, { sdp: answer });
  }

  // attachLocalMedia adds the local tracks to a peer connection. On the
  // offering side it creates the m-lines; on the answering side it reuses the
  // transceivers the offer already created. A missing device leaves that
  // direction receive-only instead of failing the call.
  private attachLocalMedia(pc: RTCPeerConnection): void {
    const stream = this.localStream;
    const audio = stream?.getAudioTracks()[0];
    const video = stream?.getVideoTracks()[0];
    const attach = (kind: 'audio' | 'video', track?: MediaStreamTrack) => {
      if (!track) return;
      const transceiver = pc.getTransceivers().find((t) => t.receiver.track.kind === kind);
      if (transceiver) {
        transceiver.sender.replaceTrack(track);
        transceiver.direction = 'sendrecv';
      } else {
        pc.addTrack(track, stream as MediaStream);
      }
    };
    attach('audio', audio);
    attach('video', video);
    const has = (kind: 'audio' | 'video') =>
      pc.getTransceivers().some((t) => t.receiver.track.kind === kind);
    if (!audio && !has('audio')) {
      pc.addTransceiver('audio', { direction: 'recvonly' });
    }
    if (!video && !has('video')) {
      pc.addTransceiver('video', { direction: 'recvonly' });
    }
  }

  private dropPeer(member: string): void {
    const pc = this.peers.get(member);
    if (pc) {
      pc.close();
      this.peers.delete(member);
    }
    this.participants.update((list) => list.filter((p) => p.member !== member));
    this.remoteStreams.delete(member);
    this.meters.delete(member);
    this.raisedHands.update((list) => list.filter((m) => m !== member));
  }

  private setMicEnabled(on: boolean): void {
    const track = this.localStream?.getAudioTracks()[0];
    if (!track) return;
    track.enabled = on;
    this.micOn.set(on);
  }

  // addMeter starts measuring one participant's audio. The analyser is what
  // moves the active speaker to the front of the grid.
  private addMeter(member: string, stream: MediaStream): void {
    if (this.meters.has(member) || stream.getAudioTracks().length === 0) return;
    try {
      this.audioContext ??= new AudioContext();
      void this.audioContext.resume();
      const source = this.audioContext.createMediaStreamSource(stream);
      const analyser = this.audioContext.createAnalyser();
      analyser.fftSize = 512;
      source.connect(analyser);
      this.meters.set(member, analyser);
      this.startMetering();
    } catch {
      // WebAudio can be unavailable (or refused); the call works without it.
    }
  }

  private startMetering(): void {
    if (this.meterTimer) return;
    this.meterTimer = window.setInterval(() => {
      const raw: Record<string, number> = {};
      for (const [member, analyser] of this.meters) {
        analyser.getByteTimeDomainData(this.meterData);
        let sum = 0;
        for (const value of this.meterData) {
          const centred = (value - 128) / 128;
          sum += centred * centred;
        }
        raw[member] = Math.sqrt(sum / this.meterData.length);
      }
      // Smooth over the previous reading so the order does not flicker on
      // every syllable.
      this.levels.update((previous) => {
        const next: Record<string, number> = {};
        for (const [member, level] of Object.entries(raw)) {
          next[member] = 0.65 * (previous[member] ?? 0) + 0.35 * level;
        }
        return next;
      });
      const active = Object.entries(this.levels())
        .filter(([, level]) => level > 0.03)
        .sort((a, b) => b[1] - a[1])[0];
      this.activeSpeaker.set(active ? active[0] : '');
    }, 150);
  }

  private stopMetering(): void {
    if (this.meterTimer) {
      window.clearInterval(this.meterTimer);
      this.meterTimer = undefined;
    }
    this.meters.clear();
    void this.audioContext?.close();
    this.audioContext = undefined;
    this.levels.set({});
    this.activeSpeaker.set('');
  }

  private cleanupCall(): void {
    this.signalUnsub?.();
    this.signalUnsub = undefined;
    for (const pc of this.peers.values()) {
      pc.close();
    }
    this.peers.clear();
    this.remoteStreams.clear();
    this.participants.set([]);
    this.stopMetering();
    this.localStream?.getTracks().forEach((track) => track.stop());
    this.localStream = undefined;
    this.inCall.set(false);
    this.micOn.set(false);
    this.cameraOn.set(false);
    this.mediaNotice.set('');
    this.handRaised.set(false);
    this.floorGranted.set(false);
    this.raisedHands.set([]);
    this.floorSpeaker.set('');
  }

  // hasAudio reports whether a participant currently has a live, enabled
  // microphone. A tile with no audio shows the muted marker.
  hasAudio(p: CallParticipant): boolean {
    return p.stream.getAudioTracks().some((track) => track.readyState === 'live' && track.enabled);
  }

  // initials is the avatar for a participant whose camera is off or absent.
  initials(member: string): string {
    const local = member.split('@')[0];
    const parts = local.split(/[._\- ]+/).filter(Boolean);
    if (parts.length >= 2) {
      return (parts[0][0] + parts[1][0]).toUpperCase();
    }
    return local.slice(0, 2).toUpperCase() || '?';
  }

  // guestParams keeps the invitation token on links that leave the room.
  guestParams(): Record<string, string> {
    if (this.mode() !== 'invitee') return {};
    return { to: this.inviteeTo(), token: this.inviteeToken };
  }

  // --- loading ---

  private async reload(): Promise<void> {
    this.loading.set(true);
    try {
      const m =
        this.mode() === 'invitee'
          ? await this.api.inviteView(this.uid, this.inviteeTo(), this.inviteeToken)
          : await this.api.get(this.uid);
      this.meeting.set(m);
      this.pageError.set('');
      // The room's chat is part of the conference, so open its socket as
      // soon as the meeting is known rather than waiting for a button.
      if (m.chatConversation && !m.cancelled) {
        void this.openChat();
      }
    } catch (err) {
      this.pageError.set(this.loadMessage(err));
    } finally {
      this.loading.set(false);
    }
  }

  // loadMessage separates "this link is not a meeting" from a network
  // problem, because the invitee can do something about only the first: ask
  // for the invitation again.
  private loadMessage(err: unknown): string {
    if (err instanceof HttpErrorResponse && err.status === 404) {
      return this.mode() === 'invitee'
        ? 'This invitation link is not valid. Ask the organiser to send it again.'
        : 'This meeting does not exist.';
    }
    return errorMessage(err);
  }
}
