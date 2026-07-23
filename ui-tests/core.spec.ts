import { test, expect } from '@playwright/test';
import { collectPageErrors, resetScenario } from './helpers';

test('core page settles with all primary rails', async ({ page, request }) => {
  await resetScenario(request, 'core');
  const errors = collectPageErrors(page);
  await page.goto('/');
  await expect(page.locator('#schedLabel')).toHaveText('scheduler · 1 agents');
  await expect(page.locator('#contexts .sess')).toHaveCount(18);
  await expect(page.locator('#rooms .sess')).toHaveCount(14);
  await expect(page.locator('#archived .sess')).toHaveCount(8);
  await expect(page.locator('#contexts')).not.toContainText('加载中…');
  expect(errors).toEqual([]);
});

test('empty state settles without a writable composer', async ({ page, request }) => {
  await resetScenario(request, 'empty');
  const errors = collectPageErrors(page);
  await page.goto('/');
  await expect(page.locator('#schedLabel')).toHaveText('scheduler · 0 agents');
  await expect(page.locator('#contexts')).toContainText('还没有对话');
  await expect(page.locator('#sendBtn')).toBeDisabled();
  expect(errors).toEqual([]);
});

test('scheduler failure is explicit and leaves a rendered shell', async ({ page, request }) => {
  await resetScenario(request, 'scheduler-error');
  const errors = collectPageErrors(page);
  await page.goto('/');
  await expect(page.locator('#schedLabel')).toHaveText('scheduler 不可达');
  await expect(page.locator('.app')).toBeVisible();
  await expect(page.locator('#contexts')).not.toContainText('加载中…');
  const expectedNetworkError =
    /^console: Failed to load resource: the server responded with a status of (502 \(Bad Gateway\)|503 \(Service Unavailable\))$/;
  const unexpectedErrors = errors.filter(error => !expectedNetworkError.test(error));
  expect(unexpectedErrors).toEqual([]);
});
