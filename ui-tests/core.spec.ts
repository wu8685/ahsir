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

test('live participant details and chat remain usable', async ({ page, request }) => {
  await resetScenario(request, 'core');
  await page.goto('/');
  await page.locator('#contexts .sess', { hasText: 'Existing core question' }).click();
  await page.locator('#agentRows .agent-row', { hasText: 'live-codex' }).click();
  await expect(page.locator('#detailCard')).toContainText('deterministic live agent');
  await expect(page.locator('#detailCard')).toContainText('http://127.0.0.1:9802');
  await expect(page.locator('#detailCard')).toContainText('1.2.3-e2e');
  await expect(page.locator('#sendBtn')).toBeEnabled();
  await page.locator('#ta').fill('E2E ping');
  const chat = page.waitForRequest(
    r => r.url().endsWith('/api/agents/live-codex/chat') && r.method() === 'POST',
  );
  await page.locator('#sendBtn').click();
  await chat;
  await expect(page.locator('#thread')).toContainText('E2E fixed reply');
});

test('archived participant is detailed but read-only', async ({ page, request }) => {
  await resetScenario(request, 'core');
  await page.goto('/');
  let chatRequests = 0;
  page.on('request', r => {
    if (/\/api\/agents\/.*\/chat$/.test(r.url())) chatRequests++;
  });
  await page.locator('#archived .sess', { hasText: 'Archived core context' }).click();
  await expect(page.locator('#detailCard')).toContainText('archived-kimi');
  await expect(page.locator('#detailCard')).toContainText('已归档 · 只读');
  await expect(page.locator('#detailCard')).not.toContainText('选择一个 agent 查看详情');
  await expect(page.locator('#thread')).toContainText('Archived retained reply');
  await expect(page.locator('#ta')).toBeDisabled();
  await expect(page.locator('#sendBtn')).toBeDisabled();
  await page.waitForTimeout(100);
  expect(chatRequests).toBe(0);
});
