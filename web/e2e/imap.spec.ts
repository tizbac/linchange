import { expect, test } from '@playwright/test';
import { ImapFlow } from 'imapflow';
import * as net from 'net';

// These tests drive the IMAP server directly, over its implicit-TLS port, and
// prove the two things an IMAP client has to get right before anything else:
// listing the folders, and listing the messages inside them. They run for a
// personal mailbox and for a shared one, which is a separate principal with its
// own folders and mail.
//
// Mail is injected through the inbound SMTP port rather than sent from the web
// UI: the point here is the IMAP read path, and an SMTP delivery removes the
// browser from the loop so a failure is unambiguously the server's.

const USER = process.env.E2E_USER ?? 'e2e-user@example.com';
const PASS = process.env.E2E_PASSWORD ?? 'correct horse battery staple';
// The seed creates this shared mailbox and grants the user editor access to it.
const SHARED = process.env.E2E_SHARED ?? 'team@example.com';
const RUN = Date.now().toString(36);

const HOST = '127.0.0.1';
const IMAPS_PORT = Number(process.env.E2E_IMAPS_PORT ?? 1993);
const SMTP_PORT = Number(process.env.E2E_SMTP_PORT ?? 1025);

// The stack uses a self-signed certificate; this is a test-only process, so the
// certificate is not verified.
process.env.NODE_TLS_REJECT_UNAUTHORIZED = '0';

const STANDARD_FOLDERS = ['INBOX', 'Sent', 'Drafts', 'Archive', 'Junk', 'Trash'];

const sleep = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms));

// A fresh connection per use, so a mailbox opened earlier never masks a message
// that arrived since.
function connect(user: string): ImapFlow {
  return new ImapFlow({
    host: HOST,
    port: IMAPS_PORT,
    secure: true,
    auth: { user, pass: PASS },
    logger: false,
  });
}

async function withRetry<T>(fn: () => Promise<T>, retries = 5, delayMs = 1000): Promise<T> {
  let lastError: unknown;
  for (let attempt = 0; attempt < retries; attempt++) {
    try {
      return await fn();
    } catch (error) {
      lastError = error;
      if (attempt < retries - 1) {
        await sleep(delayMs);
      }
    }
  }
  throw lastError;
}

// listFolders returns the folder paths the session can see.
async function listFolders(user: string): Promise<string[]> {
  const imap = connect(user);
  await withRetry(() => imap.connect());
  try {
    const mailboxes = await imap.list();
    return mailboxes.map((mailbox) => mailbox.path);
  } finally {
    await imap.logout();
  }
}

// inboxSubjects opens INBOX and returns the subject of every message in it.
async function inboxSubjects(user: string): Promise<string[]> {
  const imap = connect(user);
  await withRetry(() => imap.connect());
  try {
    const lock = await imap.getMailboxLock('INBOX');
    try {
      const subjects: string[] = [];
      // "1:*" is the first fetch almost every client sends; it must return the
      // messages, not an empty set.
      for await (const message of imap.fetch('1:*', { envelope: true, uid: true })) {
        subjects.push(message.envelope?.subject ?? '');
      }
      return subjects;
    } finally {
      lock.release();
    }
  } finally {
    await imap.logout();
  }
}

// waitForSubject polls INBOX until the subject arrives, because SMTP delivery
// is asynchronous relative to the test.
async function waitForSubject(user: string, subject: string): Promise<boolean> {
  const deadline = Date.now() + 25_000;
  while (Date.now() < deadline) {
    try {
      if ((await inboxSubjects(user)).includes(subject)) {
        return true;
      }
    } catch {
      // A transient connection or authentication hiccup is retried.
    }
    await sleep(1000);
  }
  return false;
}

// injectMail delivers a plain message to a local recipient over the inbound
// SMTP port, so it takes the same path real mail does.
function injectMail(to: string, subject: string): Promise<void> {
  const message = [
    'From: e2e-sender@example.com',
    `To: ${to}`,
    `Subject: ${subject}`,
    `Date: ${new Date().toUTCString()}`,
    'Content-Type: text/plain; charset="utf-8"',
    '',
    `Body of ${subject}.`,
    '',
  ].join('\r\n');

  // The dialogue is a sequence of writes, each prompted by the previous reply.
  const steps = [
    () => 'EHLO e2e\r\n',
    () => 'MAIL FROM:<e2e-sender@example.com>\r\n',
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
      if (done) {
        return;
      }
      done = true;
      socket.destroy();
      if (error) {
        reject(error);
      } else {
        resolve();
      }
    };

    socket.on('connect', () => socket.setNoDelay(true));

    socket.on('data', (chunk: Buffer) => {
      buffer += chunk.toString('utf8');
      for (;;) {
        const end = buffer.indexOf('\r\n');
        if (end < 0) {
          return;
        }
        const line = buffer.slice(0, end);
        buffer = buffer.slice(end + 2);

        // Multi-line replies use "-" after the code; only the final line uses
        // " " and marks the reply complete.
        if (!/^\d{3} /.test(line)) {
          continue;
        }

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

test.describe('IMAP personal mailbox', () => {
  test('lists the standard folders', async () => {
    const folders = await listFolders(USER);
    for (const name of STANDARD_FOLDERS) {
      expect(folders, `folder ${name} should be listed`).toContain(name);
    }
  });

  test('lists a delivered message in INBOX', async () => {
    const subject = `IMAP personal ${RUN}`;
    await injectMail(USER, subject);

    const found = await waitForSubject(USER, subject);
    expect(found, `INBOX should show ${subject}`).toBe(true);
  });
});

test.describe('IMAP shared mailbox', () => {
  // "user#shared" logs the member in as the shared mailbox itself, which is the
  // syntax Outlook and other clients use for a shared account.
  const sharedLogin = `${USER}#${SHARED}`;

  test('lists the shared mailbox folders', async () => {
    const folders = await listFolders(sharedLogin);
    for (const name of STANDARD_FOLDERS) {
      expect(folders, `shared folder ${name} should be listed`).toContain(name);
    }
  });

  test('lists a delivered message in the shared INBOX', async () => {
    const subject = `IMAP shared ${RUN}`;
    await injectMail(SHARED, subject);

    const found = await waitForSubject(sharedLogin, subject);
    expect(found, `shared INBOX should show ${subject}`).toBe(true);
  });
});
