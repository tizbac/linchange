import { inject } from '@angular/core';
import { ActivatedRouteSnapshot, CanActivateFn, Router, RouterStateSnapshot } from '@angular/router';
import { AuthService } from './auth.service';

// requireAuth guards the signed-in routes, sending guests to the login page.
//
// Every guard awaits auth.bootstrap() before reading auth state: the router's
// initial navigation can reach a guard before a stored refresh token has been
// restored, which would otherwise read as "signed out" and bounce a returning
// visitor to the login page. The page the visitor wanted is kept, so a sign-in
// from a link (an application-password approval, say) comes back there.
export const requireAuth: CanActivateFn = async (
  _route: ActivatedRouteSnapshot,
  state: RouterStateSnapshot,
) => {
  const auth = inject(AuthService);
  const router = inject(Router);
  await auth.bootstrap();
  if (auth.isAuthenticated()) return true;
  return router.parseUrl(`/login?returnUrl=${encodeURIComponent(state.url)}`);
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
