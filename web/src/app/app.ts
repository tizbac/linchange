import { Component, LOCALE_ID, OnInit, inject, signal } from '@angular/core';
import { Router, RouterLink, RouterLinkActive, RouterOutlet } from '@angular/router';
import { AuthService } from './core/auth/auth.service';
import { ThemeOption } from './core/auth/models';

@Component({
  selector: 'app-root',
  imports: [RouterOutlet, RouterLink, RouterLinkActive],
  templateUrl: './app.html',
})
export class App implements OnInit {
  protected readonly auth = inject(AuthService);
  private readonly router = inject(Router);
  protected readonly ready = signal(false);
  // The locale the app was built for. Angular's i18n is compile-time, so
  // switching means loading the other build rather than swapping strings.
  protected readonly locale = inject(LOCALE_ID);

  ngOnInit(): void {
    // The theme is applied before the shell is shown, and again once /me has
    // supplied the resolved choice.
    this.auth.applyTheme();
    // Wait for a prior session to be restored (or ruled out) before letting the
    // router decide where an unauthenticated-looking visitor should go.
    this.auth.bootstrap().then(() => {
      this.auth.applyTheme();
      this.ready.set(true);
    });
  }

  protected changeTheme(event: Event): void {
    void this.auth.setTheme((event.target as HTMLSelectElement).value).catch(() => undefined);
  }

  // Custom themes already carry their chosen name; the built-ins are
  // translated here so a theme selector reads naturally in either language.
  protected themeLabel(option: ThemeOption): string {
    switch (option.kind) {
      case 'light':
        return $localize`:@@theme.light:Light`;
      case 'dark':
        return $localize`:@@theme.dark:Dark`;
      case 'system':
        return $localize`:@@theme.system:System`;
    }
    return option.name;
  }

  // switchLanguage keeps the current page and moves to the same route in the
  // other locale's build. A cookie lets the server send the bare "/" there on
  // the next visit.
  protected switchLanguage(language: string): void {
    if (language !== 'en' && language !== 'it') return;
    if (language === this.locale) return;
    const path = window.location.pathname;
    const bare = path.replace(/^\/(?:en|it)(?=\/|$)/, '') || '/';
    const target = `/${language}${bare === '/' ? '/' : bare}`;
    document.cookie = `linchange_lang=${language}; path=/; max-age=31536000; samesite=lax`;
    window.location.assign(target || '/');
  }

  protected async signOut(): Promise<void> {
    await this.auth.logout();
    this.router.navigateByUrl('/login');
  }

  protected async stopImpersonating(): Promise<void> {
    await this.auth.stopImpersonating();
  }
}
