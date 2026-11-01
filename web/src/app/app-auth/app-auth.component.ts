import { ChangeDetectionStrategy, Component, OnInit, inject, signal } from '@angular/core';
import { ActivatedRoute, RouterLink } from '@angular/router';
import { ProfileApi, ApplicationTokenRequest } from '../core/profile/profile-api';
import { errorMessage } from '../core/http-error';

// The page a mail client sends somebody to when it asks for an application
// password. The person is already signed in through the normal login page
// (with the second factor where one is enrolled), and this page only asks
// whether the client named in the request may have one.
@Component({
  selector: 'app-app-auth',
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [RouterLink],
  template: `
    <main class="mx-auto flex min-h-screen max-w-lg items-center px-4 py-8">
      <section class="w-full rounded-2xl bg-white p-8 shadow-sm ring-1 ring-slate-200">
        <h1 class="text-2xl font-semibold tracking-tight text-slate-900" i18n="@@appAuth.title">
          Allow a mail client?
        </h1>

        @if (loading()) {
          <p class="mt-4 text-sm text-slate-500" i18n="@@appAuth.loading">Reading the request…</p>
        } @else if (done()) {
          <p class="mt-4 rounded-xl bg-emerald-50 px-4 py-3 text-sm text-emerald-800 ring-1 ring-emerald-200">
            @if (approved()) {
              <span i18n="@@appAuth.approved">Allowed. You can return to the application now.</span>
            } @else {
              <span i18n="@@appAuth.denied">Denied. The application was not given a password.</span>
            }
          </p>
          <a routerLink="/profile" class="mt-5 inline-block text-sm font-medium text-brand-700 hover:underline" i18n="@@appAuth.manage">
            Manage application passwords
          </a>
        } @else if (request(); as request) {
          <p class="mt-4 text-sm text-slate-600">
            <span i18n="@@appAuth.asks">This application is asking for an application password:</span>
            <strong class="mt-1 block text-slate-900">{{ request.description }}</strong>
          </p>
          <p class="mt-3 text-sm text-slate-500" i18n="@@appAuth.explain">
            It will be able to read and change your mail, calendar and contacts as you can. Your account password is not shared with it, and you can revoke it later from your account page.
          </p>
          <p class="mt-3 text-xs text-slate-400">
            <span i18n="@@appAuth.code">Request code:</span> {{ request.user_code }}
          </p>

          @if (error()) {
            <p class="mt-4 rounded-xl bg-rose-50 px-4 py-3 text-sm text-rose-700 ring-1 ring-rose-200">{{ error() }}</p>
          }

          <div class="mt-6 flex flex-wrap gap-3">
            <button
              type="button"
              (click)="decide(true)"
              [disabled]="busy()"
              class="rounded-lg bg-brand-600 px-4 py-2 text-sm font-semibold text-white transition hover:bg-brand-700 disabled:opacity-50"
              i18n="@@appAuth.allow"
            >
              Allow
            </button>
            <button
              type="button"
              (click)="decide(false)"
              [disabled]="busy()"
              class="rounded-lg border border-slate-300 px-4 py-2 text-sm font-medium text-slate-700 transition hover:bg-slate-100 disabled:opacity-50"
              i18n="@@appAuth.deny"
            >
              Deny
            </button>
          </div>
        } @else {
          <p class="mt-4 rounded-xl bg-rose-50 px-4 py-3 text-sm text-rose-700 ring-1 ring-rose-200">{{ error() }}</p>
          <a routerLink="/profile" class="mt-5 inline-block text-sm font-medium text-brand-700 hover:underline" i18n="@@appAuth.back">
            Back to your account
          </a>
        }
      </section>
    </main>
  `,
})
export class AppAuthComponent implements OnInit {
  private readonly api = inject(ProfileApi);
  private readonly route = inject(ActivatedRoute);

  protected readonly request = signal<ApplicationTokenRequest | null>(null);
  protected readonly loading = signal(true);
  protected readonly busy = signal(false);
  protected readonly done = signal(false);
  protected readonly approved = signal(false);
  protected readonly error = signal('');

  private code = '';

  ngOnInit(): void {
    this.code = this.route.snapshot.queryParamMap.get('code') ?? '';
    if (!this.code.trim()) {
      this.loading.set(false);
      this.error.set($localize`:@@appAuth.missingCode:This link has no request code. Start again from the application.`);
      return;
    }
    void this.load();
  }

  private async load(): Promise<void> {
    this.loading.set(true);
    this.error.set('');
    try {
      this.request.set(await this.api.applicationTokenRequest(this.code));
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.loading.set(false);
    }
  }

  protected async decide(approve: boolean): Promise<void> {
    if (this.busy()) return;
    this.busy.set(true);
    this.error.set('');
    try {
      await this.api.decideApplicationToken(this.code, approve);
      this.approved.set(approve);
      this.done.set(true);
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }
}
