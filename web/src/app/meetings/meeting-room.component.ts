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
import { ChatMessage, ChatService, ChatSignal } from '../core/chat/chat.service';
import { errorMessage } from '../core/http-error';
import { MeetingsApi } from '../core/meetings/meetings-api';
import { Meeting } from '../core/meetings/meetings.models';
import { formatWhen, partStatClass, partStatLabel } from '../core/meetings/meetings.ui';

// A four-person grid: the local tile plus three remote ones. A full mesh can
// carry more, and does — the participants past this limit are still connected
// and audible; only their video tiles are left out.
const MAX_REMOTE_TILES = 3;

interface CallParticipant {
  member: string;
  self: boolean;
  stream: MediaStream;
}

// MeetingRoomComponent is one page with two visitors: a signed-in user, who
// may be the organiser or a guest of this meeting, and an invitee who followed
// the link in their invitation and has no account here at all. The token in
// the link decides which one is standing in front of it - it wins even for a
// signed-in visitor, because the link names the guest whose answer it carries.
@Component({
  selector: 'app-meeting-room',
  standalone: true,
  imports: [NgClass, FormsModule, RouterLink],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <main class="mx-auto max-w-3xl px-6 py-10">
      @if (loading()) {
        <p class="text-sm text-slate-500">Loading the meeting…</p>
      } @else if (pageError()) {
        <div class="rounded-2xl bg-white p-8 text-center shadow-sm ring-1 ring-slate-200">
          <p class="text-sm text-slate-600">{{ pageError() }}</p>
          @if (mode() === 'signin') {
            <a
              routerLink="/login"
              class="mt-4 inline-block rounded-lg bg-brand-700 px-4 py-2 text-sm font-medium text-white hover:bg-brand-800 transition-colors"
            >
              Sign in
            </a>
          }
        </div>
      } @else if (meeting(); as m) {
        <header>
          <div class="flex items-center gap-3">
            <h1 class="text-2xl font-semibold tracking-tight text-slate-900">{{ m.title }}</h1>
            @if (m.cancelled) {
              <span class="rounded bg-rose-100 px-2 py-0.5 text-sm font-medium text-rose-700">Cancelled</span>
            }
          </div>
          @if (mode() === 'invitee') {
            <p class="mt-1 text-sm text-slate-500">
              You are answering as <span class="font-medium text-slate-700">{{ inviteeTo() }}</span> —
              the link from your invitation.
            </p>
          } @else if (isOrganizer()) {
            <p class="mt-1 text-sm text-slate-500">You are the organiser.</p>
          }
        </header>

        @if (note()) {
          <p class="mt-4 rounded-lg bg-emerald-50 px-4 py-2 text-sm text-emerald-800 ring-1 ring-emerald-200">
            {{ note() }}
          </p>
        }
        @if (actionError()) {
          <p class="mt-4 rounded-lg bg-rose-50 px-4 py-2 text-sm text-rose-700 ring-1 ring-rose-200">
            {{ actionError() }}
          </p>
        }

        <!-- What the meeting is -->
        <section class="mt-5 rounded-2xl bg-white p-6 shadow-sm ring-1 ring-slate-200">
          <dl class="space-y-3 text-sm">
            <div class="flex gap-3">
              <dt class="w-24 shrink-0 text-slate-400">When</dt>
              <dd class="text-slate-800">{{ whenText() }}</dd>
            </div>
            <div class="flex gap-3">
              <dt class="w-24 shrink-0 text-slate-400">Where</dt>
              <dd class="text-slate-800">{{ m.location || 'The link below' }}</dd>
            </div>
            <div class="flex gap-3">
              <dt class="w-24 shrink-0 text-slate-400">Organiser</dt>
              <dd class="text-slate-800">
                {{ m.organizer.name || m.organizer.address }}
                @if (m.organizer.name) {
                  <span class="text-slate-400">{{ m.organizer.address }}</span>
                }
              </dd>
            </div>
            @if (m.description) {
              <div class="flex gap-3">
                <dt class="w-24 shrink-0 text-slate-400">About</dt>
                <dd class="whitespace-pre-wrap text-slate-800">{{ m.description }}</dd>
              </div>
            }
            <div class="flex gap-3">
              <dt class="w-24 shrink-0 text-slate-400">Room link</dt>
              <dd class="flex min-w-0 items-center gap-2">
                <code class="truncate rounded bg-slate-100 px-2 py-1 text-xs text-slate-600">{{ m.joinUrl }}</code>
                <button
                  type="button"
                  (click)="copyLink(m.joinUrl)"
                  class="shrink-0 text-xs font-medium text-brand-700 hover:text-brand-800"
                >
                  Copy
                </button>
              </dd>
            </div>
          </dl>
        </section>

        <!-- Your answer. The organiser is not on their own guest list, so
             this block is absent for them rather than showing a row of
             buttons the server would refuse. -->
        @if (!m.cancelled && iAmGuest()) {
          <section class="mt-5 rounded-2xl bg-white p-6 shadow-sm ring-1 ring-slate-200">
            <h2 class="text-lg font-semibold text-slate-900">Your answer</h2>
            <p class="mt-1 text-sm text-slate-500">
              @if (myAnswer() && myAnswer() !== 'NEEDS-ACTION') {
                You answered <span class="font-medium text-slate-700">{{ label(myAnswer()!) }}</span>.
                You can change it until the meeting is over.
              } @else {
                The organiser sees your answer as soon as you give one.
              }
            </p>
            <div class="mt-4 flex flex-wrap gap-3">
              @for (option of answerOptions; track option) {
                <button
                  type="button"
                  [disabled]="busy()"
                  (click)="answer(option)"
                  class="rounded-lg px-4 py-2 text-sm font-medium transition-colors disabled:opacity-50"
                  [ngClass]="
                    myAnswer() === option
                      ? answerSelectedClass(option)
                      : 'border border-slate-300 text-slate-700 hover:bg-slate-50'
                  "
                >
                  {{ label(option) }}
                </button>
              }
            </div>
          </section>
        }
        @if (m.cancelled && iAmGuest()) {
          <p class="mt-5 rounded-lg bg-slate-100 px-4 py-3 text-sm text-slate-600 ring-1 ring-slate-200">
            This meeting was called off by the organiser.
          </p>
        }

        <!-- Organiser's controls -->
        @if (isOrganizer() && !m.cancelled) {
          <section class="mt-5 rounded-2xl bg-white p-6 shadow-sm ring-1 ring-slate-200">
            <h2 class="text-lg font-semibold text-slate-900">Invitations</h2>
            <p class="mt-1 text-sm text-slate-500">
              {{ m.attendees.length }} guests, {{ acceptedCount(m) }} accepted.
              Resending mails everybody their own link again — answers already given stand.
            </p>
            <div class="mt-4 flex flex-wrap gap-3">
              <button
                type="button"
                [disabled]="busy()"
                (click)="resend()"
                class="rounded-lg border border-slate-300 px-4 py-2 text-sm font-medium text-slate-700 hover:bg-slate-50 disabled:opacity-50 transition-colors"
              >
                {{ busy() ? 'Sending…' : 'Resend invitations' }}
              </button>
              <button
                type="button"
                [disabled]="busy()"
                (click)="cancelMeeting()"
                class="rounded-lg border border-rose-300 px-4 py-2 text-sm font-medium text-rose-700 hover:bg-rose-50 disabled:opacity-50 transition-colors"
              >
                Cancel meeting
              </button>
            </div>
          </section>
        }

        <!-- Guest list -->
        <section class="mt-5 rounded-2xl bg-white p-6 shadow-sm ring-1 ring-slate-200">
          <h2 class="text-lg font-semibold text-slate-900">Guests</h2>
          <table class="mt-3 w-full text-sm">
            <thead>
              <tr class="border-b border-slate-200 text-left text-xs uppercase tracking-wide text-slate-400">
                <th class="pb-2 font-medium">Guest</th>
                <th class="pb-2 text-right font-medium">Answer</th>
              </tr>
            </thead>
            <tbody>
              @for (a of m.attendees; track a.address) {
                <tr class="border-b border-slate-100 last:border-0">
                  <td class="py-2.5">
                    <div class="flex items-center gap-2">
                      <span class="text-slate-800">{{ a.displayName || a.address }}</span>
                      @if (a.displayName) {
                        <span class="text-xs text-slate-400">{{ a.address }}</span>
                      }
                      @if (isMe(a.address)) {
                        <span class="rounded bg-brand-100 px-1.5 py-0.5 text-xs font-medium text-brand-800">you</span>
                      }
                    </div>
                  </td>
                  <td class="py-2.5 text-right">
                    <span class="rounded px-2 py-0.5 text-xs font-medium" [ngClass]="partClass(a.partStat)">
                      {{ label(a.partStat) }}
                    </span>
                  </td>
                </tr>
              }
            </tbody>
          </table>
        </section>

        @if (mode() === 'member' && !m.cancelled && m.chatConversation) {
          <!-- Call: a mesh of peer connections. The server introduces the
               participants and relays their signalling; the media itself never
               passes through it. -->
          <section class="mt-5 rounded-2xl bg-white p-6 shadow-sm ring-1 ring-slate-200">
            <div class="flex items-center justify-between">
              <h2 class="text-lg font-semibold text-slate-900">Call</h2>
              @if (inCall()) {
                <button
                  type="button"
                  (click)="leaveCall()"
                  class="rounded-lg border border-slate-300 px-4 py-2 text-sm font-medium text-slate-700 hover:bg-slate-50 transition-colors"
                >
                  Leave call
                </button>
              } @else {
                <button
                  type="button"
                  (click)="joinCall()"
                  class="rounded-lg bg-brand-700 px-4 py-2 text-sm font-medium text-white hover:bg-brand-800 transition-colors"
                >
                  Join call
                </button>
              }
            </div>
            @if (callError()) {
              <p class="mt-3 text-sm text-rose-600">{{ callError() }}</p>
            }
            @if (inCall()) {
              <div class="mt-3 grid gap-3 sm:grid-cols-2" data-testid="call-grid">
                @for (p of visibleParticipants(); track p.member) {
                  <div
                    data-testid="call-tile"
                    class="rounded-lg"
                    [ngClass]="p.member === activeSpeaker() ? 'ring-2 ring-emerald-500' : ''"
                  >
                    <video
                      [srcObject]="p.stream"
                      autoplay
                      [muted]="p.self"
                      playsinline
                      class="aspect-video w-full rounded-lg bg-slate-900 object-cover"
                    ></video>
                    <p class="mt-1 flex flex-wrap items-center gap-2 text-xs text-slate-400">
                      <span>{{ p.self ? 'You' : p.member }}</span>
                      @if (p.member === activeSpeaker()) {
                        <span class="rounded bg-emerald-100 px-1.5 py-0.5 text-[10px] font-medium text-emerald-800">
                          speaking
                        </span>
                      }
                      @if (p.member === floorSpeaker()) {
                        <span class="rounded bg-brand-100 px-1.5 py-0.5 text-[10px] font-medium text-brand-800">
                          floor
                        </span>
                      }
                    </p>
                  </div>
                }
                @if (hiddenParticipants().length > 0) {
                  <div
                    class="flex items-center justify-center rounded-lg bg-slate-50 p-4 text-sm text-slate-500 ring-1 ring-slate-200"
                    data-testid="more-participants"
                  >
                    +{{ hiddenParticipants().length }} more listening
                  </div>
                }
              </div>
              <!-- Participants past the four tiles are still heard: their
                   audio plays without a video tile. -->
              @for (p of hiddenParticipants(); track p.member) {
                <audio [srcObject]="p.stream" autoplay></audio>
              }
              <div class="mt-3 flex flex-wrap items-center gap-2">
                <button
                  type="button"
                  (click)="toggleMic()"
                  class="rounded-lg border border-slate-300 px-4 py-2 text-sm font-medium text-slate-700 hover:bg-slate-50 transition-colors"
                >
                  {{ micOn() ? 'Mute' : 'Unmute' }}
                </button>
                <button
                  type="button"
                  (click)="toggleCamera()"
                  class="rounded-lg border border-slate-300 px-4 py-2 text-sm font-medium text-slate-700 hover:bg-slate-50 transition-colors"
                >
                  {{ cameraOn() ? 'Camera off' : 'Camera on' }}
                </button>
                @if (m.oneWay && !isOrganizer()) {
                  <button
                    type="button"
                    (click)="toggleHand()"
                    data-testid="raise-hand"
                    class="rounded-lg border border-amber-300 bg-amber-50 px-4 py-2 text-sm font-medium text-amber-800 hover:bg-amber-100 transition-colors"
                  >
                    {{ handRaised() ? 'Lower hand' : 'Raise hand' }}
                  </button>
                }
              </div>
              @if (m.oneWay) {
                <p class="mt-2 text-xs text-slate-500" data-testid="webinar-note">
                  One-way conference: only the presenter speaks.
                  @if (floorSpeaker()) {
                    <span class="font-medium text-slate-700">{{ floorSpeaker() }}</span> has the floor.
                  }
                </p>
              }
              @if (isOrganizer() && m.oneWay && raisedHands().length > 0) {
                <div class="mt-3 rounded-lg bg-amber-50 p-3 ring-1 ring-amber-200" data-testid="raised-hands">
                  <p class="text-sm font-medium text-amber-900">Raised hands</p>
                  @for (hand of raisedHands(); track hand) {
                    <div class="mt-1 flex flex-wrap items-center justify-between gap-2">
                      <span class="text-sm text-amber-900">{{ hand }}</span>
                      <span class="flex gap-2">
                        <button
                          type="button"
                          (click)="allowSpeak(hand)"
                          class="rounded-lg bg-amber-700 px-3 py-1.5 text-xs font-medium text-white hover:bg-amber-800 transition-colors"
                        >
                          Allow to speak
                        </button>
                        @if (floorSpeaker() === hand) {
                          <button
                            type="button"
                            (click)="revokeSpeak(hand)"
                            class="rounded-lg border border-amber-300 px-3 py-1.5 text-xs font-medium text-amber-900 hover:bg-amber-100 transition-colors"
                          >
                            Mute
                          </button>
                        }
                      </span>
                    </div>
                  }
                </div>
              }
            }
          </section>

          <!-- Chat: a signed-in guest or the organiser, in a meeting that has
               a room. Invitees have no account to enter it with, and a
               cancelled meeting has no conversation worth continuing. -->
          <section class="mt-5 rounded-2xl bg-white p-6 shadow-sm ring-1 ring-slate-200">
            <div class="flex items-center justify-between">
              <h2 class="text-lg font-semibold text-slate-900">Meeting chat</h2>
              @if (!chatOpen()) {
                <button
                  type="button"
                  (click)="openChat()"
                  class="rounded-lg bg-brand-700 px-4 py-2 text-sm font-medium text-white hover:bg-brand-800 transition-colors"
                >
                  Open chat
                </button>
              }
            </div>

            @if (chatOpen()) {
              @if (chatError()) {
                <p class="mt-3 text-sm text-rose-600">{{ chatError() }}</p>
                <button
                  type="button"
                  (click)="openChat()"
                  class="mt-3 rounded-lg border border-slate-300 px-4 py-2 text-sm font-medium text-slate-700 hover:bg-slate-50 transition-colors"
                >
                  Try again
                </button>
              } @else if (chat.state() === 'connected') {
                <div
                  #chatLog
                  id="chatLog"
                  class="mt-3 h-72 overflow-y-auto rounded-lg bg-slate-50 p-3 ring-1 ring-slate-200"
                >
                  @if (chat.messages().length === 0) {
                    <p class="pt-24 text-center text-sm text-slate-400">
                      No messages yet. Say something.
                    </p>
                  }
                  @for (msg of chat.messages(); track msg.id || msg.clientId) {
                    <div class="mb-3 flex gap-2" [ngClass]="isSelf(msg) ? 'flex-row-reverse' : ''">
                      <div
                        class="flex h-7 w-7 shrink-0 items-center justify-center rounded-full bg-slate-200 text-xs font-medium text-slate-600"
                      >
                        {{ initial(msg) }}
                      </div>
                      <div class="min-w-0" [ngClass]="isSelf(msg) ? 'text-right' : ''">
                        <div class="text-xs text-slate-400">{{ msg.sender }}</div>
                        <div
                          class="mt-0.5 inline-block rounded-lg px-3 py-1.5 text-sm text-slate-800 whitespace-pre-wrap"
                          [ngClass]="isSelf(msg) ? 'bg-brand-100' : 'bg-white ring-1 ring-slate-200'"
                        >
                          {{ msg.body }}
                        </div>
                        <div class="mt-0.5 text-[10px] text-slate-400">{{ timeOf(msg) }}</div>
                      </div>
                    </div>
                  }
                  @if (chat.typing()) {
                    <p class="text-xs italic text-slate-400">{{ chat.typing() }} is typing…</p>
                  }
                </div>
                <form class="mt-3 flex gap-2" (ngSubmit)="sendChat()">
                  <input
                    type="text"
                    [(ngModel)]="draftText"
                    (ngModelChange)="chat.notifyTyping()"
                    name="draftText"
                    placeholder="Message the room"
                    autocomplete="off"
                    class="flex-1 rounded-lg border border-slate-300 px-3 py-2 text-sm focus:border-brand-500 focus:outline-none focus:ring-1 focus:ring-brand-500"
                  />
                  <button
                    type="submit"
                    [disabled]="chatBusy() || !draftText.trim()"
                    class="rounded-lg bg-brand-700 px-4 py-2 text-sm font-medium text-white hover:bg-brand-800 disabled:opacity-50 transition-colors"
                  >
                    Send
                  </button>
                </form>
              } @else if (chat.state() === 'error') {
                <p class="mt-3 text-sm text-rose-600">{{ chat.error() || 'The chat connection could not be opened.' }}</p>
                <button
                  type="button"
                  (click)="openChat()"
                  class="mt-3 rounded-lg border border-slate-300 px-4 py-2 text-sm font-medium text-slate-700 hover:bg-slate-50 transition-colors"
                >
                  Try again
                </button>
              } @else {
                <p class="mt-3 text-sm text-slate-500">Connecting to the chat…</p>
              }
            }
          </section>
        }
      }
    </main>
  `,
})
export class MeetingRoomComponent implements OnInit, OnDestroy {
  private readonly route = inject(ActivatedRoute);
  private readonly api = inject(MeetingsApi);
  private readonly auth = inject(AuthService);

  readonly chat = inject(ChatService);

  readonly meeting = signal<Meeting | null>(null);
  readonly loading = signal(true);
  readonly mode = signal<'invitee' | 'member' | 'signin'>('member');
  readonly inviteeTo = signal('');
  readonly busy = signal(false);
  readonly note = signal('');
  readonly actionError = signal('');
  readonly pageError = signal('');
  readonly chatOpen = signal(false);
  readonly chatBusy = signal(false);
  readonly chatError = signal('');

  // --- call state ---
  readonly inCall = signal(false);
  readonly callError = signal('');
  readonly micOn = signal(true);
  readonly cameraOn = signal(true);
  // participants is every connected participant, local included. The grid
  // shows at most four of them; the rest are heard but not seen.
  readonly participants = signal<CallParticipant[]>([]);
  readonly levels = signal<Record<string, number>>({});
  readonly activeSpeaker = signal('');
  readonly raisedHands = signal<string[]>([]);
  readonly floorSpeaker = signal('');
  readonly handRaised = signal(false);
  readonly floorGranted = signal(false);

  // visibleParticipants keeps the local tile and fills the remaining three
  // slots, with whoever is speaking moved to the front.
  readonly visibleParticipants = computed(() => {
    const all = this.orderedParticipants();
    const self = all.filter((p) => p.self);
    const others = all.filter((p) => !p.self).slice(0, MAX_REMOTE_TILES);
    const chosen = [...self, ...others];
    const active = this.activeSpeaker();
    return chosen.sort((a, b) => {
      if (a.member === active && b.member !== active) return -1;
      if (b.member === active && a.member !== active) return 1;
      return 0;
    });
  });

  // hiddenParticipants are the ones past the grid limit. Their audio still
  // plays; only their video is left out.
  readonly hiddenParticipants = computed(() => {
    const shown = new Set(this.visibleParticipants().map((p) => p.member));
    return this.orderedParticipants().filter((p) => !p.self && !shown.has(p.member));
  });

  private orderedParticipants = computed(() => {
    const active = this.activeSpeaker();
    return [...this.participants()].sort((a, b) => {
      if (a.member === active && b.member !== active) return -1;
      if (b.member === active && a.member !== active) return 1;
      return 0;
    });
  });

  private localStream?: MediaStream;
  private readonly peers = new Map<string, RTCPeerConnection>();
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

  readonly answerOptions = ['ACCEPTED', 'TENTATIVE', 'DECLINED'] as const;

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

  // myAnswer is this visitor's standing on the guest list, which is the only
  // thing that decides whether answer buttons appear at all: the organiser is
  // not on it, and neither is a signed-in stranger.
  readonly myAnswer = computed(() => {
    const m = this.meeting();
    if (!m) return undefined;
    const who = this.mode() === 'invitee' ? this.inviteeTo() : this.auth.user();
    if (!who) return undefined;
    const lower = who.toLowerCase();
    return m.attendees.find((a) => a.address.toLowerCase() === lower)?.partStat;
  });

  readonly iAmGuest = computed(() => this.myAnswer() !== undefined);

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
  }

  async ngOnDestroy(): Promise<void> {
    // A call and its camera must not outlive the page.
    this.cleanupCall();
    this.chat.close();
  }

  label(partStat: string): string {
    return partStatLabel(partStat);
  }

  partClass(partStat: string): string {
    return partStatClass(partStat);
  }

  isMe(address: string): boolean {
    const who = this.mode() === 'invitee' ? this.inviteeTo() : this.auth.user();
    return !!who && address.toLowerCase() === who.toLowerCase();
  }

  acceptedCount(m: Meeting): number {
    return m.attendees.filter((a) => a.partStat === 'ACCEPTED').length;
  }

  // --- answers ---

  async answer(partStat: string): Promise<void> {
    if (this.busy()) return;
    this.busy.set(true);
    this.note.set('');
    this.actionError.set('');
    try {
      const inv = this.mode() === 'invitee';
      if (inv) {
        await this.api.inviteRSVP(this.uid, this.inviteeTo(), this.inviteeToken, partStat);
      } else {
        await this.api.rsvp(this.uid, partStat);
      }
      await this.reload();
      this.note.set(`Answered: ${partStatLabel(partStat)}. The organiser sees it straight away.`);
    } catch (err) {
      this.actionError.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }

  // --- organiser ---

  async resend(): Promise<void> {
    if (this.busy()) return;
    this.busy.set(true);
    this.note.set('');
    this.actionError.set('');
    try {
      const res = await this.api.resend(this.uid);
      if (res.inviteFailures?.length) {
        this.actionError.set(`Sent to ${res.sent}, but failed for: ${res.inviteFailures.join(', ')}`);
      } else {
        this.note.set(`Invitations resent to ${res.sent} guests.`);
      }
    } catch (err) {
      this.actionError.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }

  async cancelMeeting(): Promise<void> {
    if (this.busy()) return;
    // The cancellation is mailed to every guest, so it gets the pause a
    // confirmation gives it; everything else on this page is reversible.
    if (!confirm('Call this meeting off and mail every guest a cancellation?')) {
      return;
    }
    this.busy.set(true);
    this.note.set('');
    this.actionError.set('');
    try {
      await this.api.cancel(this.uid);
      await this.reload();
      this.note.set('Meeting cancelled. Every guest has been mailed the cancellation.');
    } catch (err) {
      this.actionError.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
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
      await this.chat.open(m.chatConversation);
    }
    if (this.chat.state() === 'error') {
      this.chatError.set(this.chat.error() || 'The chat connection could not be opened.');
    }
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
    return msg.sender.toLowerCase() === this.auth.user().toLowerCase();
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

    // The call shares the chat socket: it carries both the WebRTC signalling
    // and the webinar's hand/floor frames. Joining the call therefore opens
    // the socket if the chat panel has not been opened yet.
    const meeting = this.meeting();
    if (!meeting?.chatConversation) {
      this.callError.set('This meeting has no room to join.');
      return;
    }
    if (!this.chat.isOpenFor(meeting.chatConversation)) {
      await this.chat.open(meeting.chatConversation);
    }
    if (!this.chat.isOpenFor(meeting.chatConversation)) {
      this.callError.set(this.chat.error() || 'The call could not connect.');
      return;
    }

    // The server decides what ICE servers this deployment uses: its own
    // STUN/TURN by default, or an external pair named in configuration. A
    // failure here leaves the fallback in place rather than refusing the call.
    try {
      const ice = await this.api.iceServers();
      if (ice.iceServers?.length) {
        this.iceServers = ice.iceServers;
      }
    } catch {
      // Keep the fallback.
    }

    const webinar = meeting.oneWay === true;
    const presenter = this.isOrganizer();
    try {
      const stream = await navigator.mediaDevices.getUserMedia({ audio: true, video: true });
      this.localStream = stream;

      if (webinar && !presenter) {
        stream.getAudioTracks().forEach((track) => { track.enabled = false; });
        stream.getVideoTracks().forEach((track) => { track.enabled = false; });
        this.micOn.set(false);
        this.cameraOn.set(false);
      } else {
        this.micOn.set(true);
        this.cameraOn.set(true);
      }

      const me = this.auth.user().toLowerCase();
      this.inCall.set(true);
      this.handRaised.set(false);
      this.floorGranted.set(false);
      this.raisedHands.set([]);
      this.floorSpeaker.set('');
      this.participants.set([{ member: me, self: true, stream }]);
      this.addMeter(me, stream);

      this.signalUnsub = this.chat.onSignal((signal) => void this.onSignal(signal));
      this.chat.sendSignal('rtc-join', '', {});
    } catch {
      this.callError.set('The camera and microphone could not be opened. Check the browser permission.');
      this.cleanupCall();
    }
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
    if (!track) return;
    track.enabled = !track.enabled;
    this.micOn.set(track.enabled);
  }

  toggleCamera(): void {
    const track = this.localStream?.getVideoTracks()[0];
    if (!track) return;
    track.enabled = !track.enabled;
    this.cameraOn.set(track.enabled);
  }

  // toggleHand raises or lowers the guest's hand in a one-way meeting.
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
    const me = this.auth.user().toLowerCase();
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

  private createPeer(remote: string): RTCPeerConnection {
    const existing = this.peers.get(remote);
    if (existing) return existing;

    const pc = new RTCPeerConnection({ iceServers: this.iceServers });
    this.peers.set(remote, pc);

    this.localStream?.getTracks().forEach((track) => {
      pc.addTrack(track, this.localStream as MediaStream);
    });

    pc.onicecandidate = (event) => {
      if (event.candidate) {
        this.chat.sendSignal('rtc-ice', remote, { candidate: event.candidate.toJSON() });
      }
    };
    pc.ontrack = (event) => {
      const stream = event.streams[0];
      if (!stream) return;
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
    const pc = this.createPeer(remote);
    const offer = await pc.createOffer();
    await pc.setLocalDescription(offer);
    this.chat.sendSignal('rtc-offer', remote, { sdp: offer });
  }

  private async answerOffer(signal: ChatSignal): Promise<void> {
    if (!signal.sdp) return;
    const pc = this.createPeer(signal.from);
    await pc.setRemoteDescription(signal.sdp as RTCSessionDescriptionInit);
    const answer = await pc.createAnswer();
    await pc.setLocalDescription(answer);
    this.chat.sendSignal('rtc-answer', signal.from, { sdp: answer });
  }

  private dropPeer(member: string): void {
    const pc = this.peers.get(member);
    if (pc) {
      pc.close();
      this.peers.delete(member);
    }
    this.participants.update((list) => list.filter((p) => p.member !== member));
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
    this.participants.set([]);
    this.stopMetering();
    this.localStream?.getTracks().forEach((track) => track.stop());
    this.localStream = undefined;
    this.inCall.set(false);
    this.handRaised.set(false);
    this.floorGranted.set(false);
    this.raisedHands.set([]);
    this.floorSpeaker.set('');
  }

  // answerSelectedClass is the chosen button: the answer the visitor already
  // gave is filled in, the rest are outlines, so a reader sees their standing
  // before they read a word.
  answerSelectedClass(option: string): string {
    switch (option) {
      case 'ACCEPTED':
        return 'bg-emerald-600 text-white ring-2 ring-emerald-200';
      case 'TENTATIVE':
        return 'bg-amber-500 text-white ring-2 ring-amber-200';
      default:
        return 'bg-rose-600 text-white ring-2 ring-rose-200';
    }
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
