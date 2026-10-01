import { HttpClient } from '@angular/common/http';
import { Injectable, computed, inject, signal } from '@angular/core';
import { firstValueFrom } from 'rxjs';
import { AuthState, LoginResponse } from './models';

// The API lives on the same origin in production and is proxied there by the
// dev server, so relative paths are all the app ever needs.
const API = '/api/v1';
const K_REFRESH = 'linchange.refresh';
const K_USER = 'linchange.user';

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
  }

  requestReset(login: string): Promise<unknown> {
    return this.post('/auth/reset/request', { login });
  }

  completeReset(token: string, password: string): Promise<unknown> {
    return this.post('/auth/reset/complete', { token, password });
  }

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
    });
  }

  private post<T>(path: string, body: unknown): Promise<T> {
    return firstValueFrom(this.http.post<T>(`${API}${path}`, body));
  }

  private get storage(): Storage {
    return sessionStorage;
  }
}
