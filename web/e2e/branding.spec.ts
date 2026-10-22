import { Page, expect, test } from '@playwright/test';

// An organization's logo replaces the "name - linchange" wordmark in the top
// bar and is shown at the top of the conference room. Only an administrator
// can upload one.
const USER = process.env.E2E_USER ?? 'e2e-user@example.com';
const PASS = process.env.E2E_PASSWORD ?? 'correct horse battery staple';
const GUEST = 'e2e-other@example.com';
const RUN = Date.now().toString(36);

// A 1x1 transparent PNG: enough for an <img> to load.
const PNG = Buffer.from(
  'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==',
  'base64',
);

async function signIn(page: Page, address: string): Promise<void> {
  await page.goto('/login');
  await page.getByLabel('Email').fill(address);
  await page.getByLabel('Password').fill(PASS);
  await page.getByRole('button', { name: 'Sign in' }).click();
  await page.waitForURL(/\/dashboard$/);
}

test('an organization logo replaces the wordmark and shows in the room', async ({ page }) => {
  await signIn(page, USER);
  // Enter the admin area from the header, which already has the session
  // description loaded. A direct load of the route races the restore.
  await page.getByTestId('account-menu').click();
  await page.getByRole('link', { name: 'Administration' }).click();
  await page.waitForURL(/\/admin\/domains/);
  await page.getByRole('link', { name: 'Organizations' }).click();
  await page.waitForURL(/\/admin\/workgroups/);
  await expect(page.getByRole('heading', { name: 'Organizations' })).toBeVisible({ timeout: 20000 });

  await page.getByTestId('admin-logo-upload').first().setInputFiles({
    name: 'logo.png',
    mimeType: 'image/png',
    buffer: PNG,
  });
  await expect(page.getByTestId('admin-org-logo').first()).toBeVisible({ timeout: 15000 });

  // A full reload re-reads /me, which is where the header learns the logo.
  await page.goto('/dashboard');
  await expect(page.getByTestId('org-logo')).toBeVisible({ timeout: 15000 });
  await expect(page.getByTestId('org-logo')).toHaveAttribute('src', /\/branding\/logo/);
  await expect(page.getByText('Example - linchange')).toHaveCount(0);

  // The conference room carries the organizer's organization logo top-left,
  // and an invitation guest sees the same one.
  await page.goto('/meetings');
  await page.getByRole('button', { name: 'New meeting' }).click();
  await page.getByLabel('Title').fill(`Branding ${RUN}`);
  await page.locator('textarea[name="attendees"]').fill(GUEST);
  await page.getByRole('button', { name: 'Create and invite' }).click();
  await page.waitForURL(/\/meetings\/[A-Za-z0-9_@-]+$/);
  await page.goto((await page.getByRole('link', { name: 'Join meeting' }).getAttribute('href')) ?? '');
  await page.waitForURL(/\/meetings\/[^/]+\/room/);
  await expect(page.getByTestId('room-org-logo')).toBeVisible({ timeout: 15000 });

  // Removing the logo restores the wordmark.
  await page.goto('/dashboard');
  await page.getByTestId('account-menu').click();
  await page.getByRole('link', { name: 'Administration' }).click();
  await page.waitForURL(/\/admin\/domains/);
  await page.getByRole('link', { name: 'Organizations' }).click();
  await page.waitForURL(/\/admin\/workgroups/);
  await page.getByTestId('admin-logo-remove').first().click();
  await expect(page.getByTestId('admin-org-logo')).toHaveCount(0);
  await page.goto('/dashboard');
  await expect(page.getByText('Example - linchange')).toBeVisible({ timeout: 15000 });
});
