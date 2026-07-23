import { defineConfig, devices } from '@playwright/test';

export default defineConfig({
  testDir: '.',
  testMatch: 'core.spec.ts',
  outputDir: 'test-results',
  workers: 1,
  retries: process.env.CI ? 1 : 0,
  reporter: process.env.CI ? [['github'], ['html', { open: 'never' }]] : 'list',
  use: {
    baseURL: 'http://127.0.0.1:19809',
    screenshot: 'only-on-failure',
    serviceWorkers: 'block',
    trace: 'retain-on-failure',
    video: 'off',
    ...devices['Desktop Chrome'],
  },
  webServer: {
    command: "sh -c 'mkdir -p ui-tests/test-results && exec go run ./internal/ui/e2e/testserver >ui-tests/test-results/fixture.log 2>&1'",
    cwd: '..',
    url: 'http://127.0.0.1:19809/',
    reuseExistingServer: false,
    stdout: 'ignore',
    stderr: 'ignore',
    timeout: 120_000,
  },
});
