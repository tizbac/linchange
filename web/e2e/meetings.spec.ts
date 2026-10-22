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

    // The call has its own full-page room now. Close the signed-in guest's
    // tab so the same address is not represented twice; the anonymous link is
    // the guest that enters the room.
    await guest.close();
    await page.getByRole('link', { name: 'Join meeting' }).click();
    await page.waitForURL(/\/meetings\/[^/]+\/room/);
    await expect(page.getByTestId('call-grid')).toBeVisible({ timeout: 15000 });

    await expect(anon.getByRole('link', { name: 'Join meeting' })).toBeVisible({ timeout: 15000 });
    await anon.getByRole('link', { name: 'Join meeting' }).click();
    await anon.waitForURL(/\/meetings\/[^/]+\/room/);
    await expect(anon.getByTestId('call-grid')).toBeVisible({ timeout: 15000 });

    // Chat is a collapsible sidebar in the room and shares the call socket.
    const composer = page.locator('input[name="draftText"]');
    await expect(composer).toBeVisible({ timeout: 20000 });
    await composer.fill(MARKER);
    await page.locator('form').filter({ has: composer }).getByRole('button', { name: 'Send' }).click();
    await expect(page.locator('#chatLog').getByText(MARKER)).toBeVisible();
    await expect(anon.locator('#chatLog').getByText(MARKER)).toBeVisible({ timeout: 25000 });

    const guestMarker = `guest marker ${RUN}`;
    const guestComposer = anon.locator('input[name="draftText"]');
    await expect(guestComposer).toBeVisible({ timeout: 20000 });
    await guestComposer.fill(guestMarker);
    await anon.locator('form').filter({ has: guestComposer }).getByRole('button', { name: 'Send' }).click();
    await expect(page.locator('#chatLog').getByText(guestMarker)).toBeVisible({ timeout: 15000 });

    // The room collapses the sidebar without dropping the call.
    await page.getByRole('button', { name: 'Collapse chat' }).click();
    await expect(page.locator('input[name="draftText"]')).toHaveCount(0);
    await page.getByRole('button', { name: 'Chat' }).click();
    await expect(page.locator('input[name="draftText"]')).toBeVisible();

    // Each side sees the other as a remote tile once the offer/answer and ICE
    // have completed. The tile is labelled with the other member's address.
    await expect(anon.getByTestId('call-tile').filter({ hasText: USER })).toBeVisible({ timeout: 30000 });
    await expect(page.getByTestId('call-tile').filter({ hasText: GUEST })).toBeVisible({ timeout: 30000 });
  });

  test('a one-way meeting keeps guests muted until the presenter grants the floor', async ({ page, browser }) => {
    const title = `Webinar ${RUN}`;

    await signIn(page, USER);
    await page.goto('/meetings');
    await page.getByRole('button', { name: 'New meeting' }).click();
    await page.getByLabel('Title').fill(title);
    await page.locator('textarea[name="attendees"]').fill(GUEST);
    await page.getByLabel('One-way conference (webinar)').check();
    await page.getByRole('button', { name: 'Create and invite' }).click();
    await page.waitForURL(/\/meetings\/[A-Za-z0-9_@-]+$/);
    await expect(page.getByText('You are the organiser.')).toBeVisible();

    // The guest joins with no microphone: a webinar is listen-first.
    const guest = await browser.newPage();
    await signIn(guest, GUEST);
    await openMeeting(guest, title);
    await guest.getByRole('link', { name: 'Join meeting' }).click();
    await guest.waitForURL(/\/meetings\/[^/]+\/room/);
    await expect(guest.getByTestId('call-grid')).toBeVisible({ timeout: 15000 });
    await expect(guest.getByTestId('webinar-note')).toContainText('One-way conference');
    await expect(guest.getByTestId('raise-hand')).toHaveText('Raise hand');
    await expect(guest.getByRole('button', { name: 'Unmute' })).toBeVisible();

    // Unmuting without the floor is refused, with a reason rather than silence.
    await guest.getByRole('button', { name: 'Unmute' }).click();
    await expect(guest.getByText('Raise your hand to ask the presenter for the floor.')).toBeVisible();

    // Raising a hand asks for the floor; the presenter sees the request.
    await guest.getByTestId('raise-hand').click();
    await expect(guest.getByTestId('raise-hand')).toHaveText('Lower hand');

    await page.getByRole('link', { name: 'Join meeting' }).click();
    await page.waitForURL(/\/meetings\/[^/]+\/room/);
    const hands = page.getByTestId('raised-hands');
    await expect(hands).toContainText(GUEST, { timeout: 15000 });
    await hands.getByRole('button', { name: 'Allow to speak' }).click();

    // The floor is granted: the guest's microphone opens and both sides see
    // who is speaking.
    await expect(guest.getByRole('button', { name: 'Mute' })).toBeVisible({ timeout: 15000 });
    await expect(guest.getByTestId('webinar-note')).toContainText(`${GUEST} has the floor.`);
    await expect(page.getByTestId('webinar-note')).toContainText(`${GUEST} has the floor.`);

    // The presenter can take the floor back.
    await hands.getByRole('button', { name: 'Mute' }).click();
    await expect(guest.getByRole('button', { name: 'Unmute' })).toBeVisible({ timeout: 15000 });
  });
});

test('a participant can join with camera and microphone denied', async ({ page }) => {
  const title = `No devices ${RUN}`;

  await signIn(page, USER);
  await page.goto('/meetings');
  await page.getByRole('button', { name: 'New meeting' }).click();
  await page.getByLabel('Title').fill(title);
  await page.locator('textarea[name="attendees"]').fill(GUEST);
  await page.getByRole('button', { name: 'Create and invite' }).click();
  await page.waitForURL(/\/meetings\/[A-Za-z0-9_@-]+$/);
  await expect(page.getByText('You are the organiser.')).toBeVisible();

  // Refuse both devices, as a browser with no camera/microphone or a person
  // who does not want to share them would. The room must still render a
  // gallery and the chat.
  await page.evaluate(() => {
    navigator.mediaDevices.getUserMedia = () => Promise.reject(new Error('permission denied'));
  });
  await page.getByRole('link', { name: 'Join meeting' }).click();
  await page.waitForURL(/\/meetings\/[^/]+\/room/);

  await expect(page.getByTestId('call-grid')).toBeVisible({ timeout: 15000 });
  await expect(page.getByText(/listen-only/)).toBeVisible();
  await expect(page.getByTestId('call-tile').filter({ hasText: 'You' })).toBeVisible();
  await expect(page.locator('input[name="draftText"]')).toBeVisible();
  await expect(page.getByText(/camera and microphone could not be opened/i)).toHaveCount(0);
});
