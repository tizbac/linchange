import { Page, expect, test } from '@playwright/test';

// One shared server + database for the whole run; every test signs in fresh and
// uses RUN-unique subjects so tests never collide with each other's messages.
const USER = process.env.E2E_USER ?? 'e2e-user@example.com';
const PASS = process.env.E2E_PASSWORD ?? 'correct horse battery staple';
const RUN = Date.now().toString(36);
const EXTERNAL = `external-${RUN}@example.invalid`;

async function signIn(page: Page): Promise<void> {
  await page.goto('/login');
  await page.getByLabel('Email').fill(USER);
  await page.getByLabel('Password').fill(PASS);
  await page.getByRole('button', { name: 'Sign in' }).click();
  await page.waitForURL(/\/dashboard$/);
}

function folderButton(page: Page, name: string) {
  return page.locator('aside').getByRole('button', { name, exact: true });
}

async function openSent(page: Page): Promise<void> {
  await folderButton(page, 'Sent').click();
}

async function sendCompose(page: Page, to: string, subject: string, body: string): Promise<void> {
  await page.goto('/mail/compose');
  await page.locator('#mc-to').fill(to);
  await page.locator('#mc-subject').fill(subject);
  await page.locator('textarea[name="body"]').fill(body);
  await page.getByRole('button', { name: 'Send' }).click();
  await page.waitForURL(/\/mail$/);
}

test.describe('webmail', () => {
  test('signs in and shows the standard folders', async ({ page }) => {
    await signIn(page);
    await page.goto('/mail');
    for (const name of ['INBOX', 'Sent', 'Drafts', 'Junk', 'Trash', 'Archive']) {
      await expect(folderButton(page, name)).toBeVisible();
    }
    await expect(page.getByText('Select a message to read it.')).toBeVisible();
  });

  test('sends a message with an attachment and files it in Sent', async ({ page }) => {
    const subject = `E2E ${RUN} with attachment`;
    const body = `Playwright says hello (${RUN}).`;

    await signIn(page);
    await page.goto('/mail/compose');
    await page.locator('#mc-to').fill(EXTERNAL);
    await page.locator('#mc-subject').fill(subject);
    await page.locator('textarea[name="body"]').fill(body);
    await page
      .locator('input[type="file"]')
      .setInputFiles([{ name: 'note.txt', mimeType: 'text/plain', buffer: Buffer.from('hello from playwright') }]);
    await expect(page.getByText('note.txt')).toBeVisible();

    await page.getByRole('button', { name: 'Send' }).click();
    await page.waitForURL(/\/mail$/);
    await openSent(page);
    await page.getByText(subject).first().click();

    await expect(page.getByRole('heading', { name: subject })).toBeVisible();
    await expect(page.locator('pre')).toContainText(body);
    await expect(page.getByText('note.txt')).toBeVisible();
  });

  test('replies to a sent message', async ({ page }) => {
    const subject = `E2E ${RUN} reply target`;
    const body = `Original message (${RUN}).`;

    await signIn(page);
    await sendCompose(page, EXTERNAL, subject, body);
    await openSent(page);
    await page.getByText(subject).first().click();
    await expect(page.getByRole('heading', { name: subject })).toBeVisible();

    await page.getByRole('button', { name: 'Reply', exact: true }).click();
    await page.waitForURL(/\/mail\/compose$/);
    await expect(page.locator('#mc-subject')).toHaveValue(new RegExp(`^Re: `));
    await page.locator('textarea[name="body"]').fill(`Reply body (${RUN}).`);
    await page.getByRole('button', { name: 'Send' }).click();
    await page.waitForURL(/\/mail$/);

    await openSent(page);
    await expect(page.getByText(`Re: ${subject}`).first()).toBeVisible();
  });

  test('searches a folder', async ({ page }) => {
    const subject = `E2E ${RUN} searchable`;
    await signIn(page);
    await sendCompose(page, EXTERNAL, subject, `Something to find (${RUN}).`);
    await openSent(page);
    await page.getByPlaceholder('Search mail').fill(subject);
    await page.getByTitle('Search').click();

    await expect(page.getByText(`Results for “${subject}”`)).toBeVisible();
    await expect(page.getByText(subject).first()).toBeVisible();
  });

  test('creates and deletes a folder', async ({ page }) => {
    const name = `E2E Folder ${RUN}`;
    await signIn(page);
    await page.goto('/mail');

    page.once('dialog', (d) => void d.accept(name));
    await page.getByRole('button', { name: 'New folder' }).click();
    await expect(folderButton(page, name)).toBeVisible();

    await folderButton(page, name).click();
    page.once('dialog', (d) => void d.accept());
    await page.getByRole('button', { name: 'Delete', exact: true }).click();
    await expect(folderButton(page, name)).toHaveCount(0);
  });

  test('creates, disables and deletes a rule', async ({ page }) => {
    const name = `E2E rule ${RUN}`;
    await signIn(page);
    await page.goto('/mail/rules');
    await expect(page.getByRole('heading', { name: 'Rules' })).toBeVisible();

    await page.getByRole('button', { name: 'New rule' }).click();
    await page.getByPlaceholder('e.g. Invoices to the archive').fill(name);

    const selects = page.locator('form select.rrule');
    await selects.nth(0).selectOption('subject');
    await page.getByPlaceholder('Value').fill(`e2e-${RUN}`);
    await selects.nth(2).selectOption('move');
    await selects.nth(3).selectOption('Archive');

    await page.getByRole('button', { name: 'Save rule' }).click();
    await expect(page.getByRole('heading', { name: 'New rule' })).toHaveCount(0);
    await expect(page.getByText(name)).toBeVisible();

    const enabled = page.getByTitle('Enabled');
    await enabled.click();
    await expect(enabled).not.toBeChecked();

    page.once('dialog', (d) => void d.accept());
    await page.getByRole('button', { name: 'Delete', exact: true }).click();
    await expect(page.getByText(name)).toHaveCount(0);
  });
});
