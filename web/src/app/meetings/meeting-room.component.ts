import { NgClass } from '@angular/common';
import { HttpErrorResponse } from '@angular/common/http';
import type { MessageItem } from '@openim/client-sdk';
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
import { errorMessage } from '../core/http-error';
import { MeetingsApi } from '../core/meetings/meetings-api';
import { Meeting } from '../core/meetings/meetings.models';
import { formatWhen, partStatClass, partStatLabel } from '../core/meetings/meetings.ui';
import {
  OpenIMService,
  messagePlaceholder,
  messageText,
  sdkError,
} from '../core/meetings/openim.service';

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

        <!-- Chat: a signed-in guest or the organiser, in a meeting that has
             a room. Invitees have no chat account to enter it with, and a
             cancelled meeting has no conversation worth continuing. -->
        @if (mode() === 'member' && !m.cancelled && m.imGroupId) {
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
                  @for (msg of chat.messages(); track msg.clientMsgID) {
                    <div class="mb-3 flex gap-2" [ngClass]="isSelf(msg) ? 'flex-row-reverse' : ''">
                      <div
                        class="flex h-7 w-7 shrink-0 items-center justify-center rounded-full bg-slate-200 text-xs font-medium text-slate-600"
                      >
                        {{ initial(msg) }}
                      </div>
                      <div class="min-w-0" [ngClass]="isSelf(msg) ? 'text-right' : ''">
                        <div class="text-xs text-slate-400">{{ msg.senderNickname || msg.sendID }}</div>
                        <div
                          class="mt-0.5 inline-block rounded-lg px-3 py-1.5 text-sm text-slate-800"
                          [ngClass]="isSelf(msg) ? 'bg-brand-100' : 'bg-white ring-1 ring-slate-200'"
                        >
                          @if (textOf(msg); as text) {
                            {{ text }}
                          } @else if (placeholderOf(msg); as what) {
                            <span class="italic text-slate-400">{{ what }}</span>
                          }
                        </div>
                        <div class="mt-0.5 text-[10px] text-slate-400">{{ timeOf(msg) }}</div>
                      </div>
                    </div>
                  }
                </div>
                <form class="mt-3 flex gap-2" (ngSubmit)="sendChat()">
                  <input
                    type="text"
                    [(ngModel)]="draftText"
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
                <p class="mt-3 text-sm text-rose-600">{{ chat.error() || 'The chat server could not be reached.' }}</p>
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

  readonly chat = inject(OpenIMService);

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
    // The SDK connection is per room: leaving the page logs it out so a
    // rotated token cannot find a session nobody is watching.
    await this.chat.close();
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
    if (!m?.imGroupId) return;
    this.chatOpen.set(true);
    this.chatError.set('');
    try {
      const session = await this.api.imSession();
      await this.chat.open(session, m.imGroupId);
      if (this.chat.state() === 'error') {
        this.chatError.set(this.chat.error() || 'The chat server could not be reached.');
      }
    } catch (err) {
      // 503 means this deployment runs without chat at all: the invitation
      // above is unaffected, and the page says so rather than implying the
      // meeting itself is broken.
      if (err instanceof HttpErrorResponse && err.status === 503) {
        this.chatError.set('Chat is not available on this server. The invitation and answers are unaffected.');
      } else {
        this.chatError.set(errorMessage(err));
      }
    }
  }

  async sendChat(): Promise<void> {
    const text = this.draftText.trim();
    if (!text || this.chatBusy()) return;
    this.chatBusy.set(true);
    this.chatError.set('');
    try {
      await this.chat.send(text);
      this.draftText = '';
    } catch (err) {
      this.chatError.set(sdkError(err));
    } finally {
      this.chatBusy.set(false);
    }
  }

  textOf(msg: MessageItem): string {
    return messageText(msg);
  }

  placeholderOf(msg: MessageItem): string {
    return messagePlaceholder(msg);
  }

  isSelf(msg: MessageItem): boolean {
    return msg.sendID === this.chat.selfId();
  }

  initial(msg: MessageItem): string {
    const name = msg.senderNickname || msg.sendID || '?';
    return name.trim().charAt(0).toUpperCase() || '?';
  }

  timeOf(msg: MessageItem): string {
    const at = msg.sendTime || msg.createTime;
    return at ? new Date(at).toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit' }) : '';
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
