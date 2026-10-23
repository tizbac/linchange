import { Page, expect, test } from '@playwright/test';

// A theme is a user preference (with organization and deployment fallbacks)
// and a custom theme is an administrator-uploaded ZIP.
const USER = process.env.E2E_USER ?? 'e2e-user@example.com';
const PASS = process.env.E2E_PASSWORD ?? 'correct horse battery staple';
const RUN = Date.now().toString(36);

// A ZIP whose only member is theme.css. Generated with Python's zipfile.
const THEME_ZIP = Buffer.from(
  'UEsDBBQAAAAIACCtVl3nfVQeSQAAAEcAAAAJAAAAdGhlbWUuY3Nzi05JLEnULclIzU21VUouLS7Jz1WKVUjKT6lUqFZISkzOTi/KL81LsVIoSk/SMDTSUTA20VEwNdNUUMzMLcgvKknMK7FWqOUCAFBLAQIUAxQAAAAIACCtVl3nfVQeSQAAAEcAAAAJAAAAAAAAAAAAAACAAQAAAAB0aGVtZS5jc3NQSwUGAAAAAAEAAQA3AAAAcAAAAAAA',
  'base64',
);

async function signIn(page: Page): Promise<void> {
  // Deleting a theme asks for confirmation; Playwright dismisses dialogs by
  // default, so accept them for this test.
  page.on('dialog', (dialog) => void dialog.accept());
  await page.goto('/login');
  await page.getByLabel('Email').fill(USER);
  await page.getByLabel('Password').fill(PASS);
  await page.getByRole('button', { name: 'Sign in' }).click();
  await page.waitForURL(/\/dashboard$/);
}

test('the theme switches, persists, and accepts an admin-uploaded theme', async ({ page }) => {
  await signIn(page);
  const html = page.locator('html');
  // The default is system, resolved to one of the two concrete values.
  await expect(html).toHaveAttribute('data-theme', /light|dark/, { timeout: 15000 });

  await page.getByRole('combobox', { name: 'Theme' }).selectOption('dark');
  await expect(html).toHaveAttribute('data-theme', 'dark');
  // The header uses a translucent Tailwind opacity variant; make sure the
  // dark remap actually reaches it rather than leaving a white bar.
  const headerBg = await page.locator('header').first().evaluate((el) => getComputedStyle(el).backgroundColor);
  expect(headerBg).toContain('30, 41, 59');

  // Webmail's small action buttons have component-scoped styles, so the
  // global dark remaps do not reach them on their own. They must still switch
  // to a light text colour rather than staying slate-700 on a dark toolbar.
  await page.goto('/mail');
  const refresh = page.getByTitle('Refresh');
  await expect(refresh).toBeVisible({ timeout: 15000 });
  const refreshColor = await refresh.evaluate((el) => getComputedStyle(el).color);
  expect(refreshColor).toBe('rgb(203, 213, 225)');

  // The choice is stored on the account, not only in this tab.
  await page.reload();
  await expect(html).toHaveAttribute('data-theme', 'dark', { timeout: 15000 });

  // Upload a custom theme as the domain administrator.
  await page.getByTestId('account-menu').click();
  await page.getByRole('link', { name: 'Administration' }).click();
  await page.waitForURL(/\/admin\/domains/);
  await page.getByRole('link', { name: 'Themes' }).click();
  await page.waitForURL(/\/admin\/themes/);
  const themeName = `Custom ${RUN}`;
  await page.locator('input[name="name"]').fill(themeName);
  await page.locator('input[type="file"]').setInputFiles({
    name: 'theme.zip',
    mimeType: 'application/zip',
    buffer: THEME_ZIP,
  });
  await page.getByRole('button', { name: 'Upload' }).click();
  await expect(page.getByRole('cell', { name: themeName })).toBeVisible({ timeout: 15000 });

  // A reload picks the new option up from /me, and selecting it links the CSS.
  await page.goto('/dashboard');
  await page.getByRole('combobox', { name: 'Theme' }).selectOption({ label: themeName });
  await expect(html).toHaveAttribute('data-theme', 'custom');
  await expect(page.locator('#lc-theme-css')).toHaveAttribute('href', /\/branding\/themes\//);
  const cssHref = await page.locator('#lc-theme-css').getAttribute('href');
  const css = await page.request.get(cssHref ?? '');
  expect(css.ok()).toBeTruthy();
  expect(await css.text()).toContain('rgb(12, 34, 56)');

  // Leave the account on the default and remove the uploaded theme.
  await page.getByRole('combobox', { name: 'Theme' }).selectOption('system');
  await page.goto('/dashboard');
  await page.getByTestId('account-menu').click();
  await page.getByRole('link', { name: 'Administration' }).click();
  await page.waitForURL(/\/admin\/domains/);
  await page.getByRole('link', { name: 'Themes' }).click();
  await page.waitForURL(/\/admin\/themes/);
  await page.getByRole('button', { name: 'Delete' }).first().click();
  await expect(page.getByRole('cell', { name: themeName })).toHaveCount(0, { timeout: 15000 });
});
