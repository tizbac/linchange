import { Page, expect, test } from '@playwright/test';

// A full browser import needs a real Outlook archive, which is not committed
// (it is somebody's mailbox). Point E2E_PST_FIXTURE at one to run this test;
// without it the test is skipped rather than quietly passing.
const FIXTURE = process.env.E2E_PST_FIXTURE ?? '';
const USER = process.env.E2E_USER ?? 'e2e-user@example.com';
const PASS = process.env.E2E_PASSWORD ?? 'correct horse battery staple';

test.skip(FIXTURE === '', 'set E2E_PST_FIXTURE to a .pst file to run the PST import test');

async function signIn(page: Page): Promise<void> {
  await page.goto('/login');
  await page.getByLabel('Email').fill(USER);
  await page.getByLabel('Password').fill(PASS);
  await page.getByRole('button', { name: 'Sign in' }).click();
  await page.waitForURL(/\/dashboard$/);
}

test('imports a PST from the browser', async ({ page }) => {
  test.setTimeout(240_000);

  await signIn(page);

  // The existing import entry point now asks which kind of import this is.
  await page.goto('/mail/import');
  await expect(page.getByRole('heading', { name: 'Import mail' })).toBeVisible();
  const pstCard = page.locator('a[href$="/mail/import/pst"]');
  await expect(pstCard).toBeVisible();
  await pstCard.click();
  await expect(page).toHaveURL(/\/mail\/import\/pst$/);

  // Parsing happens in the page, so the folder summary is the readiness gate.
  await page.locator('app-pst-import-panel input[type=file]').setInputFiles(FIXTURE);
  await expect(page.getByText(/folder\(s\)/)).toBeVisible({ timeout: 90_000 });

  await page.getByRole('button', { name: 'Start import' }).click();
  await expect(page.getByText(/Do not close this page/)).toBeVisible();
  await expect(page.getByText(/Import finished:/)).toBeVisible({ timeout: 180_000 });
});
