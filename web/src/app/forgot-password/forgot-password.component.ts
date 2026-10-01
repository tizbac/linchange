import { Component, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { RouterLink } from '@angular/router';
import { AuthService } from '../core/auth/auth.service';
import { errorMessage } from '../core/http-error';

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

          <form class="mt-6 space-y-4" (ngSubmit)="submit(loginRef)">
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
            <button
              [disabled]="busy()"
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
export class ForgotPasswordComponent {
  protected readonly busy = signal(false);
  protected readonly error = signal('');
  protected readonly sent = signal(false);

  constructor(private readonly auth: AuthService) {}

  async submit(loginEl: HTMLInputElement): Promise<void> {
    const login = loginEl.value.trim();
    if (!login || this.busy()) return;
    this.busy.set(true);
    this.error.set('');
    try {
      // The endpoint always answers the same way, so success is the normal path.
      await this.auth.requestReset(login);
      this.sent.set(true);
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }
}
