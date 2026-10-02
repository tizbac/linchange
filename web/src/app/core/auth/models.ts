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

// Me is GET /api/v1/auth/me. The client reads the impersonation target from
// here rather than decoding the access token: a token is a signed blob, and
// making the UI parse one to decide whether to show the "acting as" banner
// means the banner and the server can disagree about the same session.
export interface Me {
  address: string;
  display_name?: string;
  source: string;
  enabled: boolean;
  mailbox_enabled: boolean;
  is_domain_admin: boolean;
  impersonating?: string;
  must_change_password?: boolean;
  // Whether this deployment requires an impersonation_grants row. The
  // administration page reads it rather than assuming: with it on the grant
  // list is the allowlist, with it off the same list is only a record.
  impersonation_requires_grant?: boolean;
}
