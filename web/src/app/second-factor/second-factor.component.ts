import { Component, OnInit, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { Router } from '@angular/router';
import { AuthService } from '../core/auth/auth.service';
import { errorMessage } from '../core/http-error';

@Component({
  selector: 'app-second-factor',
  imports: [FormsModule],
  template: `
    <div class="flex min-h-screen items-center justify-center px-4">
      <div class="w-full max-w-sm rounded-2xl bg-white p-8 shadow-sm ring-1 ring-slate-200">
        <h1 class="text-2xl font-semibold tracking-tight text-slate-900">Two-step verification</h1>
        <p class="mt-1 text-sm text-slate-500">
          Enter the 6-digit code from your authenticator app.
        </p>

        @if (error()) {
          <p class="mt-4 rounded-lg bg-red-50 px-3 py-2 text-sm text-red-700">{{ error() }}</p>
        }

        <form class="mt-6 space-y-4" (ngSubmit)="submit(codeRef)">
          <input
            #codeRef
            name="code"
            type="text"
            inputmode="numeric"
            autocomplete="one-time-code"
            maxlength="8"
            placeholder="••••••"
            required
            class="block w-full rounded-lg border border-slate-300 px-3 py-3 text-center text-2xl tracking-[0.5em] shadow-sm focus:border-brand-500 focus:outline-none focus:ring-2 focus:ring-brand-200"
          />
          <button
            [disabled]="busy()"
            class="w-full rounded-lg bg-brand-600 px-4 py-2 text-sm font-semibold text-white shadow-sm transition hover:bg-brand-700 disabled:cursor-not-allowed disabled:opacity-60"
          >
            {{ busy() ? 'Verifying…' : 'Verify and sign in' }}
          </button>
        </form>
      </div>
    </div>
  `,
})
export class SecondFactorComponent implements OnInit {
  protected readonly busy = signal(false);
  protected readonly error = signal('');

  constructor(
    private readonly auth: AuthService,
    private readonly router: Router,
  ) {}

  ngOnInit(): void {
    if (!this.auth.hasChallenge()) {
      this.router.navigateByUrl('/login');
    }
  }

  async submit(codeEl: HTMLInputElement): Promise<void> {
    const code = codeEl.value.trim();
    if (!code || this.busy()) return;
    this.busy.set(true);
    this.error.set('');
    try {
      await this.auth.secondFactor(code);
      this.router.navigateByUrl('/dashboard');
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }
}
