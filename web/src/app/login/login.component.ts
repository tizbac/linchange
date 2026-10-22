import { Component, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { Router, RouterLink } from '@angular/router';
import { AuthService } from '../core/auth/auth.service';
import { errorMessage } from '../core/http-error';

@Component({
  selector: 'app-login',
  imports: [FormsModule, RouterLink],
  template: `
    <div class="flex min-h-screen items-center justify-center px-4">
      <div class="w-full max-w-sm rounded-2xl bg-white p-8 shadow-sm ring-1 ring-slate-200">
        <h1 class="text-2xl font-semibold tracking-tight text-slate-900" i18n="@@login.title">Sign in</h1>
        <p class="mt-1 text-sm text-slate-500" i18n="@@login.subtitle">Use your linchange account to continue.</p>

        @if (error()) {
          <p class="mt-4 rounded-lg bg-red-50 px-3 py-2 text-sm text-red-700">{{ error() }}</p>
        }

        <form class="mt-6 space-y-4" (ngSubmit)="submit(loginRef, passwordRef)">
          <label class="block text-sm font-medium text-slate-700">
            <span i18n="@@login.email">Email</span>
            <input
              #loginRef
              name="login"
              type="email"
              autocomplete="username"
              required
              class="mt-1 block w-full rounded-lg border border-slate-300 px-3 py-2 text-sm shadow-sm focus:border-brand-500 focus:outline-none focus:ring-2 focus:ring-brand-200"
            />
          </label>
          <label class="mt-4 block text-sm font-medium text-slate-700">
            <span i18n="@@login.password">Password</span>
            <input
              #passwordRef
              name="password"
              type="password"
              autocomplete="current-password"
              required
              class="mt-1 block w-full rounded-lg border border-slate-300 px-3 py-2 text-sm shadow-sm focus:border-brand-500 focus:outline-none focus:ring-2 focus:ring-brand-200"
            />
          </label>
          <button
            [disabled]="busy()"
            class="mt-2 w-full rounded-lg bg-brand-600 px-4 py-2 text-sm font-semibold text-white shadow-sm transition hover:bg-brand-700 disabled:cursor-not-allowed disabled:opacity-60"
          >
            @if (busy()) {
              <span i18n="@@login.signingIn">Signing in…</span>
            } @else {
              <span i18n="@@login.signIn">Sign in</span>
            }
          </button>
        </form>

        <a
          routerLink="/forgot-password"
          class="mt-4 block text-center text-sm text-brand-600 hover:underline"
          i18n="@@login.forgot"
          >Forgot your password?</a
        >
      </div>
    </div>
  `,
})
export class LoginComponent {
  protected readonly busy = signal(false);
  protected readonly error = signal('');

  constructor(
    private readonly auth: AuthService,
    private readonly router: Router,
  ) {}

  async submit(loginEl: HTMLInputElement, passwordEl: HTMLInputElement): Promise<void> {
    const login = loginEl.value.trim();
    const password = passwordEl.value;
    if (!login || !password || this.busy()) return;
    this.busy.set(true);
    this.error.set('');
    try {
      const challenge = await this.auth.login(login, password);
      this.router.navigateByUrl(challenge ? '/second-factor' : '/dashboard');
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }
}
