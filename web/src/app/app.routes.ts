import { Routes } from '@angular/router';
import { onlyGuests, requireAuth } from './core/auth/auth.guard';
import { DashboardComponent } from './dashboard/dashboard.component';
import { ForgotPasswordComponent } from './forgot-password/forgot-password.component';
import { LoginComponent } from './login/login.component';
import { ResetPasswordComponent } from './reset-password/reset-password.component';
import { SecondFactorComponent } from './second-factor/second-factor.component';

export const routes: Routes = [
  { path: '', pathMatch: 'full', component: DashboardComponent, canActivate: [requireAuth] },
  { path: 'dashboard', component: DashboardComponent, canActivate: [requireAuth] },
  { path: 'login', component: LoginComponent, canActivate: [onlyGuests] },
  { path: 'second-factor', component: SecondFactorComponent, canActivate: [onlyGuests] },
  { path: 'forgot-password', component: ForgotPasswordComponent, canActivate: [onlyGuests] },
  { path: 'reset-password', component: ResetPasswordComponent },
  { path: '**', redirectTo: '' },
];
