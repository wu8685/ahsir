import { APIRequestContext, Page, expect } from '@playwright/test';

export async function resetScenario(request: APIRequestContext, scenario: string) {
  const response = await request.post(`/__test/reset?scenario=${encodeURIComponent(scenario)}`);
  expect(response.status()).toBe(204);
}

export function collectPageErrors(page: Page) {
  const errors: string[] = [];
  page.on('pageerror', error => errors.push(`pageerror: ${error.message}`));
  page.on('console', message => {
    if (message.type() === 'error') errors.push(`console: ${message.text()}`);
  });
  return errors;
}
