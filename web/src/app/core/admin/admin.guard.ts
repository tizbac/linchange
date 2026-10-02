import { inject } from '@angular/core';
import { CanActivateFn, Router } from '@angular/router';
import { AuthService } from '../auth/auth.service';

// requireDomainAdmin guards the administration routes.
//
// The server checks this too, and it is the server that is authoritative: this
// guard exists so an ordinary user gets the dashboard rather than a page of
// failed requests, not so that the pages are protected. A grant can be revoked
// between this check and the API call, which is why the client is not the wall.
export const requireDomainAdmin: CanActivateFn = async () => {
  const auth = inject(AuthService);
  const router = inject(Router);
  if (!auth.isAuthenticated()) return router.parseUrl('/login');
  const me = auth.me() ?? (await auth.loadMe());
  if (!me?.is_domain_admin) return router.parseUrl('/');
  return true;
};
