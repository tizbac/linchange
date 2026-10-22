import { Page, expect, test } from '@playwright/test';

// Angular's i18n builds one app per locale. The header switcher moves to the
// same route in the other build and records the choice in a cookie.
const USER = process.env.E2E_USER ?? 'e2e-user@example.com';
const PASS = process.env.E2E_PASSWORD ?? 'correct horse battery staple';

async function signIn(page: Page): Promise<void> {
  await page.goto('/login');
  await page.getByLabel('Email').fill(USER);
  await page.getByLabel('Password').fill(PASS);
  await page.getByRole('button', { name: 'Sign in' }).click();
  await page.waitForURL(/\/dashboard$/);
}

test('the web UI switches between English and Italian', async ({ page }) => {
  await signIn(page);
  const nav = page.getByRole('navigation');
  await expect(nav.getByRole('link', { name: 'Mail', exact: true })).toBeVisible();

  await page.getByRole('combobox', { name: 'Language' }).selectOption('it');
  await expect(page.locator('html')).toHaveAttribute('lang', 'it', { timeout: 15000 });
  await expect(nav.getByRole('link', { name: 'Posta', exact: true })).toBeVisible();
  await expect(page.getByRole('heading', { name: 'Posta' })).toBeVisible();

  // The cookie makes the bare root choose Italian on the next visit.
  await page.goto('/');
  await expect(page.locator('html')).toHaveAttribute('lang', 'it', { timeout: 15000 });
  await expect(nav.getByRole('link', { name: 'Riunioni', exact: true })).toBeVisible();

  await page.getByRole('combobox', { name: 'Lingua' }).selectOption('en');
  await expect(page.locator('html')).toHaveAttribute('lang', 'en', { timeout: 15000 });
  await expect(nav.getByRole('link', { name: 'Mail', exact: true })).toBeVisible();
});
