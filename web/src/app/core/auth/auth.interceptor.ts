import { HttpErrorResponse, HttpEvent, HttpHandlerFn, HttpInterceptorFn, HttpRequest } from '@angular/common/http';
import { inject } from '@angular/core';
import { Observable, catchError, from, switchMap, throwError } from 'rxjs';
import { AuthService } from './auth.service';

// Signs every non-login API call with the current access token and transparently
// renews the session once on a 401 before surfacing the failure.
export const authInterceptor: HttpInterceptorFn = (req, next) => {
  const auth = inject(AuthService);

  // Login and second-factor calls carry no bearer; everything else does.
  const isCredentialStep =
    req.url.endsWith('/auth/login') || req.url.endsWith('/auth/second-factor');
  const state = auth.state();
  if (isCredentialStep || !state) {
    return next(req);
  }

  const withToken = (token: string) =>
    req.clone({ setHeaders: { Authorization: `Bearer ${token}` } });

  return next(withToken(state.accessToken)).pipe(
    catchError((err) => {
      if (err instanceof HttpErrorResponse && err.status === 401) {
        return from(auth.refreshOnce()).pipe(
          switchMap(() => {
            const renewed = auth.state()?.accessToken ?? '';
            return next(withToken(renewed));
          }),
          catchError(() => throwError(() => err)),
        );
      }
      return throwError(() => err);
    }),
  );
};
