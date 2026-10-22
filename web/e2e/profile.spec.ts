import { Page, expect, test } from '@playwright/test';

const USER = process.env.E2E_USER ?? 'e2e-user@example.com';
const PASS = process.env.E2E_PASSWORD ?? 'correct horse battery staple';
const RUN = Date.now().toString(36);

async function signIn(page: Page): Promise<void> {
  await page.goto('/login');
  await page.getByLabel('Email').fill(USER);
  await page.getByLabel('Password').fill(PASS);
  await page.getByRole('button', { name: 'Sign in' }).click();
  await page.waitForURL(/\/dashboard$/);
}

test('the profile page saves a recovery email and starts a 2FA setup', async ({ page }) => {
  await signIn(page);
  await page.getByTestId('account-menu').click();
  await page.getByRole('link', { name: 'Profile' }).click();
  await page.waitForURL(/\/profile$/);
  await expect(page.getByRole('heading', { name: 'Your account' })).toBeVisible();

  const recovery = `recover-${RUN}@outside.example`;
  await page.locator('input[name="recoveryEmail"]').fill(recovery);
  await page.getByRole('button', { name: 'Save', exact: true }).click();
  await expect(page.getByText('Recovery email saved.')).toBeVisible({ timeout: 15000 });
  await expect(page.locator('input[name="recoveryEmail"]')).toHaveValue(recovery);

  // Starting the setup returns a QR code and one-time recovery codes. The
  // enrolment stays disabled until a code is confirmed, so this does not
  // change the account's login for the rest of the suite.
  await page.getByRole('button', { name: 'Set up two-factor authentication' }).click();
  const qr = page.locator('img[alt="QR code"]');
  await expect(qr).toBeVisible({ timeout: 15000 });
  await expect(qr).toHaveAttribute('src', /^data:image\/png;base64,/);
  await expect(page.getByText('Recovery codes')).toBeVisible();
  await page.getByRole('button', { name: 'Cancel' }).click();
  await expect(qr).toHaveCount(0);
});
