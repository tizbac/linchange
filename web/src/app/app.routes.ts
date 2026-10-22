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
    title: $localize`:@@title.dashboard:Dashboard`,
  },
  // The signed-in landing page has a name of its own: sign-in, the second
  // factor, and the admin guard all send the browser here, so the URL says
  // where the user is instead of pretending to be the site root.
  {
    path: 'dashboard',
    component: DashboardComponent,
    canActivate: [requireAuth],
    title: $localize`:@@title.dashboard:Dashboard`,
  },
  {
    path: 'mail',
    canActivate: [requireAuth],
    children: [
      {
        path: '',
        pathMatch: 'full',
        loadComponent: () => import('./mail/mail.component').then((m) => m.MailComponent),
        title: $localize`:@@title.mail:Mail`,
      },
      {
        path: 'compose',
        loadComponent: () => import('./mail/mail.compose.component').then((m) => m.MailComposeComponent),
        title: $localize`:@@title.compose:Compose`,
      },
      {
        path: 'rules',
        loadComponent: () => import('./mail/mail.rules.component').then((m) => m.MailRulesComponent),
        title: $localize`:@@title.rules:Rules`,
      },
      {
        path: 'signature',
        loadComponent: () => import('./settings/signature.component').then((m) => m.SignatureComponent),
        title: $localize`:@@title.signature:Signature`,
      },
      {
        path: 'import',
        loadComponent: () => import('./settings/import.component').then((m) => m.ImportComponent),
        title: $localize`:@@title.import:Import`,
      },
    ],
  },
  {
    path: 'calendar',
    canActivate: [requireAuth],
    loadComponent: () => import('./calendar/calendar.component').then((m) => m.CalendarComponent),
    title: $localize`:@@title.calendar:Calendar`,
  },
  {
    path: 'contacts',
    canActivate: [requireAuth],
    loadComponent: () => import('./contacts/contacts.component').then((m) => m.ContactsComponent),
    title: $localize`:@@title.contacts:Contacts`,
  },
  {
    path: 'meetings',
    canActivate: [requireAuth],
    loadComponent: () => import('./meetings/meetings.component').then((m) => m.MeetingsComponent),
    title: $localize`:@@title.meetings:Meetings`,
  },
  {
    path: 'profile',
    canActivate: [requireAuth],
    loadComponent: () => import('./profile/profile.component').then((m) => m.ProfileComponent),
    title: $localize`:@@title.profile:Profile`,
  },
  // The call itself is its own full-screen page, so the meeting details can
  // stay readable and the gallery has the whole viewport.
  {
    path: 'meetings/:uid/room',
    loadComponent: () =>
      import('./meetings/call-room.component').then((m) => m.CallRoomComponent),
    title: $localize`:@@title.room:Meeting room`,
  },
  // The room page is deliberately unguarded: an invitee answers from the link
  // in their invitation with no account here. The page works out which visitor
  // it has from the token in the query, and a signed-out visitor without one
  // is pointed at the login page from inside it.
  {
    path: 'meetings/:uid',
    loadComponent: () =>
      import('./meetings/meeting-room.component').then((m) => m.MeetingRoomComponent),
    title: $localize`:@@title.meeting:Meeting`,
  },
  {
    path: 'login',
    component: LoginComponent,
    canActivate: [onlyGuests],
    title: $localize`:@@title.login:Sign in`,
  },
  {
    path: 'second-factor',
    component: SecondFactorComponent,
    canActivate: [onlyGuests],
    title: $localize`:@@title.secondFactor:Two-factor authentication`,
  },
  {
    path: 'forgot-password',
    component: ForgotPasswordComponent,
    canActivate: [onlyGuests],
    title: $localize`:@@title.forgotPassword:Forgot password`,
  },
  {
    path: 'reset-password',
    component: ResetPasswordComponent,
    title: $localize`:@@title.resetPassword:Reset password`,
  },
  {
    path: 'admin',
    canActivate: [requireDomainAdmin],
    children: [
      { path: '', pathMatch: 'full', redirectTo: 'domains' },
      {
        path: 'workgroups',
        loadComponent: () => import('./admin/admin-workgroups.component').then((m) => m.AdminWorkgroupsComponent),
        title: $localize`:@@title.adminOrganizations:Organizations`,
      },
      {
        path: 'workgroups/:id',
        loadComponent: () =>
          import('./admin/admin-workgroup-manage.component').then((m) => m.AdminWorkgroupManageComponent),
        title: $localize`:@@title.adminOrganization:Organization`,
      },
      {
        path: 'themes',
        loadComponent: () => import('./admin/admin-themes.component').then((m) => m.AdminThemesComponent),
        title: $localize`:@@title.adminThemes:Themes`,
      },
      {
        path: 'domains',
        loadComponent: () => import('./admin/admin-domains.component').then((m) => m.AdminDomainsComponent),
        title: $localize`:@@title.adminDomains:Domains`,
      },
      {
        path: 'users',
        loadComponent: () => import('./admin/admin-users.component').then((m) => m.AdminUsersComponent),
        title: $localize`:@@title.adminUsers:Users`,
      },
      {
        path: 'users/:address',
        loadComponent: () =>
          import('./admin/admin-user-edit.component').then((m) => m.AdminUserEditComponent),
        title: $localize`:@@title.adminUserEdit:Edit mailbox`,
      },
      {
        path: 'aliases',
        loadComponent: () => import('./admin/admin-aliases.component').then((m) => m.AdminAliasesComponent),
        title: $localize`:@@title.adminAliases:Aliases`,
      },
      {
        path: 'shared-mailboxes',
        loadComponent: () =>
          import('./admin/admin-shared-mailboxes.component').then((m) => m.AdminSharedMailboxesComponent),
        title: $localize`:@@title.adminSharedMailboxes:Shared mailboxes`,
      },
      {
        path: 'impersonation',
        loadComponent: () =>
          import('./admin/admin-impersonation.component').then((m) => m.AdminImpersonationComponent),
        title: $localize`:@@title.adminImpersonation:Impersonation`,
      },
      {
        path: 'queue',
        loadComponent: () => import('./admin/admin-queue.component').then((m) => m.AdminQueueComponent),
        title: $localize`:@@title.adminQueue:Outbound queue`,
      },
      {
        path: 'audit',
        loadComponent: () => import('./admin/admin-audit.component').then((m) => m.AdminAuditComponent),
        title: $localize`:@@title.adminAudit:Audit log`,
      },
      {
        path: 'imports',
        loadComponent: () => import('./admin/admin-imports.component').then((m) => m.AdminImportsComponent),
        title: $localize`:@@title.adminImports:Imports`,
      },
      { path: '**', redirectTo: 'domains' },
    ],
  },
  { path: '**', redirectTo: '' },
];
