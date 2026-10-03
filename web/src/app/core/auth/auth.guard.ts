import { inject } from '@angular/core';
import { CanActivateFn, Router } from '@angular/router';
import { AuthService } from './auth.service';

// requireAuth guards the signed-in routes, sending guests to the login page.
//
// Every guard awaits auth.bootstrap() before reading auth state: the router's
// initial navigation can reach a guard before a stored refresh token has been
// restored, which would otherwise read as "signed out" and bounce a returning
// visitor to the login page.
export const requireAuth: CanActivateFn = async () => {
  const auth = inject(AuthService);
  const router = inject(Router);
  await auth.bootstrap();
  return auth.isAuthenticated() ? true : router.parseUrl('/login');
};

// onlyGuests keeps signed-in users off the login and second-factor pages.
export const onlyGuests: CanActivateFn = async () => {
  const auth = inject(AuthService);
  const router = inject(Router);
  await auth.bootstrap();
  return auth.isAuthenticated() ? router.parseUrl('/') : true;
};

// requireDomainAdmin guards the admin panel so only a domain admin can reach it.
// A user who is merely a normal mailbox owner or who has an expired session is
// sent back to the dashboard, where the link is hidden.
export const requireDomainAdmin: CanActivateFn = async () => {
  const auth = inject(AuthService);
  const router = inject(Router);
  await auth.bootstrap();
  return auth.isDomainAdmin() ? true : router.parseUrl('/dashboard');
};
