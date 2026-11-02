import { expect, test } from '@playwright/test';

const USER = process.env.E2E_USER ?? 'e2e-user@example.com';
const PASS = process.env.E2E_PASSWORD ?? 'correct horse battery staple';
const RUN = Date.now().toString(36);

// A CalDAV client writes an appointment, then the browser opens the calendar
// page and has to draw it. This is deliberately end to end through the CalDAV
// resource and the rendered FullCalendar view, not just the REST list: the
// latter was already right while the page could still miss the event.
test('an event written over CalDAV is drawn on the calendar page', async ({ page, request }) => {
  const login = await request.post('/api/v1/auth/login', {
    data: { login: USER, password: PASS },
  });
  expect(login.ok()).toBeTruthy();
  const access = (await login.json()).access_token as string;
  const auth = { Authorization: `Bearer ${access}` };

  const started = await request.post('/api/v1/auth/app-token', {
    headers: auth,
    data: { description: `e2e calendar ${RUN}` },
  });
  expect(started.ok()).toBeTruthy();
  const grant = await started.json();
  const approved = await request.post(
    `/api/v1/profile/application-tokens/request/${grant.user_code}`,
    { headers: auth, data: { approve: true } },
  );
  expect(approved.ok()).toBeTruthy();
  const finished = await request.post('/api/v1/auth/app-token/token', {
    data: { device_code: grant.device_code },
  });
  expect(finished.ok()).toBeTruthy();
  const appPassword = (await finished.json()).token as string;

  const calendars = await request.get('/api/v1/calendars', { headers: auth });
  expect(calendars.ok()).toBeTruthy();
  const list = (await calendars.json()).calendars as Array<{
    id: number;
    name: string;
    is_default?: boolean;
  }>;
  const calendar = list.find((cal) => cal.is_default) ?? list[0];
  expect(calendar).toBeTruthy();

  const title = `CalDAV browser ${RUN}`;
  const uid = `caldav-browser-${RUN}`;
  const start = '2026-11-05T10:00:00Z';
  const end = '2026-11-05T11:00:00Z';
  const card = [
    'BEGIN:VCALENDAR',
    'VERSION:2.0',
    'PRODID:-//e2e//EN',
    'BEGIN:VEVENT',
    `UID:${uid}`,
    'DTSTAMP:20261102T200000Z',
    'DTSTART:20261105T100000Z',
    'DTEND:20261105T110000Z',
    'TRANSP:TRANSPARENT',
    `SUMMARY:${title}`,
    'END:VEVENT',
    'END:VCALENDAR',
    '',
  ].join('\r\n');
  const href = `/caldav/${encodeURIComponent(USER)}/${encodeURIComponent(calendar.name)}/${uid}.ics`;
  const written = await request.put(href, {
    headers: {
      Authorization: `Basic ${Buffer.from(`${USER}:${appPassword}`).toString('base64')}`,
      'Content-Type': 'text/calendar',
    },
    data: card,
  });
  expect(written.status(), await written.text()).toBe(201);

  await page.goto('/login');
  await page.getByLabel('Email').fill(USER);
  await page.getByLabel('Password').fill(PASS);
  await page.getByRole('button', { name: 'Sign in' }).click();
  await page.waitForURL(/\/dashboard$/);
  await page.goto('/calendar');
  // The user's report was after a refresh: prove the ordinary load path first.
  await expect(page.getByText(title, { exact: true })).toBeVisible({ timeout: 15_000 });

  // And the page looks again while it is open: a second event made after the
  // first load is drawn without a reload, when the window comes back into focus.
  const later = `${title} later`;
  const laterUID = `${uid}-later`;
  const second = card.replaceAll(uid, laterUID).replace(title, later);
  const secondWrite = await request.put(
    `/caldav/${encodeURIComponent(USER)}/${encodeURIComponent(calendar.name)}/${laterUID}.ics`,
    {
      headers: {
        Authorization: `Basic ${Buffer.from(`${USER}:${appPassword}`).toString('base64')}`,
        'Content-Type': 'text/calendar',
      },
      data: second,
    },
  );
  expect(secondWrite.status()).toBe(201);
  await page.evaluate(() => window.dispatchEvent(new Event('focus')));
  await expect(page.getByText(later, { exact: true })).toBeVisible({ timeout: 15_000 });
});
