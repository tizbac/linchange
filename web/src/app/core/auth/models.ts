// Shapes the /api/v1/auth endpoints exchange. They mirror the JSON the Go
// server emits, so a rename on either side breaks the build here first.
export interface LoginResponse {
  requires_second_factor?: boolean;
  challenge_id?: string;
  challenge_expires_at?: string;
  token_type?: string;
  access_token?: string;
  refresh_token?: string;
  expires_in?: number;
  must_change_password?: boolean;
}

export interface AuthState {
  user: string;
  mustChangePassword: boolean;
  accessToken: string;
  refreshToken: string;
}
