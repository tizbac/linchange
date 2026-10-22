import { Page, expect, test } from '@playwright/test';
import * as net from 'net';

// One shared stack, as the other suites use. The contacts here are RUN-unique
// so a rerun does not collide with the one before it.
const USER = process.env.E2E_USER ?? 'e2e-user@example.com';
const PASS = process.env.E2E_PASSWORD ?? 'correct horse battery staple';
const RUN = Date.now().toString(36);

const SMTP_PORT = Number(process.env.E2E_SMTP_PORT ?? 1025);
const HOST = '127.0.0.1';

async function signIn(page: Page): Promise<void> {
  await page.goto('/login');
  await page.getByLabel('Email').fill(USER);
  await page.getByLabel('Password').fill(PASS);
  await page.getByRole('button', { name: 'Sign in' }).click();
  await page.waitForURL(/\/dashboard$/);
}

// injectMail delivers one message over the inbound SMTP port, the path real
// mail takes. The From name is what the contacts hook should learn.
function injectMail(fromName: string, fromAddress: string, to: string, subject: string): Promise<void> {
  const message = [
    `From: ${fromName} <${fromAddress}>`,
    `To: ${to}`,
    `Subject: ${subject}`,
    `Date: ${new Date().toUTCString()}`,
    'Content-Type: text/plain; charset="utf-8"',
    '',
    `Body of ${subject}.`,
    '',
  ].join('\r\n');

  const steps = [
    () => 'EHLO e2e\r\n',
    () => `MAIL FROM:<${fromAddress}>\r\n`,
    () => `RCPT TO:<${to}>\r\n`,
    () => 'DATA\r\n',
    () => `${message}.\r\n`,
    () => 'QUIT\r\n',
  ];

  return new Promise((resolve, reject) => {
    const socket = net.connect(SMTP_PORT, HOST);
    let buffer = '';
    let step = 0;
    let done = false;

    const finish = (error?: Error) => {
      if (done) return;
      done = true;
      socket.destroy();
      if (error) reject(error);
      else resolve();
    };

    socket.on('connect', () => socket.setNoDelay(true));
    socket.on('data', (chunk: Buffer) => {
      buffer += chunk.toString('utf8');
      for (;;) {
        const end = buffer.indexOf('\r\n');
        if (end < 0) return;
        const line = buffer.slice(0, end);
        buffer = buffer.slice(end + 2);
        if (!/^\d{3} /.test(line)) continue;
        const code = Number(line.slice(0, 3));
        if (code >= 400) {
          finish(new Error(`SMTP rejected: ${line}`));
          return;
        }
        if (step >= steps.length) {
          finish();
          return;
        }
        socket.write(steps[step++]());
      }
    });
    socket.on('error', (error) => finish(error));
    socket.on('close', () => finish());
    socket.setTimeout(10_000, () => finish(new Error('SMTP injection timed out')));
  });
}

test.describe('contacts', () => {
  test('personal contacts, autocomplete sources, and the send/receive hooks', async ({ page }) => {
    const contactName = `Alice Contact ${RUN}`;
    const contactEmail = `alice-contact-${RUN}@example.com`;
    const unknownAddress = `new-contact-${RUN}@example.com`;

    await signIn(page);

    // The personal book exists by default and is selected on arrival.
    await page.goto('/contacts');
    await expect(page.getByRole('heading', { name: 'Contacts', exact: true })).toBeVisible();
    await expect(page.getByRole('button', { name: /My contacts/ })).toBeVisible();

    // Create a card with the usual fields.
    await page.getByRole('button', { name: 'New contact' }).click();
    await page.locator('input[name="displayName"]').fill(contactName);
    await page.getByTestId('email-value-0').fill(contactEmail);
    await page.getByTestId('phone-value-0').fill('+39 02 1234');
    await page.locator('input[name="organization"]').fill('Example Ltd');
    await page.locator('input[name="title"]').fill('Engineer');
    await page.getByRole('button', { name: 'Save' }).click();
    await expect(page.getByText(contactName).first()).toBeVisible();

    // The composer's address book offers it, and says which book it came from.
    await page.goto('/mail/compose');
    await page.getByRole('button', { name: 'Address book' }).click();
    await expect(page.getByText(contactName).first()).toBeVisible();
    await expect(page.getByText('My contacts').first()).toBeVisible();

    // Creating a meeting autocompletes from the same books, with the source on
    // every candidate.
    await page.goto('/meetings');
    await page.getByRole('button', { name: 'New meeting' }).click();
    await page.getByLabel('Title').fill(`Contacts meeting ${RUN}`);
    await page.locator('input[name="contactQuery"]').fill('Alice Contact');
    const suggestion = page.getByRole('button', { name: new RegExp(contactName) });
    await expect(suggestion).toBeVisible({ timeout: 15000 });
    await expect(suggestion).toContainText('My contacts');
    await suggestion.click();
    await expect(page.locator('textarea[name="attendees"]')).toHaveValue(new RegExp(contactEmail));

    // Sending to somebody adds them to the personal book. The name is not
    // known yet, so the card is listed by its address.
    await page.goto('/mail/compose');
    await page.locator('#mc-to').fill(unknownAddress);
    await page.locator('#mc-subject').fill(`Contacts send ${RUN}`);
    await page.getByRole('button', { name: 'Plain text', exact: true }).click();
    await page.locator('textarea[name="body"]').fill('Hello from the contacts test.');
    await page.getByRole('button', { name: 'Send' }).click();
    await page.waitForURL(/\/mail$/);

    await page.goto('/contacts');
    await expect(page.getByText(unknownAddress).first()).toBeVisible({ timeout: 15000 });

    // A non-junk message from that address fills in the name. Reloading is
    // part of the retry: the list is loaded once per page, and the inbound
    // hook runs on the server after the delivery.
    await injectMail('New Person', unknownAddress, USER, `Contacts inbound ${RUN}`);
    await expect(async () => {
      await page.goto('/contacts');
      await expect(page.getByText('New Person', { exact: true }).first()).toBeVisible({ timeout: 2000 });
    }).toPass({ timeout: 45000 });
  });
});
