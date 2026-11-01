import { Page, expect, test } from '@playwright/test';

// Angular's i18n builds one app per locale. The header switcher moves to the
// same route in the other build and records the choice in a cookie.
const USER = process.env.E2E_USER ?? 'e2e-user@example.com';
const PASS = process.env.E2E_PASSWORD ?? 'correct horse battery staple';

// The label of the language selector is itself translated, which is a cheap way
// to prove the build really is the one asked for.
const LOCALES = [
  { code: 'en', label: 'Language', mail: 'Mail' },
  { code: 'it', label: 'Lingua', mail: 'Posta' },
  { code: 'de', label: 'Sprache', mail: 'E-Mail' },
  { code: 'pl', label: 'Język', mail: 'E-mail' },
];

const LANGUAGE_SELECTOR = /Language|Lingua|Sprache|Język/;

async function signIn(page: Page): Promise<void> {
  await page.goto('/login');
  await page.getByLabel('Email').fill(USER);
  await page.getByLabel('Password').fill(PASS);
  await page.getByRole('button', { name: 'Sign in' }).click();
  await page.waitForURL(/\/dashboard$/);
}

test('the web UI switches between every built locale', async ({ page }) => {
  await signIn(page);
  await page.goto('/mail');
  const nav = page.getByRole('navigation');

  for (const locale of LOCALES) {
    await page.getByRole('combobox', { name: LANGUAGE_SELECTOR }).selectOption(locale.code);
    await expect(page.locator('html')).toHaveAttribute('lang', locale.code, { timeout: 15000 });
    await expect(nav.getByRole('link', { name: locale.mail, exact: true })).toBeVisible();
    await expect(page).toHaveTitle(`Linchange - ${locale.mail}`);
  }

  // The cookie makes the bare root serve the language last chosen, and going
  // back to English works from any locale.
  await page.goto('/');
  await expect(page.locator('html')).toHaveAttribute('lang', 'pl', { timeout: 15000 });

  await page.getByRole('combobox', { name: LANGUAGE_SELECTOR }).selectOption('en');
  await expect(page.locator('html')).toHaveAttribute('lang', 'en', { timeout: 15000 });
  await expect(nav.getByRole('link', { name: 'Mail', exact: true })).toBeVisible();
});

test('a visitor with no cookie is served the language their browser asks for', async ({ page }) => {
  // Asked through the request context rather than a browser navigation: Chromium
  // insists on its own Accept-Language, so a header set by the test is not what
  // the server would see. Negotiating the redirect directly is the point of the
  // check anyway - it is the server choosing the shell, not the app.
  for (const locale of LOCALES) {
    const response = await page.request.get('/login', {
      headers: { 'Accept-Language': `${locale.code}-DE,${locale.code};q=0.9,en;q=0.8` },
    });
    await expect(response.url()).toContain(`/${locale.code}/login`);
    const shell = await response.text();
    await expect(shell).toContain(`lang="${locale.code}"`);
  }
});
