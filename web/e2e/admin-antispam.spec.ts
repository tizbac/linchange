import { Page, expect, test } from '@playwright/test';

// The anti-spam settings live on the domain, and the point of the test is that
// naming a relay does not stop the server delivering its own mail: everything
// the web client sends to a mailbox on this machine arrives through the loopback
// inbound listener. The settings are restored at the end so the rest of the
// suite sees the domain as it was seeded.
const USER = process.env.E2E_USER ?? 'e2e-user@example.com';
const PASS = process.env.E2E_PASSWORD ?? 'correct horse battery staple';
const RUN = Date.now().toString(36);
const DOMAIN = 'example.com';

async function signIn(page: Page): Promise<void> {
  await page.goto('/login');
  await page.getByLabel('Email').fill(USER);
  await page.getByLabel('Password').fill(PASS);
  await page.getByRole('button', { name: 'Sign in' }).click();
  await page.waitForURL(/\/dashboard$/);
}

async function openDomainEdit(page: Page, domain: string): Promise<void> {
  await page.goto('/admin/domains');
  const row = page.locator('tbody tr').filter({ hasText: domain });
  await row.getByRole('button', { name: 'Edit' }).click();
  await expect(page.locator('input[name="editNetworks"]')).toBeVisible();
}

async function saveDomain(page: Page): Promise<void> {
  await page.getByRole('button', { name: 'Save changes' }).click();
  await expect(page.getByText(`${DOMAIN} was updated.`)).toBeVisible({ timeout: 15000 });
}

// Sending a message is not delivery: the composer queues it, the delivery
// worker signs and relays it, and the inbound listener files it. The mailbox
// list is fetched as the page loads, so it can be a moment behind the message.
// Refreshing until the subject appears is what a user does when mail is slow.
async function waitForMessage(page: Page, subject: string): Promise<void> {
  for (let attempt = 0; attempt < 10; attempt++) {
    await page.getByRole('button', { name: 'Refresh' }).click();
    if (await page.getByText(subject).count() > 0) return;
    await page.waitForTimeout(1000);
  }
  throw new Error(`the message "${subject}" never arrived in the mailbox`);
}

test('an administrator names a domain anti-spam relays and a header rule', async ({ page }) => {
  await signIn(page);
  await openDomainEdit(page, DOMAIN);

  await page.locator('input[name="editNetworks"]').fill('127.0.0.1, 203.0.113.0/24');
  await page.getByRole('button', { name: '+ Add header rule' }).click();

  const rule = page.locator('[data-testid="header-rule-0"]');
  await expect(rule).toBeVisible();
  await rule.locator('input[placeholder="Header, e.g. X-Spam"]').fill('X-Spam');
  await rule.locator('input[placeholder="Value, e.g. YES"]').fill('YES');
  await rule.locator('select').nth(0).selectOption('contains');
  await rule.locator('select').nth(1).selectOption('junk');

  await saveDomain(page);

  // The list says what it restricted, so an operator can see a domain is not
  // open to the whole internet without opening the edit panel.
  const row = page.locator('tbody tr').filter({ hasText: DOMAIN });
  await expect(row.getByText('only from 2 anti-spam sources')).toBeVisible();

  // Opening the panel again reads the rule back from the server rather than
  // from what the form held in memory.
  await row.getByRole('button', { name: 'Edit' }).click();
  await expect(page.locator('[data-testid="header-rule-0"]')).toBeVisible();
  await expect(
    page.locator('[data-testid="header-rule-0"] input[placeholder="Header, e.g. X-Spam"]'),
  ).toHaveValue('X-Spam');
});

test('mail still reaches a mailbox in a domain restricted to its relays', async ({ page }) => {
  const subject = `Anti-spam e2e ${RUN}`;

  await signIn(page);
  await page.goto('/mail/compose');
  await page.locator('#mc-to').fill(USER);
  await page.locator('#mc-subject').fill(subject);
  await page.getByRole('button', { name: 'Plain text', exact: true }).click();
  await page.locator('textarea[name="body"]').fill('Delivered while the domain names its relays.');
  await page.getByRole('button', { name: 'Send' }).click();
  await page.waitForURL(/\/mail$/);

  await page.locator('aside').getByRole('button', { name: 'INBOX' }).click();
  await waitForMessage(page, subject);

  await page.getByText(subject).first().click();
  await expect(page.getByRole('heading', { name: subject })).toBeVisible();
});

test('the header rule and the relay list can be removed again', async ({ page }) => {
  await signIn(page);
  await openDomainEdit(page, DOMAIN);

  // The rule the first test stored is still there, and it is gone when the row
  // is removed and the form saved.
  await expect(
    page.locator('[data-testid="header-rule-0"] input[placeholder="Header, e.g. X-Spam"]'),
  ).toHaveValue('X-Spam');
  await page.locator('[data-testid="header-rule-0"]')
    .getByRole('button', { name: 'Remove' })
    .click();
  await page.locator('input[name="editNetworks"]').fill('');

  await saveDomain(page);

  const row = page.locator('tbody tr').filter({ hasText: DOMAIN });
  await expect(row.getByText('only from')).toHaveCount(0);

  // A cleared list means the domain is open to any source again, which is what
  // it was before this setting existed.
  await row.getByRole('button', { name: 'Edit' }).click();
  await expect(page.locator('input[name="editNetworks"]')).toHaveValue('');
  await expect(page.locator('[data-testid="header-rule-0"]')).toHaveCount(0);
});
