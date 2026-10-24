import { Page, expect, test } from '@playwright/test';

const USER = process.env.E2E_USER ?? 'e2e-user@example.com';
const PASS = process.env.E2E_PASSWORD ?? 'correct horse battery staple';

async function signIn(page: Page): Promise<void> {
  await page.goto('/login');
  await page.getByLabel('Email').fill(USER);
  await page.getByLabel('Password').fill(PASS);
  await page.getByRole('button', { name: 'Sign in' }).click();
  await page.waitForURL(/\/dashboard$/);
}

test('saving the full mailbox edit form does not report a malformed request', async ({ page }) => {
  await signIn(page);
  await page.getByTestId('account-menu').click();
  await page.getByRole('link', { name: 'Administration' }).click();
  await page.waitForURL(/\/admin\/domains/);
  await page.getByRole('link', { name: 'Users' }).click();
  await page.waitForURL(/\/admin\/users$/);

  const row = page.locator('tbody tr').filter({ hasText: USER });
  await row.getByRole('button', { name: 'Actions' }).click();
  await page.getByRole('menuitem', { name: 'Edit' }).click();
  await page.waitForURL(/\/admin\/users\/.+/);

  await page.getByRole('button', { name: 'Save changes' }).click();
  await expect(page.getByText(`${USER} was saved.`)).toBeVisible({ timeout: 15000 });
  await expect(page.getByText('malformed request')).toHaveCount(0);
});
