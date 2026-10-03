import { Routes } from '@angular/router';
import { onlyGuests, requireAuth, requireDomainAdmin } from './core/auth/auth.guard';
import { DashboardComponent } from './dashboard/dashboard.component';
import { ForgotPasswordComponent } from './forgot-password/forgot-password.component';
import { LoginComponent } from './login/login.component';
import { ResetPasswordComponent } from './reset-password/reset-password.component';
import { SecondFactorComponent } from './second-factor/second-factor.component';
import { MailComponent } from './mail/mail.component';
import { MailComposeComponent } from './mail/mail.compose.component';
import { MailRulesComponent } from './mail/mail.rules.component';
import { AdminAliasesComponent } from './admin/admin-aliases.component';
import { AdminAuditComponent } from './admin/admin-audit.component';
import { AdminDomainsComponent } from './admin/admin-domains.component';
import { AdminImpersonationComponent } from './admin/admin-impersonation.component';
import { AdminSharedMailboxesComponent } from './admin/admin-shared-mailboxes.component';
import { AdminUsersComponent } from './admin/admin-users.component';

export const routes: Routes = [
  { path: '', pathMatch: 'full', component: DashboardComponent, canActivate: [requireAuth] },
  { path: 'dashboard', component: DashboardComponent, canActivate: [requireAuth] },
  {
    path: 'mail',
    canActivate: [requireAuth],
    children: [
      { path: '', pathMatch: 'full', component: MailComponent },
      { path: 'compose', component: MailComposeComponent },
      { path: 'rules', component: MailRulesComponent },
    ],
  },
  { path: 'login', component: LoginComponent, canActivate: [onlyGuests] },
  { path: 'second-factor', component: SecondFactorComponent, canActivate: [onlyGuests] },
  { path: 'forgot-password', component: ForgotPasswordComponent, canActivate: [onlyGuests] },
  { path: 'reset-password', component: ResetPasswordComponent },
  {
    path: 'admin',
    canActivate: [requireDomainAdmin],
    children: [
      { path: '', pathMatch: 'full', redirectTo: 'domains' },
      { path: 'domains', component: AdminDomainsComponent },
      { path: 'users', component: AdminUsersComponent },
      { path: 'aliases', component: AdminAliasesComponent },
      { path: 'shared-mailboxes', component: AdminSharedMailboxesComponent },
      { path: 'impersonation', component: AdminImpersonationComponent },
      { path: 'audit', component: AdminAuditComponent },
      { path: '**', redirectTo: 'domains' },
    ],
  },
  { path: '**', redirectTo: '' },
];
