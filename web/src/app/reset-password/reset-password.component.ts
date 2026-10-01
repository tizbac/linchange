import { Component, OnInit, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { ActivatedRoute, RouterLink } from '@angular/router';
import { AuthService } from '../core/auth/auth.service';
import { errorMessage } from '../core/http-error';

@Component({
  selector: 'app-reset-password',
  imports: [FormsModule, RouterLink],
  template: `
    <div class="flex min-h-screen items-center justify-center px-4">
      <div class="w-full max-w-sm rounded-2xl bg-white p-8 shadow-sm ring-1 ring-slate-200">
        <h1 class="text-2xl font-semibold tracking-tight text-slate-900">Choose a new password</h1>

        @if (done()) {
          <p class="mt-1 text-sm text-slate-500">
            Your password has been updated. You can sign in now.
          </p>
          <a
            routerLink="/login"
            class="mt-6 block w-full rounded-lg bg-brand-600 px-4 py-2 text-center text-sm font-semibold text-white shadow-sm transition hover:bg-brand-700"
            >Go to sign in</a
          >
        } @else {
          <p class="mt-1 text-sm text-slate-500">
            Use at least 8 characters; a mix of cases and digits is best.
          </p>

          @if (error()) {
            <p class="mt-4 rounded-lg bg-red-50 px-3 py-2 text-sm text-red-700">{{ error() }}</p>
          }

          <form class="mt-6 space-y-4" (ngSubmit)="submit()">
            <label class="block text-sm font-medium text-slate-700">
              New password
              <input
                name="password"
                [(ngModel)]="password"
                type="password"
                autocomplete="new-password"
                minlength="8"
                required
                class="mt-1 block w-full rounded-lg border border-slate-300 px-3 py-2 text-sm shadow-sm focus:border-brand-500 focus:outline-none focus:ring-2 focus:ring-brand-200"
              />
            </label>
            <label class="mt-4 block text-sm font-medium text-slate-700">
              Confirm password
              <input
                name="confirm"
                [(ngModel)]="confirm"
                type="password"
                autocomplete="new-password"
                required
                class="mt-1 block w-full rounded-lg border border-slate-300 px-3 py-2 text-sm shadow-sm focus:border-brand-500 focus:outline-none focus:ring-2 focus:ring-brand-200"
              />
            </label>
            @if (confirm.length > 0 && password !== confirm) {
              <p class="text-sm text-red-700">Passwords do not match.</p>
            }
            <button
              [disabled]="busy() || (confirm.length > 0 && password !== confirm)"
              class="mt-2 w-full rounded-lg bg-brand-600 px-4 py-2 text-sm font-semibold text-white shadow-sm transition hover:bg-brand-700 disabled:cursor-not-allowed disabled:opacity-60"
            >
              {{ busy() ? 'Saving…' : 'Save new password' }}
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
export class ResetPasswordComponent implements OnInit {
  protected token = '';
  protected password = '';
  protected confirm = '';
  protected readonly busy = signal(false);
  protected readonly error = signal('');
  protected readonly done = signal(false);

  constructor(
    private readonly auth: AuthService,
    private readonly route: ActivatedRoute,
  ) {}

  ngOnInit(): void {
    this.token = (this.route.snapshot.queryParamMap.get('token') ?? '').trim();
  }

  async submit(): Promise<void> {
    if (!this.token || !this.password || this.password !== this.confirm || this.busy()) return;
    this.busy.set(true);
    this.error.set('');
    try {
      await this.auth.completeReset(this.token, this.password);
      this.done.set(true);
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }
}
