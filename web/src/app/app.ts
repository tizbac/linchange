import { Component, OnInit, inject, signal } from '@angular/core';
import { Router, RouterLink, RouterOutlet } from '@angular/router';
import { AuthService } from './core/auth/auth.service';

@Component({
  selector: 'app-root',
  imports: [RouterOutlet, RouterLink],
  templateUrl: './app.html',
})
export class App implements OnInit {
  protected readonly auth = inject(AuthService);
  private readonly router = inject(Router);
  protected readonly ready = signal(false);

  ngOnInit(): void {
    // Wait for a prior session to be restored (or ruled out) before letting the
    // router decide where an unauthenticated-looking visitor should go.
    this.auth.bootstrap().then(() => this.ready.set(true));
  }

  protected async signOut(): Promise<void> {
    await this.auth.logout();
    this.router.navigateByUrl('/login');
  }

  protected async stopImpersonating(): Promise<void> {
    await this.auth.stopImpersonating();
  }
}
