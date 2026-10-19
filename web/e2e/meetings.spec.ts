import { Page, expect, test } from '@playwright/test';

// Same shared stack as the webmail suite. The meeting's guests are the seeded
// local account (which has a chat identity next to its invitation) and an
// address with no account anywhere (whose link is the whole interface).
const USER = process.env.E2E_USER ?? 'e2e-user@example.com';
const PASS = process.env.E2E_PASSWORD ?? 'correct horse battery staple';
const RUN = Date.now().toString(36);
const GUEST = 'e2e-other@example.com';
const OUTSIDER = `greta-${RUN}@outside.example`;
const TITLE = `Playwright sync ${RUN}`;
const MARKER = `room marker ${RUN}`;

async function signIn(page: Page, address: string): Promise<void> {
  await page.goto('/login');
  await page.getByLabel('Email').fill(address);
  await page.getByLabel('Password').fill(PASS);
  await page.getByRole('button', { name: 'Sign in' }).click();
  await page.waitForURL(/\/dashboard$/);
}

async function openMeeting(page: Page, title: string): Promise<void> {
  await page.goto('/meetings');
  await expect(page.getByText(title).first()).toBeVisible();
  await page.getByText(title).first().click();
  await expect(page.getByRole('heading', { name: title })).toBeVisible();
}

test.describe('meetings', () => {
  test('an organiser invites, guests answer, and the room chats', async ({ page, browser }) => {
    // Create: the form leads straight to the meeting's own page.
    await signIn(page, USER);
    await page.goto('/meetings');
    await page.getByRole('button', { name: 'New meeting' }).click();
    await page.getByLabel('Title').fill(TITLE);
    await page
      .locator('textarea[name="attendees"]')
      .fill(`${GUEST}\nGreta Outsider <${OUTSIDER}>`);
    await page.getByRole('button', { name: 'Create and invite' }).click();
    await page.waitForURL(/\/meetings\/[A-Za-z0-9_@-]+$/);
    await expect(page.getByRole('heading', { name: TITLE })).toBeVisible();
    await expect(page.getByText('You are the organiser.')).toBeVisible();
    await expect(page.getByText(/2 guests, 0 accepted/)).toBeVisible();
    await expect(page.getByText('No answer', { exact: true })).toHaveCount(2);

    // The invitation mail itself: the local guest's copy lands in their
    // mailbox, and the body carries the per-guest link. Delivery is
    // asynchronous, so the inbox is reopened until it shows up.
    const guest = await browser.newPage();
    await signIn(guest, GUEST);
    await guest.goto('/mail');
    const invite = guest.getByText(`Invitation: ${TITLE}`).first();
    for (let i = 0; i < 20 && !(await invite.isVisible()); i++) {
      await guest.reload();
      await guest.waitForTimeout(1000);
    }
    await expect(invite).toBeVisible();
    await invite.click();
    const link = /Join or answer: (\S+)/.exec(await guest.locator('pre').first().innerText())?.[1];
    if (!link) throw new Error('the invitation body carries no "Join or answer" link');

    // Answering while signed in: the guest is on their own guest list.
    await openMeeting(guest, TITLE);
    await guest.getByRole('button', { name: 'Maybe' }).click();
    await expect(guest.getByText('Answered: Maybe')).toBeVisible();

    // Answering through the link with no account at all: the token in the
    // link is the identity, and the same link can change the same answer.
    const anon = await browser.newPage();
    await anon.goto(link);
    await expect(anon.getByText(`You are answering as ${GUEST}`)).toBeVisible();
    await anon.getByRole('button', { name: 'Accepted' }).click();
    await expect(anon.getByText('Answered: Accepted')).toBeVisible();

    // The organiser's tally reflects both paths.
    await page.reload();
    await expect(page.getByText(/2 guests, 1 accepted/)).toBeVisible();

    // Chat, through the real OpenIM SDK: the organiser opens the room and
    // says something; the guest's own page, opening the same room from a
    // different browser context, reads it back from server history.
    await page.getByRole('button', { name: 'Open chat' }).click();
    const composer = page.locator('input[name="draftText"]');
    await expect(composer).toBeVisible({ timeout: 20000 });
    await composer.fill(MARKER);
    await page.locator('form').filter({ has: composer }).getByRole('button', { name: 'Send' }).click();
    await expect(page.locator('#chatLog').getByText(MARKER)).toBeVisible();

    await openMeeting(guest, TITLE);
    await guest.getByRole('button', { name: 'Open chat' }).click();
    await expect(guest.locator('#chatLog').getByText(MARKER)).toBeVisible({ timeout: 25000 });
  });
});
