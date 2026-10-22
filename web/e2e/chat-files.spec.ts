import { Page, expect, test } from '@playwright/test';
import { readFile } from 'node:fs/promises';

// A file sent into a meeting's chat is stored once in the blob store and
// downloaded by the other side through the room's membership check.
const USER = process.env.E2E_USER ?? 'e2e-user@example.com';
const PASS = process.env.E2E_PASSWORD ?? 'correct horse battery staple';
const GUEST = 'e2e-other@example.com';
const RUN = Date.now().toString(36);

async function signIn(page: Page, address: string): Promise<void> {
  await page.goto('/login');
  await page.getByLabel('Email').fill(address);
  await page.getByLabel('Password').fill(PASS);
  await page.getByRole('button', { name: 'Sign in' }).click();
  await page.waitForURL(/\/dashboard$/);
}

test('a file sent in the meeting chat is downloadable by the other side', async ({ page, browser }) => {
  const title = `Chat files ${RUN}`;
  const body = `hello from ${RUN}`;

  await signIn(page, USER);
  await page.goto('/meetings');
  await page.getByRole('button', { name: 'New meeting' }).click();
  await page.getByLabel('Title').fill(title);
  await page.locator('textarea[name="attendees"]').fill(GUEST);
  await page.getByRole('button', { name: 'Create and invite' }).click();
  await page.waitForURL(/\/meetings\/[A-Za-z0-9_@-]+$/);
  const meetingUrl = page.url();

  await page.goto((await page.getByRole('link', { name: 'Join meeting' }).getAttribute('href')) ?? '');
  await page.waitForURL(/\/meetings\/[^/]+\/room/);
  await expect(page.locator('input[name="draftText"]')).toBeVisible({ timeout: 20000 });

  const guest = await browser.newPage();
  await signIn(guest, GUEST);
  await guest.goto(meetingUrl);
  await guest.goto((await guest.getByRole('link', { name: 'Join meeting' }).getAttribute('href')) ?? '');
  await guest.waitForURL(/\/meetings\/[^/]+\/room/);
  await expect(guest.locator('input[name="draftText"]')).toBeVisible({ timeout: 20000 });

  // The composer's hidden file input is what the paperclip opens.
  await page.setInputFiles('input[type="file"]', {
    name: 'notes.txt',
    mimeType: 'text/plain',
    buffer: Buffer.from(body),
  });

  // Both sides see the attachment chip; the sender gets it immediately and
  // the guest after the socket fans the stored message out.
  await expect(page.getByTestId('chat-attachment').first()).toContainText('notes.txt');
  await expect(guest.getByTestId('chat-attachment').first()).toContainText('notes.txt', {
    timeout: 20000,
  });

  const [download] = await Promise.all([
    guest.waitForEvent('download'),
    guest.getByRole('button', { name: 'Download' }).first().click(),
  ]);
  const path = await download.path();
  if (!path) throw new Error('the download produced no file');
  expect(await readFile(path, 'utf8')).toBe(body);

  await guest.close();
});
