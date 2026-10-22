import { ChangeDetectionStrategy, Component, OnInit, inject, signal } from '@angular/core';
import { NgClass } from '@angular/common';
import { FormsModule } from '@angular/forms';
import { AuthService } from '../core/auth/auth.service';
import { errorMessage } from '../core/http-error';
import { OutOfOffice, Profile, ProfileApi, TwoFactorStart } from '../core/profile/profile-api';

// The signed-in user's own account. Everything here is about the caller: no
// address is named in a request, so the server can never be asked to change
// somebody else's password or second factor by mistake.
@Component({
  selector: 'app-profile',
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [FormsModule, NgClass],
  template: `
    <main class="mx-auto max-w-3xl px-4 py-8 sm:px-6">
      <header class="mb-6">
        <h1 class="text-2xl font-semibold tracking-tight text-slate-900" i18n="@@profile.title">Your account</h1>
        <p class="mt-1 text-sm text-slate-500">{{ profile()?.address }}</p>
      </header>

      @if (loading()) {
        <p class="text-sm text-slate-500" i18n="@@profile.loading">Loading your account…</p>
      } @else if (profile(); as p) {
        <div class="space-y-5">
          <section class="rounded-2xl bg-white p-6 shadow-sm ring-1 ring-slate-200">
            <h2 class="text-lg font-semibold text-slate-900" i18n="@@profile.account">Account</h2>
            <dl class="mt-4 grid gap-4 text-sm sm:grid-cols-2">
              <div>
                <dt class="text-slate-400" i18n="@@profile.address">Address</dt>
                <dd class="mt-0.5 text-slate-800">{{ p.address }}</dd>
              </div>
              <div>
                <dt class="text-slate-400" i18n="@@profile.displayName">Display name</dt>
                <dd class="mt-0.5 text-slate-800">{{ p.display_name || '—' }}</dd>
              </div>
              <div>
                <dt class="text-slate-400" i18n="@@profile.recovery">Recovery email</dt>
                <dd class="mt-0.5 text-slate-800">{{ p.recovery_email || 'Not set' }}</dd>
              </div>
            </dl>
          </section>

          <section class="rounded-2xl bg-white p-6 shadow-sm ring-1 ring-slate-200">
            <h2 class="text-lg font-semibold text-slate-900" i18n="@@profile.passwordTitle">Password</h2>
            @if (p.source === 'ldap') {
              <p class="mt-2 text-sm text-slate-500" i18n="@@profile.ldapPassword">
                Your password is managed by your organization's directory. Change it there.
              </p>
            } @else {
              <form class="mt-4 grid gap-4 sm:grid-cols-2" (ngSubmit)="changePassword()">
                <label class="block sm:col-span-2">
                  <span class="text-sm font-medium text-slate-700" i18n="@@profile.currentPassword">Current password</span>
                  <input
                    type="password"
                    name="currentPassword"
                    autocomplete="current-password"
                    [(ngModel)]="currentPassword"
                    required
                    class="mt-1 w-full rounded-lg border border-slate-300 px-3 py-2 text-sm text-slate-900 focus:border-brand-500 focus:outline-none focus:ring-1 focus:ring-brand-500"
                  />
                </label>
                <label class="block">
                  <span class="text-sm font-medium text-slate-700" i18n="@@profile.newPassword">New password</span>
                  <input
                    type="password"
                    name="newPassword"
                    autocomplete="new-password"
                    [(ngModel)]="newPassword"
                    required
                    class="mt-1 w-full rounded-lg border border-slate-300 px-3 py-2 text-sm text-slate-900 focus:border-brand-500 focus:outline-none focus:ring-1 focus:ring-brand-500"
                  />
                </label>
                <label class="block">
                  <span class="text-sm font-medium text-slate-700" i18n="@@profile.confirmPassword">Confirm new password</span>
                  <input
                    type="password"
                    name="confirmPassword"
                    autocomplete="new-password"
                    [(ngModel)]="confirmPassword"
                    required
                    class="mt-1 w-full rounded-lg border border-slate-300 px-3 py-2 text-sm text-slate-900 focus:border-brand-500 focus:outline-none focus:ring-1 focus:ring-brand-500"
                  />
                </label>
                <div class="flex flex-wrap items-center gap-3 sm:col-span-2">
                  <button
                    type="submit"
                    [disabled]="busy() || !currentPassword || !newPassword || !confirmPassword"
                    class="rounded-lg bg-brand-600 px-4 py-2 text-sm font-semibold text-white transition hover:bg-brand-700 disabled:opacity-50"
                    i18n="@@profile.changePassword"
                  >
                    Change password
                  </button>
                  <span class="text-xs text-slate-500" i18n="@@profile.passwordHint">
                    At least {{ p.min_password_length }} characters.
                  </span>
                </div>
              </form>
            }
          </section>

          <section class="rounded-2xl bg-white p-6 shadow-sm ring-1 ring-slate-200">
            <h2 class="text-lg font-semibold text-slate-900" i18n="@@profile.recoveryTitle">Recovery email</h2>
            <p class="mt-1 text-sm text-slate-500" i18n="@@profile.recoveryDescription">
              An address we can use if you lose access to this mailbox.
            </p>
            <form class="mt-4 flex flex-col gap-2 sm:flex-row" (ngSubmit)="saveRecovery()">
              <input
                type="email"
                name="recoveryEmail"
                autocomplete="email"
                [(ngModel)]="recoveryEmail"
                placeholder="you@elsewhere.com"
                i18n-placeholder="@@profile.recoveryPlaceholder"
                class="min-w-0 flex-1 rounded-lg border border-slate-300 px-3 py-2 text-sm text-slate-900 placeholder:text-slate-400 focus:border-brand-500 focus:outline-none focus:ring-1 focus:ring-brand-500"
              />
              <button
                type="submit"
                [disabled]="busy()"
                class="rounded-lg border border-slate-300 px-4 py-2 text-sm font-medium text-slate-700 transition hover:bg-slate-100 disabled:opacity-50"
                i18n="@@profile.save"
              >
                Save
              </button>
            </form>
          </section>

          <section class="rounded-2xl bg-white p-6 shadow-sm ring-1 ring-slate-200">
            <div class="flex flex-wrap items-start justify-between gap-3">
              <div>
                <h2 class="text-lg font-semibold text-slate-900" i18n="@@profile.outOfOfficeTitle">
                  Out of office
                </h2>
                <p class="mt-1 text-sm text-slate-500" i18n="@@profile.outOfOfficeDescription">
                  Send one automatic reply to each person who writes while this is active.
                </p>
              </div>
              <label class="flex items-center gap-2 text-sm text-slate-700">
                <input type="checkbox" name="oooEnabled" [(ngModel)]="oooEnabled" class="h-4 w-4 rounded border-slate-300" />
                <span i18n="@@profile.outOfOfficeEnabled">Active</span>
              </label>
            </div>
            <form class="mt-4 space-y-3" (ngSubmit)="saveOutOfOffice()">
              <div class="grid gap-3 sm:grid-cols-2">
                <label class="block">
                  <span class="text-sm font-medium text-slate-700" i18n="@@profile.outOfOfficeStart">Start</span>
                  <input
                    type="datetime-local"
                    name="oooStart"
                    [(ngModel)]="oooStart"
                    class="mt-1 w-full rounded-lg border border-slate-300 px-3 py-2 text-sm text-slate-900 focus:border-brand-500 focus:outline-none focus:ring-1 focus:ring-brand-500"
                  />
                </label>
                <label class="block">
                  <span class="text-sm font-medium text-slate-700" i18n="@@profile.outOfOfficeEnd">End</span>
                  <input
                    type="datetime-local"
                    name="oooEnd"
                    [(ngModel)]="oooEnd"
                    class="mt-1 w-full rounded-lg border border-slate-300 px-3 py-2 text-sm text-slate-900 focus:border-brand-500 focus:outline-none focus:ring-1 focus:ring-brand-500"
                  />
                </label>
              </div>
              <label class="block">
                <span class="text-sm font-medium text-slate-700" i18n="@@profile.outOfOfficeSubject">Subject</span>
                <input
                  type="text"
                  name="oooSubject"
                  [(ngModel)]="oooSubject"
                  maxlength="200"
                  placeholder="Out of office"
                  class="mt-1 w-full rounded-lg border border-slate-300 px-3 py-2 text-sm text-slate-900 placeholder:text-slate-400 focus:border-brand-500 focus:outline-none focus:ring-1 focus:ring-brand-500"
                />
              </label>
              <label class="block">
                <span class="text-sm font-medium text-slate-700" i18n="@@profile.outOfOfficeMessage">Message</span>
                <textarea
                  name="oooMessage"
                  [(ngModel)]="oooMessage"
                  rows="4"
                  maxlength="10000"
                  placeholder="I am away until Monday and will reply when I return."
                  class="mt-1 w-full rounded-lg border border-slate-300 px-3 py-2 text-sm text-slate-900 placeholder:text-slate-400 focus:border-brand-500 focus:outline-none focus:ring-1 focus:ring-brand-500"
                ></textarea>
              </label>
              <div class="flex flex-wrap items-center gap-3">
                <button
                  type="submit"
                  [disabled]="busy()"
                  class="rounded-lg bg-brand-600 px-4 py-2 text-sm font-semibold text-white transition hover:bg-brand-700 disabled:opacity-50"
                  i18n="@@profile.outOfOfficeSave"
                >
                  Save automatic reply
                </button>
                <span class="text-xs text-slate-500" i18n="@@profile.outOfOfficeHint">
                  Each sender is answered once per activation. Saving resets that list.
                </span>
              </div>
            </form>
          </section>

          <section class="rounded-2xl bg-white p-6 shadow-sm ring-1 ring-slate-200">
            <div class="flex flex-wrap items-start justify-between gap-3">
              <div>
                <h2 class="text-lg font-semibold text-slate-900" i18n="@@profile.twoFactorTitle">
                  Two-factor authentication
                </h2>
                <p class="mt-1 text-sm text-slate-500" i18n="@@profile.twoFactorDescription">
                  Use an authenticator app such as Google Authenticator, 1Password or Authy.
                </p>
              </div>
              @if (p.two_factor.available) {
                <span
                  class="rounded-full px-2.5 py-1 text-xs font-medium"
                  [ngClass]="p.two_factor.enabled ? 'bg-emerald-100 text-emerald-800' : 'bg-slate-100 text-slate-600'"
                >
                  @if (p.two_factor.enabled) {
                    <span i18n="@@profile.enabled">Enabled</span>
                  } @else {
                    <span i18n="@@profile.notEnabled">Not enabled</span>
                  }
                </span>
              }
            </div>

            @if (!p.two_factor.available) {
              <p class="mt-3 text-sm text-slate-500" i18n="@@profile.twoFactorUnavailable">
                Two-factor authentication is disabled on this server.
              </p>
            } @else if (p.two_factor.enabled && !setup()) {
              <form class="mt-4 flex flex-col gap-2 sm:flex-row sm:items-end" (ngSubmit)="disable2FA()">
                <label class="flex-1">
                  <span class="text-sm font-medium text-slate-700" i18n="@@profile.codeLabel">
                    Authenticator or recovery code
                  </span>
                  <input
                    type="text"
                    name="disableCode"
                    autocomplete="one-time-code"
                    [(ngModel)]="disableCode"
                    class="mt-1 w-full rounded-lg border border-slate-300 px-3 py-2 text-sm text-slate-900 focus:border-brand-500 focus:outline-none focus:ring-1 focus:ring-brand-500"
                  />
                </label>
                <button
                  type="submit"
                  [disabled]="busy() || !disableCode"
                  class="rounded-lg border border-rose-300 px-4 py-2 text-sm font-medium text-rose-700 transition hover:bg-rose-50 disabled:opacity-50"
                  i18n="@@profile.disable2fa"
                >
                  Disable
                </button>
              </form>
            } @else if (!p.two_factor.enabled && !setup()) {
              <button
                type="button"
                (click)="start2FA()"
                [disabled]="busy()"
                class="mt-4 rounded-lg bg-brand-600 px-4 py-2 text-sm font-semibold text-white transition hover:bg-brand-700 disabled:opacity-50"
                i18n="@@profile.setup2fa"
              >
                Set up two-factor authentication
              </button>
              @if (p.two_factor.pending) {
                <p class="mt-2 text-xs text-amber-700" i18n="@@profile.pending2fa">
                  A setup is in progress. Start again to get a fresh QR code.
                </p>
              }
            }

            @if (setup(); as s) {
              <div class="mt-5 grid gap-5 rounded-xl border border-slate-200 bg-slate-50 p-4 sm:grid-cols-[192px_1fr]">
                <img
                  [src]="s.qr"
                  alt="QR code"
                  class="mx-auto h-48 w-48 rounded-lg bg-white p-2 ring-1 ring-slate-200"
                />
                <div class="min-w-0">
                  <p class="text-sm font-medium text-slate-800" i18n="@@profile.scan">
                    Scan this code with your authenticator app.
                  </p>
                  <p class="mt-2 text-xs text-slate-500" i18n="@@profile.manualKey">Can't scan it? Enter this key:</p>
                  <code class="mt-1 block break-all rounded bg-white px-2 py-1 text-xs text-slate-700 ring-1 ring-slate-200">
                    {{ s.secret }}
                  </code>
                  <div class="mt-4">
                    <p class="text-sm font-medium text-slate-800" i18n="@@profile.recoveryCodes">Recovery codes</p>
                    <p class="text-xs text-slate-500" i18n="@@profile.recoveryCodesHint">
                      Store these somewhere safe. Each works once if you lose your phone.
                    </p>
                    <div class="mt-2 grid grid-cols-2 gap-1 rounded bg-white p-2 font-mono text-xs text-slate-700 ring-1 ring-slate-200">
                      @for (code of s.recovery_codes; track code) {
                        <span>{{ code }}</span>
                      }
                    </div>
                    <div class="mt-2 flex gap-3">
                      <button type="button" (click)="copyCodes()" class="text-xs font-medium text-brand-700 hover:underline" i18n="@@profile.copyCodes">
                        Copy codes
                      </button>
                      <button type="button" (click)="downloadCodes()" class="text-xs font-medium text-brand-700 hover:underline" i18n="@@profile.downloadCodes">
                        Download
                      </button>
                    </div>
                  </div>
                </div>
              </div>

              @if (confirmed()) {
                <div class="mt-4 flex items-center gap-3">
                  <p class="text-sm font-medium text-emerald-700" i18n="@@profile.enabledNow">
                    Two-factor authentication is enabled.
                  </p>
                  <button
                    type="button"
                    (click)="finishSetup()"
                    class="rounded-lg border border-slate-300 px-4 py-2 text-sm font-medium text-slate-700 hover:bg-slate-100"
                    i18n="@@profile.done"
                  >
                    Done
                  </button>
                </div>
              } @else {
                <form class="mt-4 flex flex-col gap-2 sm:flex-row sm:items-end" (ngSubmit)="confirm2FA()">
                  <label class="flex-1">
                    <span class="text-sm font-medium text-slate-700" i18n="@@profile.verifyCode">
                      Code from the app
                    </span>
                    <input
                      type="text"
                      name="confirmCode"
                      inputmode="numeric"
                      autocomplete="one-time-code"
                      [(ngModel)]="confirmCode"
                      class="mt-1 w-full rounded-lg border border-slate-300 px-3 py-2 text-sm text-slate-900 focus:border-brand-500 focus:outline-none focus:ring-1 focus:ring-brand-500"
                    />
                  </label>
                  <button
                    type="submit"
                    [disabled]="busy() || !confirmCode"
                    class="rounded-lg bg-brand-600 px-4 py-2 text-sm font-semibold text-white transition hover:bg-brand-700 disabled:opacity-50"
                    i18n="@@profile.verifyEnable"
                  >
                    Verify and enable
                  </button>
                  <button
                    type="button"
                    (click)="cancelSetup()"
                    class="rounded-lg border border-slate-300 px-4 py-2 text-sm font-medium text-slate-700 hover:bg-slate-100"
                    i18n="@@profile.cancel"
                  >
                    Cancel
                  </button>
                </form>
              }
            }

            @if (error()) {
              <p class="mt-4 rounded-xl bg-rose-50 px-4 py-3 text-sm text-rose-700 ring-1 ring-rose-200">{{ error() }}</p>
            }
            @if (ok()) {
              <p class="mt-4 rounded-xl bg-emerald-50 px-4 py-3 text-sm text-emerald-800 ring-1 ring-emerald-200">{{ ok() }}</p>
            }
          </section>
        </div>
      } @else if (error()) {
        <p class="rounded-xl bg-rose-50 px-4 py-3 text-sm text-rose-700 ring-1 ring-rose-200">{{ error() }}</p>
      }
    </main>
  `,
})
export class ProfileComponent implements OnInit {
  private readonly api = inject(ProfileApi);
  private readonly auth = inject(AuthService);

  protected readonly profile = signal<Profile | null>(null);
  protected readonly setup = signal<TwoFactorStart | null>(null);
  protected readonly confirmed = signal(false);
  protected readonly loading = signal(true);
  protected readonly busy = signal(false);
  protected readonly error = signal('');
  protected readonly ok = signal('');

  protected currentPassword = '';
  protected newPassword = '';
  protected confirmPassword = '';
  protected recoveryEmail = '';
  protected oooEnabled = false;
  protected oooStart = '';
  protected oooEnd = '';
  protected oooSubject = '';
  protected oooMessage = '';
  protected confirmCode = '';
  protected disableCode = '';

  ngOnInit(): void {
    void this.load();
  }

  private async load(): Promise<void> {
    this.loading.set(true);
    this.error.set('');
    try {
      const profile = await this.api.get();
      this.profile.set(profile);
      this.recoveryEmail = profile.recovery_email ?? '';
      const ooo = profile.out_of_office;
      this.oooEnabled = ooo?.enabled ?? false;
      this.oooStart = this.localInput(ooo?.start);
      this.oooEnd = this.localInput(ooo?.end);
      this.oooSubject = ooo?.subject ?? '';
      this.oooMessage = ooo?.message ?? '';
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.loading.set(false);
    }
  }

  protected async changePassword(): Promise<void> {
    this.error.set('');
    this.ok.set('');
    if (!this.currentPassword || !this.newPassword) return;
    if (this.newPassword !== this.confirmPassword) {
      this.error.set($localize`:@@profile.mismatch:The new passwords do not match.`);
      return;
    }
    this.busy.set(true);
    try {
      await this.api.changePassword(this.currentPassword, this.newPassword);
      this.currentPassword = '';
      this.newPassword = '';
      this.confirmPassword = '';
      this.ok.set($localize`:@@profile.passwordChanged:Your password has been changed. Other devices have been signed out.`);
      await this.auth.loadMe();
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async saveRecovery(): Promise<void> {
    this.error.set('');
    this.ok.set('');
    this.busy.set(true);
    try {
      await this.api.setRecoveryEmail(this.recoveryEmail.trim());
      this.ok.set($localize`:@@profile.recoverySaved:Recovery email saved.`);
      await this.load();
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }

  // localInput renders an RFC 3339 instant as the value a datetime-local input
  // expects, in the browser's own zone.
  private localInput(value?: string): string {
    if (!value) return '';
    const date = new Date(value);
    if (Number.isNaN(date.getTime())) return '';
    const pad = (n: number): string => String(n).padStart(2, '0');
    return (
      `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}` +
      `T${pad(date.getHours())}:${pad(date.getMinutes())}`
    );
  }

  private rfc3339(value: string): string | undefined {
    if (!value) return undefined;
    const date = new Date(value);
    if (Number.isNaN(date.getTime())) return undefined;
    return date.toISOString();
  }

  protected async saveOutOfOffice(): Promise<void> {
    this.error.set('');
    this.ok.set('');
    if (this.oooEnabled && !this.oooMessage.trim()) {
      this.error.set($localize`:@@profile.outOfOfficeMessageRequired:A message is needed before automatic replies can be enabled.`);
      return;
    }
    const start = this.rfc3339(this.oooStart);
    const end = this.rfc3339(this.oooEnd);
    if (start && end && new Date(end) <= new Date(start)) {
      this.error.set($localize`:@@profile.outOfOfficeEndBeforeStart:The end must be after the start.`);
      return;
    }
    const setting: OutOfOffice = {
      enabled: this.oooEnabled,
      start,
      end,
      subject: this.oooSubject.trim(),
      message: this.oooMessage.trim(),
    };
    this.busy.set(true);
    try {
      const saved = await this.api.setOutOfOffice(setting);
      this.ok.set($localize`:@@profile.outOfOfficeSaved:Automatic replies saved.`);
      this.oooEnabled = saved.enabled;
      this.oooStart = this.localInput(saved.start);
      this.oooEnd = this.localInput(saved.end);
      this.oooSubject = saved.subject;
      this.oooMessage = saved.message;
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async start2FA(): Promise<void> {
    this.error.set('');
    this.ok.set('');
    this.busy.set(true);
    try {
      const setup = await this.api.start2FA();
      this.setup.set(setup);
      this.confirmed.set(false);
      this.confirmCode = '';
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async confirm2FA(): Promise<void> {
    const code = this.confirmCode.trim();
    if (!code) return;
    this.error.set('');
    this.ok.set('');
    this.busy.set(true);
    try {
      await this.api.confirm2FA(code);
      this.confirmed.set(true);
      this.ok.set($localize`:@@profile.twoFactorEnabled:Two-factor authentication is enabled.`);
      await this.load();
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async disable2FA(): Promise<void> {
    const code = this.disableCode.trim();
    if (!code) return;
    this.error.set('');
    this.ok.set('');
    this.busy.set(true);
    try {
      await this.api.disable2FA(code);
      this.disableCode = '';
      this.setup.set(null);
      this.confirmed.set(false);
      this.ok.set($localize`:@@profile.twoFactorDisabled:Two-factor authentication is disabled.`);
      await this.load();
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected finishSetup(): void {
    this.setup.set(null);
    this.confirmed.set(false);
  }

  protected async cancelSetup(): Promise<void> {
    this.setup.set(null);
    this.confirmed.set(false);
    this.confirmCode = '';
    await this.load();
  }

  protected copyCodes(): void {
    const codes = this.setup()?.recovery_codes ?? [];
    if (!codes.length) return;
    void navigator.clipboard.writeText(codes.join('\n'));
    this.ok.set($localize`:@@profile.codesCopied:Recovery codes copied.`);
  }

  protected downloadCodes(): void {
    const codes = this.setup()?.recovery_codes ?? [];
    if (!codes.length) return;
    const body = [
      'linchange recovery codes',
      ...codes,
      '',
      'Each code works once. Keep this file somewhere safe.',
    ].join('\n');
    const url = URL.createObjectURL(new Blob([body], { type: 'text/plain' }));
    const link = document.createElement('a');
    link.href = url;
    link.download = 'linchange-recovery-codes.txt';
    document.body.appendChild(link);
    link.click();
    link.remove();
    window.setTimeout(() => URL.revokeObjectURL(url), 1000);
  }
}
