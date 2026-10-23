import { Page, expect, test } from '@playwright/test';

const USER = process.env.E2E_USER ?? 'e2e-user@example.com';
const PASS = process.env.E2E_PASSWORD ?? 'correct horse battery staple';
const MEMBER = 'e2e-other@example.com';
const RUN = Date.now().toString(36);

async function signIn(page: Page): Promise<void> {
  await page.goto('/login');
  await page.getByLabel('Email').fill(USER);
  await page.getByLabel('Password').fill(PASS);
  await page.getByRole('button', { name: 'Sign in' }).click();
  await page.waitForURL(/\/dashboard$/);
}

test('an organization group opens its members, searches, adds and removes', async ({ page }) => {
  await signIn(page);
  await page.getByTestId('account-menu').click();
  await page.getByRole('link', { name: 'Administration' }).click();
  await page.waitForURL(/\/admin\/domains/);
  await page.getByRole('link', { name: 'Organizations' }).click();
  await page.waitForURL(/\/admin\/workgroups/);
  await page.getByRole('link', { name: 'Manage' }).first().click();
  await page.waitForURL(/\/admin\/workgroups\/\d+/);

  await page.getByRole('button', { name: 'Groups' }).click();
  const name = `E2E group ${RUN}`;
  await page.locator('input[name="newGroup"]').fill(name);
  await page.getByRole('button', { name: 'Create' }).click();

  const row = page.locator('tr').filter({ hasText: name });
  await expect(row).toBeVisible({ timeout: 15000 });
  await row.getByRole('button', { name: 'Members' }).click();

  await expect(page.getByRole('heading', { name: `Members of ${name}` })).toBeVisible();
  await page.locator('input[name="newGroupMember"]').fill(MEMBER);
  await page.getByRole('button', { name: 'Add', exact: true }).click();
  await expect(page.getByRole('cell', { name: MEMBER })).toBeVisible({ timeout: 15000 });

  await page.getByRole('searchbox').fill(MEMBER);
  await expect(page.getByRole('cell', { name: MEMBER })).toBeVisible();

  await page.getByRole('button', { name: 'Remove' }).click();
  await expect(page.getByRole('cell', { name: MEMBER })).toHaveCount(0, { timeout: 15000 });

  await page.getByRole('button', { name: 'Back to groups' }).click();
  page.once('dialog', (dialog) => dialog.accept());
  await page.locator('tr').filter({ hasText: name }).getByRole('button', { name: 'Delete' }).click();
  await expect(page.getByText(name)).toHaveCount(0, { timeout: 15000 });
});
