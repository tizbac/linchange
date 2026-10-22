import { HttpClient } from '@angular/common/http';
import { Injectable, inject } from '@angular/core';
import { firstValueFrom } from 'rxjs';

const API = '/api/v1';

export interface ProfileTwoFactor {
  available: boolean;
  enabled: boolean;
  pending: boolean;
}

export interface Profile {
  address: string;
  display_name?: string;
  source: string;
  recovery_email?: string;
  min_password_length: number;
  two_factor: ProfileTwoFactor;
}

export interface TwoFactorStart {
  qr: string;
  secret: string;
  uri: string;
  recovery_codes: string[];
}

// ProfileApi is the signed-in user's own account surface: password, recovery
// address, and second-factor enrolment. Nothing here can name another user.
@Injectable({ providedIn: 'root' })
export class ProfileApi {
  private readonly http = inject(HttpClient);

  get(): Promise<Profile> {
    return firstValueFrom(this.http.get<Profile>(`${API}/profile`));
  }

  changePassword(currentPassword: string, newPassword: string): Promise<void> {
    return firstValueFrom(
      this.http.put<void>(`${API}/profile/password`, {
        current_password: currentPassword,
        new_password: newPassword,
      }),
    );
  }

  setRecoveryEmail(email: string): Promise<void> {
    return firstValueFrom(this.http.put<void>(`${API}/profile/recovery-email`, { email }));
  }

  start2FA(): Promise<TwoFactorStart> {
    return firstValueFrom(this.http.post<TwoFactorStart>(`${API}/profile/2fa/start`, {}));
  }

  confirm2FA(code: string): Promise<void> {
    return firstValueFrom(this.http.post<void>(`${API}/profile/2fa/confirm`, { code }));
  }

  disable2FA(code: string): Promise<void> {
    return firstValueFrom(this.http.post<void>(`${API}/profile/2fa/disable`, { code }));
  }
}
