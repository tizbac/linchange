import { Page, expect, test } from '@playwright/test';

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

test('clicking Join while the room auto-joins does not freeze the call', async ({ page, browser }) => {
  const title = `Race ${RUN}`;
  await signIn(page, USER);
  await page.goto('/meetings');
  await page.getByRole('button', { name: 'New meeting' }).click();
  await page.getByLabel('Title').fill(title);
  await page.locator('textarea[name="attendees"]').fill(GUEST);
  await page.getByRole('button', { name: 'Create and invite' }).click();
  await page.waitForURL(/\/meetings\/[A-Za-z0-9_@-]+$/);
  const meetingUrl = page.url();

  await page.getByRole('link', { name: 'Join meeting' }).click();
  await page.waitForURL(/\/meetings\/[^/]+\/room/);
  await expect(page.getByTestId('call-grid')).toBeVisible({ timeout: 15000 });

  // A slow permission prompt keeps the auto-join in flight and the Join
  // button on screen, which is what a person would click.
  const guest = await browser.newPage();
  await guest.addInitScript(() => {
    const original = navigator.mediaDevices.getUserMedia.bind(navigator.mediaDevices);
    navigator.mediaDevices.getUserMedia = async (constraints?: MediaStreamConstraints) => {
      await new Promise((r) => setTimeout(r, 3000));
      return original(constraints);
    };
  });
  await signIn(guest, GUEST);
  await guest.goto(meetingUrl);
  await guest.getByRole('link', { name: 'Join meeting' }).click();
  await guest.waitForURL(/\/meetings\/[^/]+\/room/);

  // The room auto-joins. While that is in flight the button is disabled, so
  // a person cannot start a second negotiation by clicking it.
  const join = guest.getByRole('button', { name: /Join call|Joining/ });
  await expect(join).toBeVisible();
  await expect(join).toBeDisabled();

  // The call must come up with no negotiation error and keep showing the
  // organiser's tile.
  await expect(guest.getByTestId('call-tile').filter({ hasText: USER })).toBeVisible({ timeout: 30000 });
  await expect(guest.getByText(/A call connection could not be established/)).toHaveCount(0);
  await expect(page.getByTestId('call-tile').filter({ hasText: GUEST })).toBeVisible({ timeout: 30000 });

  const remoteVideo = guest.getByTestId('call-tile').filter({ hasText: USER }).locator('video');
  await expect(remoteVideo).toBeVisible({ timeout: 30000 });
  const first = await remoteVideo.evaluate((v) => (v as HTMLVideoElement).currentTime);
  await guest.waitForTimeout(3000);
  const second = await remoteVideo.evaluate((v) => (v as HTMLVideoElement).currentTime);
  expect(second).toBeGreaterThan(first);

  await guest.close();
});
