import { Component, OnInit, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { RouterLink } from '@angular/router';
import { AuthService } from '../core/auth/auth.service';
import { errorMessage } from '../core/http-error';

interface Challenge {
  token: string;
  image: string;
}

@Component({
  selector: 'app-forgot-password',
  imports: [FormsModule, RouterLink],
  template: `
    <div class="flex min-h-screen items-center justify-center px-4">
      <div class="w-full max-w-sm rounded-2xl bg-white p-8 shadow-sm ring-1 ring-slate-200">
        <h1 class="text-2xl font-semibold tracking-tight text-slate-900">Reset your password</h1>

        @if (sent()) {
          <p class="mt-1 text-sm text-slate-500">
            If that address is registered with a verified recovery email, a reset
            link is on its way.
          </p>
        } @else {
          <p class="mt-1 text-sm text-slate-500">
            Enter your address and we will email you a secure reset link.
          </p>

          @if (error()) {
            <p class="mt-4 rounded-lg bg-red-50 px-3 py-2 text-sm text-red-700">{{ error() }}</p>
          }

          <form class="mt-6 space-y-4" (ngSubmit)="submit(loginRef, captchaRef)">
            <label class="block text-sm font-medium text-slate-700">
              Email
              <input
                #loginRef
                name="login"
                type="email"
                autocomplete="username"
                required
                class="mt-1 block w-full rounded-lg border border-slate-300 px-3 py-2 text-sm shadow-sm focus:border-brand-500 focus:outline-none focus:ring-2 focus:ring-brand-200"
              />
            </label>

            <div class="block text-sm font-medium text-slate-700">
              <span>Verification code</span>
              <div class="mt-1 flex items-center gap-3">
                @if (challenge(); as c) {
                  <img
                    [src]="c.image"
                    alt="Verification code"
                    width="220"
                    height="64"
                    class="rounded-lg ring-1 ring-slate-200"
                  />
                } @else {
                  <span class="flex h-16 w-[220px] items-center justify-center rounded-lg bg-slate-50 text-xs text-slate-500 ring-1 ring-slate-200">
                    Loading…
                  </span>
                }
                <button
                  type="button"
                  class="rounded-lg border border-slate-300 px-3 py-1.5 text-xs font-medium text-slate-700 hover:bg-slate-50"
                  (click)="loadChallenge()"
                >
                  New code
                </button>
              </div>
              <input
                #captchaRef
                name="captcha"
                type="text"
                autocomplete="off"
                autocapitalize="characters"
                spellcheck="false"
                required
                maxlength="8"
                placeholder="Type the characters"
                class="mt-2 block w-full rounded-lg border border-slate-300 px-3 py-2 text-sm tracking-widest shadow-sm focus:border-brand-500 focus:outline-none focus:ring-2 focus:ring-brand-200"
              />
            </div>

            <button
              [disabled]="busy() || !challenge()"
              class="mt-2 w-full rounded-lg bg-brand-600 px-4 py-2 text-sm font-semibold text-white shadow-sm transition hover:bg-brand-700 disabled:cursor-not-allowed disabled:opacity-60"
            >
              {{ busy() ? 'Sending…' : 'Email my reset link' }}
            </button>
          </form>
        }

        <a
          routerLink="/login"
          class="mt-4 block text-center text-sm text-brand-600 hover:underline"
          >Back to sign in</a
        >
      </div>
    </div>
  `,
})
export class ForgotPasswordComponent implements OnInit {
  protected readonly busy = signal(false);
  protected readonly error = signal('');
  protected readonly sent = signal(false);
  protected readonly challenge = signal<Challenge | null>(null);

  constructor(private readonly auth: AuthService) {}

  ngOnInit(): void {
    void this.loadChallenge();
  }

  protected async loadChallenge(): Promise<void> {
    this.error.set('');
    try {
      const c = await this.auth.requestCaptcha();
      this.challenge.set({ token: c.token, image: c.image });
    } catch (err) {
      this.challenge.set(null);
      this.error.set(errorMessage(err));
    }
  }

  async submit(loginEl: HTMLInputElement, captchaEl: HTMLInputElement): Promise<void> {
    const login = loginEl.value.trim();
    const answer = captchaEl.value.trim();
    const challenge = this.challenge();
    if (!login || !answer || !challenge || this.busy()) return;
    this.busy.set(true);
    this.error.set('');
    try {
      // The endpoint always answers the same way, so success is the normal path.
      await this.auth.requestReset(login, challenge.token, answer);
      this.sent.set(true);
    } catch (err) {
      this.error.set(errorMessage(err));
      captchaEl.value = '';
      // A challenge is single use in practice: the person has just spent it
      // on an attempt, and a fresh one is what they need to try again.
      await this.loadChallenge();
    } finally {
      this.busy.set(false);
    }
  }
}
