import { Page, expect, test } from '@playwright/test';

// One participant has no camera. The other still sends video, and the
// audio-only participant must keep receiving it after the first frames.
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

test('a participant without a camera keeps receiving the other video', async ({ page, browser }) => {
  const title = `No webcam ${RUN}`;
  // Force media through the built-in TURN server, the path a call between
  // two machines behind NATs takes. Host candidates are not enough there.
  const forceRelay = () => {
    const Original = window.RTCPeerConnection;
    // @ts-expect-error constructor replacement
    window.RTCPeerConnection = function (config?: RTCConfiguration) {
      return new Original({ ...(config ?? {}), iceTransportPolicy: 'relay' });
    };
  };
  await page.addInitScript(forceRelay);

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

  // The guest browser has a microphone but no camera. The app asks for both
  // first, then falls back to audio only.
  const guest = await browser.newPage();
  await guest.addInitScript(forceRelay);
  await guest.addInitScript(() => {
    const original = navigator.mediaDevices.getUserMedia.bind(navigator.mediaDevices);
    navigator.mediaDevices.getUserMedia = (constraints?: MediaStreamConstraints) => {
      if (constraints && constraints.video) {
        return Promise.reject(new DOMException('no camera', 'NotFoundError'));
      }
      return original(constraints);
    };
  });
  await signIn(guest, GUEST);
  await guest.goto(meetingUrl);
  await guest.getByRole('link', { name: 'Join meeting' }).click();
  await guest.waitForURL(/\/meetings\/[^/]+\/room/);

  await expect(guest.getByTestId('call-tile').filter({ hasText: USER })).toBeVisible({ timeout: 30000 });
  await expect(page.getByTestId('call-tile').filter({ hasText: GUEST })).toBeVisible({ timeout: 30000 });

  // The organiser sees an avatar for the camera-less guest.
  await expect(
    page.getByTestId('call-tile').filter({ hasText: GUEST }).getByText('Camera off'),
  ).toBeVisible();

  // The guest's remote video must keep playing, not stop after the first
  // frames. A frozen stream stops advancing currentTime.
  const remoteVideo = guest.getByTestId('call-tile').filter({ hasText: USER }).locator('video');
  await expect(remoteVideo).toBeVisible({ timeout: 30000 });
  const first = await remoteVideo.evaluate((v) => (v as HTMLVideoElement).currentTime);
  await guest.waitForTimeout(3000);
  const second = await remoteVideo.evaluate((v) => (v as HTMLVideoElement).currentTime);
  expect(second).toBeGreaterThan(first);

  await guest.close();
});

test('a screen share is a separate view-only feed and stops cleanly', async ({ page, browser }) => {
  const title = `Screen ${RUN}`;

  // Headless Chromium has no picker, so give getDisplayMedia a synthetic
  // screen: a canvas captured as a live video track.
  await page.addInitScript(() => {
    Object.defineProperty(navigator.mediaDevices, 'getDisplayMedia', {
      configurable: true,
      value: async () => {
        const canvas = document.createElement('canvas');
        canvas.width = 640;
        canvas.height = 360;
        canvas.style.position = 'fixed';
        canvas.style.left = '-10000px';
        document.body.appendChild(canvas);
        const ctx = canvas.getContext('2d');
        let x = 0;
        window.setInterval(() => {
          if (!ctx) return;
          ctx.fillStyle = '#0f172a';
          ctx.fillRect(0, 0, 640, 360);
          ctx.fillStyle = '#22c55e';
          ctx.beginPath();
          ctx.arc(x, 180, 40, 0, Math.PI * 2);
          ctx.fill();
          x = (x + 12) % 640;
        }, 50);
        return canvas.captureStream(15);
      },
    });
  });

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

  const guest = await browser.newPage();
  await signIn(guest, GUEST);
  await guest.goto(meetingUrl);
  await guest.getByRole('link', { name: 'Join meeting' }).click();
  await guest.waitForURL(/\/meetings\/[^/]+\/room/);

  await expect(guest.getByTestId('call-tile').filter({ hasText: USER })).toBeVisible({ timeout: 30000 });
  await expect(page.getByTestId('call-tile').filter({ hasText: GUEST })).toBeVisible({ timeout: 30000 });

  // The organiser shares; the camera tile stays and the screen is a second,
  // separate feed.
  await page.getByTestId('share-screen').click();
  const ownScreen = page.getByTestId('screen-share');
  await expect(ownScreen).toBeVisible({ timeout: 15000 });
  await expect(ownScreen).toContainText('Your screen');
  await expect(page.getByTestId('call-tile').filter({ hasText: GUEST })).toBeVisible();

  // The guest sees the same screen, labelled with who is sharing, muted and
  // view-only. It plays without advancing any controls.
  const guestScreen = guest.getByTestId('screen-share');
  await expect(guestScreen).toBeVisible({ timeout: 30000 });
  await expect(guestScreen).toContainText(USER);
  await expect(guestScreen).toContainText('view only');
  const screenVideo = guestScreen.locator('video');
  await expect(screenVideo).toBeVisible();
  const first = await screenVideo.evaluate((v) => (v as HTMLVideoElement).currentTime);
  await guest.waitForTimeout(2000);
  const second = await screenVideo.evaluate((v) => (v as HTMLVideoElement).currentTime);
  expect(second).toBeGreaterThan(first);

  // Stopping removes the screen for both sides but leaves the call and the
  // camera tile alone.
  await page.getByTestId('share-screen').click();
  await expect(guestScreen).toHaveCount(0, { timeout: 15000 });
  await expect(page.getByTestId('screen-share')).toHaveCount(0);
  await expect(guest.getByTestId('call-tile').filter({ hasText: USER })).toBeVisible();

  await guest.close();
});

test('a webinar guest cannot share a screen', async ({ page, browser }) => {
  const title = `Webinar screen ${RUN}`;

  await signIn(page, USER);
  await page.goto('/meetings');
  await page.getByRole('button', { name: 'New meeting' }).click();
  await page.getByLabel('Title').fill(title);
  await page.locator('textarea[name="attendees"]').fill(GUEST);
  await page.getByLabel('One-way conference (webinar)').check();
  await page.getByRole('button', { name: 'Create and invite' }).click();
  await page.waitForURL(/\/meetings\/[A-Za-z0-9_@-]+$/);
  const meetingUrl = page.url();

  const guest = await browser.newPage();
  await signIn(guest, GUEST);
  await guest.goto(meetingUrl);
  await guest.getByRole('link', { name: 'Join meeting' }).click();
  await guest.waitForURL(/\/meetings\/[^/]+\/room/);
  await expect(guest.getByTestId('call-grid')).toBeVisible({ timeout: 15000 });

  // Only the presenter has the share control in a one-way meeting.
  await expect(guest.getByTestId('raise-hand')).toBeVisible({ timeout: 15000 });
  await expect(guest.getByTestId('share-screen')).toHaveCount(0);

  await page.getByRole('link', { name: 'Join meeting' }).click();
  await page.waitForURL(/\/meetings\/[^/]+\/room/);
  await expect(page.getByTestId('share-screen')).toBeVisible();

  await guest.close();
});
