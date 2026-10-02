import { inject } from '@angular/core';
import { CanActivateFn, Router } from '@angular/router';
import { AuthService } from './auth.service';

// requireAuth guards the signed-in routes, sending guests to the login page.
export const requireAuth: CanActivateFn = () => {
  const auth = inject(AuthService);
  const router = inject(Router);
  return auth.isAuthenticated() ? true : router.parseUrl('/login');
};

// onlyGuests keeps signed-in users off the login and second-factor pages.
export const onlyGuests: CanActivateFn = () => {
  const auth = inject(AuthService);
  const router = inject(Router);
  return auth.isAuthenticated() ? router.parseUrl('/') : true;
};

// requireDomainAdmin guards the admin panel so only a domain admin can reach it.
// A user who is merely a normal mailbox owner or who has an expired session is
// sent back to the dashboard, where the link is hidden.
export const requireDomainAdmin: CanActivateFn = () => {
  const auth = inject(AuthService);
  const router = inject(Router);
  return auth.isDomainAdmin() ? true : router.parseUrl('/dashboard');
};
