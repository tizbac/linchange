import { HttpClient } from '@angular/common/http';
import { Injectable, inject } from '@angular/core';
import { firstValueFrom } from 'rxjs';

const API = '/api/v1';

export interface ProfileTwoFactor {
  available: boolean;
  enabled: boolean;
  pending: boolean;
}

export interface OutOfOffice {
  enabled: boolean;
  start?: string;
  end?: string;
  subject: string;
  message: string;
}

export interface Profile {
  address: string;
  display_name?: string;
  source: string;
  recovery_email?: string;
  min_password_length: number;
  two_factor: ProfileTwoFactor;
  out_of_office: OutOfOffice;
}

export interface TwoFactorStart {
  qr: string;
  secret: string;
  uri: string;
  recovery_codes: string[];
}

export interface ApplicationToken {
  id: number;
  prefix: string;
  description: string;
  created_at: string;
  last_used_at?: string;
}

export interface CreatedApplicationToken {
  token: string;
  description: string;
  created_at: string;
}

export interface ApplicationTokenRequest {
  user_code: string;
  description: string;
  status: 'pending' | 'approved' | 'denied' | 'consumed';
  expires_at: string;
}

// ProfileApi is the signed-in user's own account surface: password, recovery
// address, second-factor enrolment and application passwords. Nothing here can
// name another user.
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

  setOutOfOffice(setting: OutOfOffice): Promise<OutOfOffice> {
    return firstValueFrom(
      this.http.put<OutOfOffice>(`${API}/profile/out-of-office`, {
        enabled: setting.enabled,
        start: setting.start ?? null,
        end: setting.end ?? null,
        subject: setting.subject,
        message: setting.message,
      }),
    );
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

  applicationTokens(): Promise<{ tokens: ApplicationToken[] }> {
    return firstValueFrom(this.http.get<{ tokens: ApplicationToken[] }>(`${API}/profile/application-tokens`));
  }

  createApplicationToken(description: string): Promise<CreatedApplicationToken> {
    return firstValueFrom(
      this.http.post<CreatedApplicationToken>(`${API}/profile/application-tokens`, { description }),
    );
  }

  revokeApplicationToken(id: number): Promise<void> {
    return firstValueFrom(this.http.delete<void>(`${API}/profile/application-tokens/${id}`));
  }

  applicationTokenRequest(code: string): Promise<ApplicationTokenRequest> {
    return firstValueFrom(
      this.http.get<ApplicationTokenRequest>(
        `${API}/profile/application-tokens/request/${encodeURIComponent(code)}`,
      ),
    );
  }

  decideApplicationToken(code: string, approve: boolean): Promise<{ status: string }> {
    return firstValueFrom(
      this.http.post<{ status: string }>(
        `${API}/profile/application-tokens/request/${encodeURIComponent(code)}`,
        { approve },
      ),
    );
  }
}
