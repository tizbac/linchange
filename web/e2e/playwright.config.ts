import { defineConfig } from '@playwright/test';
import * as path from 'path';

// The stack boots from a fresh database on alternate ports, so a normal
// `make dev` instance can keep running while the tests go.
const port = Number(process.env.E2E_PORT ?? 18080);
const base = `http://127.0.0.1:${port}`;

export default defineConfig({
  testDir: '.',
  testMatch: /.*\.spec\.ts/,
  timeout: 90_000,
  expect: { timeout: 15_000 },
  fullyParallel: false,
  workers: 1,
  retries: 0,
  reporter: [['list']],
  outputDir: path.join(__dirname, 'test-results'),
  use: { baseURL: base, trace: 'retain-on-failure' },
  webServer: {
    command: `bash "${path.join(__dirname, 'stack.sh')}"`,
    url: `${base}/healthz`,
    timeout: 240_000,
    reuseExistingServer: false,
  },
});
