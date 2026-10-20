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

// The inbox button carries an unread count when mail has arrived, so the
// folder buttons are matched as prefixes rather than exact names.
function folderButton(page: Page, name: string) {
  return page.locator('aside').getByRole('button', { name });
}

async function openSent(page: Page): Promise<void> {
  await folderButton(page, 'Sent').click();
}

async function sendCompose(page: Page, to: string, subject: string, body: string): Promise<void> {
  await page.goto('/mail/compose');
  await page.locator('#mc-to').fill(to);
  await page.locator('#mc-subject').fill(subject);
  await fillPlainTextBody(page, body);
  await page.getByRole('button', { name: 'Send' }).click();
  await page.waitForURL(/\/mail$/);
}

// The composer opens in rich text; the plain-text mode has the simple textarea
// the tests fill, so switch to it rather than driving the editor's iframe.
async function fillPlainTextBody(page: Page, body: string): Promise<void> {
  await page.getByRole('button', { name: 'Plain text', exact: true }).click();
  await page.locator('textarea[name="body"]').fill(body);
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
    await fillPlainTextBody(page, body);
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
    await fillPlainTextBody(page, `Reply body (${RUN}).`);
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

  test('opens a shared mailbox and files a message with an editor grant', async ({ page }) => {
    const subject = `E2E ${RUN} to the team`;

    await signIn(page);
    // Mail addressed to the shared mailbox lands in that mailbox's own inbox.
    await sendCompose(page, 'team@example.com', subject, `Team copy (${RUN}).`);

    await page.goto('/mail');
    await expect(page.getByText('Shared with you')).toBeVisible();
    // Delivery is asynchronous, and the list does not live-refresh, so reopen
    // the shared inbox until the message has landed in it.
    const message = page.getByText(subject).first();
    for (let i = 0; i < 20 && !(await message.isVisible()); i++) {
      await page.getByText('Team Mailbox').first().click();
      await page.waitForTimeout(1000);
    }
    await expect(message).toBeVisible();
    await message.click();
    await expect(page.getByRole('heading', { name: subject })).toBeVisible();
    await expect(page.locator('pre')).toContainText(`Team copy (${RUN}).`);

    // The seeded member holds an editor grant — the level that may move or
    // delete somebody else's mail — so the interface administers the team
    // mailbox instead of refusing on its behalf. The message leaves the shared
    // inbox for that mailbox's own Trash, never the member's: their own Trash
    // is still empty, and the count beside the shared folder is what tells the
    // two apart.
    await page.locator('app-mail-reader').getByRole('button', { name: 'Trash' }).click();
    await expect(page.getByText(subject)).toHaveCount(0);
    await expect(page.getByRole('button', { name: 'Trash 1' })).toBeVisible();
  });
});
