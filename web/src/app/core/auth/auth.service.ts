import { HttpClient } from '@angular/common/http';
import { Injectable, computed, inject, signal } from '@angular/core';
import { firstValueFrom } from 'rxjs';
import { AuthState, LoginResponse, Me, ThemeOption } from './models';

// The API lives on the same origin in production and is proxied there by the
// dev server, so relative paths are all the app ever needs.
const API = '/api/v1';
const K_REFRESH = 'linchange.refresh';
const K_USER = 'linchange.user';

const BUILTIN_THEMES: ThemeOption[] = [
  { key: 'light', name: 'Light', kind: 'light' },
  { key: 'dark', name: 'Dark', kind: 'dark' },
  { key: 'system', name: 'System', kind: 'system' },
];

// AuthService owns the signed-in session for the tab and is the single place
// that talks to the auth endpoints. Other code reads the public signals; only
// this service mutates them.
@Injectable({ providedIn: 'root' })
export class AuthService {
  private readonly http = inject(HttpClient);

  private readonly _state = signal<AuthState | null>(null);

  private challenge: { id: string; expiresAt: string; user: string } | null = null;
  private refreshPromise: Promise<void> | null = null;
  private bootstrapPromise: Promise<void> | null = null;

  readonly state = this._state.asReadonly();
  readonly isAuthenticated = computed(() => this._state() !== null);
  readonly user = computed(() => this._state()?.user ?? '');
  readonly mustChangePassword = computed(() => this._state()?.mustChangePassword ?? false);

  // me is what the server says about this session. It is separate from the
  // signed-in state because it can change without a new sign-in: ending an
  // impersonation revokes the session, and a refresh can change nothing else.
  private readonly _me = signal<Me | null>(null);
  readonly me = this._me.asReadonly();
  readonly isDomainAdmin = computed(() => this._me()?.is_domain_admin ?? false);
  readonly impersonating = computed(() => this._me()?.impersonating ?? '');
  readonly impersonationRequiresGrant = computed(
    () => this._me()?.impersonation_requires_grant ?? false,
  );
  // organization is the name of the workgroup the signed-in address belongs to.
  // It is empty when the domain belongs to no organization, which is the
  // ordinary case for a deployment that never created one.
  readonly organization = computed(() => this._me()?.organization ?? '');
  // organizationLogo is the public path of the organization's logo, or empty
  // when there is none and the top bar should show the name.
  readonly organizationLogo = computed(() => this._me()?.organization_logo ?? '');
  // theme is the resolved choice; themeOptions is what a selector offers.
  readonly theme = computed(() => this._me()?.theme ?? 'system');
  readonly themeKind = computed(() => this._me()?.theme_kind ?? 'system');
  readonly themeOptions = computed<ThemeOption[]>(
    () => this._me()?.theme_options ?? BUILTIN_THEMES,
  );

  constructor() {
    // A system theme has to react when the operating system changes, not only
    // when the page is reloaded.
    if (typeof window !== 'undefined' && window.matchMedia) {
      window.matchMedia('(prefers-color-scheme: dark)').addEventListener('change', () => {
        if ((this._me()?.theme_kind ?? 'system') === 'system') this.applyTheme();
      });
    }
  }

  // loadMe refreshes the session description. It is awaited after a sign-in and
  // after a restore so a guard never decides on a stale answer, and it is also
  // callable on its own to pick up a server-side change.
  async loadMe(): Promise<Me | null> {
    if (!this.isAuthenticated()) {
      this._me.set(null);
      return null;
    }
    try {
      const me = await firstValueFrom(this.http.get<Me>(`${API}/auth/me`));
      this._me.set(me);
      this.applyTheme();
      return me;
    } catch {
      // A failure here is not a reason to sign the user out: the token may
      // still be good, and /me is decoration on top of it. Treating a failed
      // lookup as a lost session would sign people out for a flaky request.
      this._me.set(null);
      this.applyTheme();
      return null;
    }
  }

  // setTheme records the user's choice and applies it at once. The server
  // returns the resolved selection, which may be a custom theme's CSS path.
  async setTheme(key: string): Promise<void> {
    const res = await firstValueFrom(
      this.http.put<{ key: string; kind: 'light' | 'dark' | 'system' | 'custom'; css?: string }>(
        `${API}/auth/me/theme`,
        { theme: key },
      ),
    );
    const me = this._me();
    if (me) {
      this._me.set({ ...me, theme: res.key, theme_kind: res.kind, theme_css: res.css });
    }
    this.applyTheme();
  }

  // applyTheme puts the resolved theme on the document: a data-theme attribute
  // for the built-in light/dark rules, and a linked stylesheet for a custom
  // theme. "system" is resolved here so the CSS only needs two cases.
  applyTheme(): void {
    if (typeof document === 'undefined') return;
    const kind = this._me()?.theme_kind ?? 'system';
    const resolved =
      kind === 'system'
        ? window.matchMedia?.('(prefers-color-scheme: dark)')?.matches
          ? 'dark'
          : 'light'
        : kind;
    document.documentElement.dataset['theme'] = resolved;

    const css = this._me()?.theme_css ?? '';
    let link = document.getElementById('lc-theme-css') as HTMLLinkElement | null;
    if (css) {
      if (!link) {
        link = document.createElement('link');
        link.id = 'lc-theme-css';
        link.rel = 'stylesheet';
        document.head.appendChild(link);
      }
      if (link.getAttribute('href') !== css) link.href = css;
    } else if (link) {
      link.remove();
    }
  }

  // stopImpersonating ends an impersonating session. The server revokes it, so
  // the stored tokens are dead and the caller signs in again as itself.
  async stopImpersonating(): Promise<void> {
    await firstValueFrom(this.http.post(`${API}/auth/stop-impersonating`, {}));
    this.clear();
  }

  // bootstrap restores a prior session from a stored refresh token. It runs once
  // at startup and resolves when the restore has settled, so the root component
  // can wait on it before deciding whether to bounce an unauthenticated visitor.
  bootstrap(): Promise<void> {
    if (!this.bootstrapPromise) {
      this.bootstrapPromise = new Promise<void>((resolve) => {
        const rt = this.storage.getItem(K_REFRESH);
        if (rt) {
          this.restore(rt)
            .catch(() => this.clear())
            .finally(resolve);
        } else {
          resolve();
        }
      });
    }
    return this.bootstrapPromise;
  }

  hasChallenge(): boolean {
    return this.challenge !== null;
  }

  private consumeChallenge(): { id: string; expiresAt: string; user: string } | null {
    const c = this.challenge;
    this.challenge = null;
    return c;
  }

  // login runs the password step. It resolves true when a second factor is
  // still required, false when the session is already complete.
  async login(login: string, password: string): Promise<boolean> {
    const res = await this.post<LoginResponse>('/auth/login', { login, password });
    if (res.requires_second_factor && res.challenge_id) {
      this.challenge = {
        id: res.challenge_id,
        expiresAt: res.challenge_expires_at ?? '',
        user: login,
      };
      return true;
    }
    this.adopt(res, login);
    return false;
  }

  async secondFactor(code: string): Promise<void> {
    const c = this.consumeChallenge();
    if (!c) throw new Error('no pending second factor');
    const res = await this.post<LoginResponse>('/auth/second-factor', {
      challenge_id: c.id,
      code,
    });
    this.adopt(res, c.user);
  }

  // refreshOnce renews the session once; concurrent 401s share a single call so
  // the refresh token is not rotated twice at the same moment.
  refreshOnce(): Promise<void> {
    if (!this.refreshPromise) {
      const rt = this.storage.getItem(K_REFRESH) ?? '';
      this.refreshPromise = this.restore(rt)
        .catch(() => {
          this.clear();
          throw new Error('session expired');
        })
        .finally(() => {
          this.refreshPromise = null;
        });
    }
    return this.refreshPromise;
  }

  async logout(): Promise<void> {
    if (this._state()) {
      // The access token is still set here, so the interceptor signs the call.
      await this.post<void>('/auth/logout', {}).catch(() => undefined);
    }
    this.clear();
  }

  clear(): void {
    this.storage.removeItem(K_REFRESH);
    this.storage.removeItem(K_USER);
    this._state.set(null);
    this._me.set(null);
    this.applyTheme();
  }

  requestCaptcha(): Promise<{ token: string; image: string; expires_at: string }> {
    return this.get('/auth/captcha');
  }

  requestReset(login: string, captchaToken: string, captchaAnswer: string): Promise<unknown> {
    return this.post('/auth/reset/request', {
      login,
      captcha_token: captchaToken,
      captcha_answer: captchaAnswer,
    });
  }

  completeReset(token: string, password: string): Promise<unknown> {
    return this.post('/auth/reset/complete', { token, password });
  }

  // adopt takes the tokens and then asks who the session is. The address typed
  // on the form is not trusted for this: the server canonicalises it, and only
  // the server knows whether the session is impersonating.
  private adopt(res: LoginResponse, user: string): void {
    if (!res.access_token || !res.refresh_token) {
      throw new Error('the server did not issue tokens');
    }
    const state: AuthState = {
      user,
      mustChangePassword: !!res.must_change_password,
      accessToken: res.access_token,
      refreshToken: res.refresh_token,
    };
    this.storage.setItem(K_REFRESH, state.refreshToken);
    this.storage.setItem(K_USER, state.user);
    this._state.set(state);
    void this.loadMe();
  }

  private restore(rt: string): Promise<void> {
    return this.post<LoginResponse>('/auth/refresh', { refresh_token: rt }).then((res) => {
      const user = this.storage.getItem(K_USER) ?? '';
      this.storage.setItem(K_REFRESH, res.refresh_token ?? '');
      this._state.set({
        user,
        mustChangePassword: !!res.must_change_password,
        accessToken: res.access_token ?? '',
        refreshToken: res.refresh_token ?? '',
      });
      void this.loadMe();
    });
  }

  private get<T>(path: string): Promise<T> {
    return firstValueFrom(this.http.get<T>(`${API}${path}`));
  }

  private post<T>(path: string, body: unknown): Promise<T> {
    return firstValueFrom(this.http.post<T>(`${API}${path}`, body));
  }

  private get storage(): Storage {
    // localStorage, not sessionStorage: the call room opens in a new tab and
    // has to find the same session there. The refresh token is already a
    // long-lived bearer credential, so the XSS exposure is the same; the
    // difference is only whether a second tab works.
    return localStorage;
  }
}
