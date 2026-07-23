import { expect, resetScenario, test } from './helpers';

test('core page settles with all primary rails', async ({ page, request }) => {
  await resetScenario(request, 'core');
  await page.goto('/');
  await expect(page.locator('#schedLabel')).toHaveText('scheduler · 1 agents');
  await expect(page.locator('#contexts .sess')).toHaveCount(18);
  await expect(page.locator('#rooms .sess')).toHaveCount(14);
  await expect(page.locator('#archived .sess')).toHaveCount(8);
  await expect(page.locator('#contexts')).not.toContainText('加载中…');
});

test('empty state settles without a writable composer', async ({ page, request }) => {
  await resetScenario(request, 'empty');
  await page.goto('/');
  await expect(page.locator('#schedLabel')).toHaveText('scheduler · 0 agents');
  await expect(page.locator('#contexts')).toContainText('还没有对话');
  await expect(page.locator('#sendBtn')).toBeDisabled();
});

test('scheduler failure is explicit and leaves a rendered shell', async ({
  browserDiagnostics,
  page,
  request,
}) => {
  await resetScenario(request, 'scheduler-error');
  browserDiagnostics.allowFailedResponse('/api/agents', 503);
  browserDiagnostics.allowFailedResponse('/api/archived-agents', 503);
  browserDiagnostics.allowFailedResponse('/api/contexts', 502);
  browserDiagnostics.allowFailedResponse('/api/rooms', 503);
  await page.goto('/');
  await expect(page.locator('#schedLabel')).toHaveText('scheduler 不可达');
  await expect(page.locator('.app')).toBeVisible();
  await expect(page.locator('#contexts')).not.toContainText('加载中…');
});
