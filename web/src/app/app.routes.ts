import { Routes } from '@angular/router';
import { onlyGuests, requireAuth, requireDomainAdmin } from './core/auth/auth.guard';
import { DashboardComponent } from './dashboard/dashboard.component';
import { ForgotPasswordComponent } from './forgot-password/forgot-password.component';
import { LoginComponent } from './login/login.component';
import { ResetPasswordComponent } from './reset-password/reset-password.component';
import { SecondFactorComponent } from './second-factor/second-factor.component';

// The pages a signed-out visitor can reach are imported eagerly, because they are
// the first thing anybody loads and they are small.
//
// Everything else is behind loadComponent, so each page arrives when it is
// navigated to. That is not only about the pages that are rarely visited: the
// composer and the signature editor both carry a rich-text engine, which is the
// largest dependency in the application, and a user who never writes a message
// should not pay for it in the first page they open.
export const routes: Routes = [
  {
    path: '',
    pathMatch: 'full',
    component: DashboardComponent,
    canActivate: [requireAuth],
  },
  {
    path: 'mail',
    canActivate: [requireAuth],
    children: [
      {
        path: '',
        pathMatch: 'full',
        loadComponent: () => import('./mail/mail.component').then((m) => m.MailComponent),
      },
      {
        path: 'compose',
        loadComponent: () => import('./mail/mail.compose.component').then((m) => m.MailComposeComponent),
      },
      {
        path: 'rules',
        loadComponent: () => import('./mail/mail.rules.component').then((m) => m.MailRulesComponent),
      },
      {
        path: 'signature',
        loadComponent: () => import('./settings/signature.component').then((m) => m.SignatureComponent),
      },
      {
        path: 'import',
        loadComponent: () => import('./settings/import.component').then((m) => m.ImportComponent),
      },
    ],
  },
  {
    path: 'calendar',
    canActivate: [requireAuth],
    loadComponent: () => import('./calendar/calendar.component').then((m) => m.CalendarComponent),
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
      {
        path: 'domains',
        loadComponent: () => import('./admin/admin-domains.component').then((m) => m.AdminDomainsComponent),
      },
      {
        path: 'users',
        loadComponent: () => import('./admin/admin-users.component').then((m) => m.AdminUsersComponent),
      },
      {
        path: 'aliases',
        loadComponent: () => import('./admin/admin-aliases.component').then((m) => m.AdminAliasesComponent),
      },
      {
        path: 'shared-mailboxes',
        loadComponent: () =>
          import('./admin/admin-shared-mailboxes.component').then((m) => m.AdminSharedMailboxesComponent),
      },
      {
        path: 'impersonation',
        loadComponent: () =>
          import('./admin/admin-impersonation.component').then((m) => m.AdminImpersonationComponent),
      },
      {
        path: 'audit',
        loadComponent: () => import('./admin/admin-audit.component').then((m) => m.AdminAuditComponent),
      },
      {
        path: 'imports',
        loadComponent: () => import('./admin/admin-imports.component').then((m) => m.AdminImportsComponent),
      },
      { path: '**', redirectTo: 'domains' },
    ],
  },
  { path: '**', redirectTo: '' },
];
