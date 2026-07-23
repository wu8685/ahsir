import { expect, resetScenario, test } from './helpers';

type RailListID = 'rooms' | 'contexts' | 'archived';

type RailBox = {
  top: number;
  bottom: number;
  height: number;
  client: number;
  scroll: number;
  minHeight: number;
  overflowY: string;
};

type RailScrollMetric = {
  before: number;
  after: number;
  positions: Record<RailListID, number>;
};

type RailMetrics = {
  boxes: Record<RailListID, RailBox>;
  scrolls: Record<RailListID, RailScrollMetric>;
  railTop: number;
  railBottom: number;
  railLeft: number;
  railRight: number;
};

function isAgentChatRequest(url: string, method: string) {
  return method === 'POST' && /^\/api\/agents\/[^/]+\/chat$/.test(new URL(url).pathname);
}

test('core page settles with all primary rails', async ({ page, request }) => {
  await resetScenario(request, 'core');
  await page.goto('/');
  await expect(page.locator('#schedLabel')).toHaveText('scheduler · 1 agents');
  await expect(page.locator('#contexts .sess')).toHaveCount(18);
  await expect(page.locator('#rooms .sess')).toHaveCount(14);
  await expect(page.locator('#archived .sess')).toHaveCount(8);
  await expect(page.locator('#contexts')).not.toContainText('加载中…');
});

test('desktop left rail preserves conversations and independent scrolling', async ({ page, request }) => {
  await resetScenario(request, 'core');
  await page.setViewportSize({ width: 1440, height: 900 });
  await page.goto('/');

  const rail = page.locator('.rail.left');
  await expect(rail).toBeVisible();
  await expect(page.locator('#contexts .sess')).toHaveCount(18);
  for (const id of ['rooms', 'contexts', 'archived'] as const) {
    await expect(page.locator(`#${id}`)).toBeVisible();
  }

  const metrics = await rail.evaluate((element): RailMetrics => {
    const ids: RailListID[] = ['rooms', 'contexts', 'archived'];
    const boxes = {} as Record<RailListID, RailBox>;
    for (const id of ids) {
      const list = element.querySelector<HTMLElement>(`#${id}`);
      if (!list) throw new Error(`left rail list #${id} is missing`);
      const rect = list.getBoundingClientRect();
      boxes[id] = {
        top: rect.top,
        bottom: rect.bottom,
        height: rect.height,
        client: list.clientHeight,
        scroll: list.scrollHeight,
        minHeight: Number.parseFloat(getComputedStyle(list).minHeight),
        overflowY: getComputedStyle(list).overflowY,
      };
    }
    const lists = ids.reduce((accumulator, id) => {
      const list = element.querySelector<HTMLElement>(`#${id}`);
      if (!list) throw new Error(`left rail list #${id} is missing`);
      accumulator[id] = list;
      return accumulator;
    }, {} as Record<RailListID, HTMLElement>);
    for (const id of ids) lists[id].scrollTop = 0;
    const scrolls = {} as Record<RailListID, RailScrollMetric>;
    for (const id of ids) {
      const list = lists[id];
      const before = list.scrollTop;
      list.scrollTop = 1;
      const positions = {} as Record<RailListID, number>;
      for (const otherID of ids) positions[otherID] = lists[otherID].scrollTop;
      scrolls[id] = { before, after: list.scrollTop, positions };
      list.scrollTop = 0;
    }
    const railRect = element.getBoundingClientRect();
    return {
      boxes,
      scrolls,
      railTop: railRect.top,
      railBottom: railRect.bottom,
      railLeft: railRect.left,
      railRight: railRect.right,
    };
  });
  expect(metrics.railTop).toBeGreaterThanOrEqual(-1);
  expect(metrics.railBottom).toBeLessThanOrEqual(900 + 1);
  expect(metrics.railLeft).toBeGreaterThanOrEqual(-1);
  expect(metrics.railRight).toBeLessThanOrEqual(1440 + 1);
  expect(metrics.boxes.contexts.height).toBeGreaterThanOrEqual(120);
  expect(metrics.boxes.contexts.minHeight).toBeGreaterThanOrEqual(120);
  expect(metrics.boxes.rooms.height).toBeLessThanOrEqual(900 * 0.26 + 1);
  expect(metrics.boxes.archived.height).toBeLessThanOrEqual(900 * 0.26 + 1);
  expect(metrics.boxes.rooms.bottom).toBeLessThanOrEqual(metrics.boxes.contexts.top + 1);
  expect(metrics.boxes.contexts.bottom).toBeLessThanOrEqual(metrics.boxes.archived.top + 1);
  for (const id of ['rooms', 'contexts', 'archived'] as const) {
    const box = metrics.boxes[id];
    expect(box.top).toBeGreaterThanOrEqual(metrics.railTop - 1);
    expect(box.bottom).toBeLessThanOrEqual(metrics.railBottom + 1);
    expect(box.scroll).toBeGreaterThan(box.client);
    expect(['auto', 'scroll']).toContain(box.overflowY);
    expect(metrics.scrolls[id].before).toBe(0);
    expect(metrics.scrolls[id].after).toBeGreaterThan(metrics.scrolls[id].before);
    for (const otherID of ['rooms', 'contexts', 'archived'] as const) {
      if (otherID !== id) expect(metrics.scrolls[id].positions[otherID]).toBe(0);
    }
  }
});

test('narrow screen switches conversations, chat, and details without overflow', async ({
  page,
  request,
}) => {
  await resetScenario(request, 'core');
  await page.setViewportSize({ width: 800, height: 900 });
  await page.goto('/');

  const leftButton = page.locator('#mobileLeft');
  const chatButton = page.locator('#mobileChat');
  const detailButton = page.locator('#mobileDetail');
  const left = page.locator('.rail.left');
  const center = page.locator('main.center');
  const right = page.locator('.rail.right');
  const expectNoHorizontalOverflow = async (surface: string) => {
    const overflow = await page.evaluate(
      () => document.documentElement.scrollWidth - document.documentElement.clientWidth,
    );
    expect(overflow, `${surface} surface horizontal overflow`).toBeLessThanOrEqual(1);
  };

  await expect(page.locator('.mob')).toBeVisible();
  await expect(leftButton).toHaveText('会话');
  await expect(chatButton).toHaveText('聊天');
  await expect(detailButton).toHaveText('详情');
  await expect(center).toBeVisible();
  await expect(left).toBeHidden();
  await expect(right).toBeHidden();
  await expect(leftButton).toHaveAttribute('aria-pressed', 'false');
  await expect(chatButton).toHaveAttribute('aria-pressed', 'true');
  await expect(detailButton).toHaveAttribute('aria-pressed', 'false');
  await expectNoHorizontalOverflow('chat');

  await detailButton.click();
  await expect(right).toBeVisible();
  await expect(left).toBeHidden();
  await expect(center).toBeHidden();
  await expect(page.locator('#detailCard')).toBeVisible();
  await expect(page.locator('#detailCard')).toContainText('live-codex');
  await expect(leftButton).toHaveAttribute('aria-pressed', 'false');
  await expect(chatButton).toHaveAttribute('aria-pressed', 'false');
  await expect(detailButton).toHaveAttribute('aria-pressed', 'true');
  await expectNoHorizontalOverflow('detail');

  await leftButton.click();
  await expect(left).toBeVisible();
  await expect(center).toBeHidden();
  await expect(right).toBeHidden();
  await expect(leftButton).toHaveAttribute('aria-pressed', 'true');
  await expect(chatButton).toHaveAttribute('aria-pressed', 'false');
  await expect(detailButton).toHaveAttribute('aria-pressed', 'false');
  await expectNoHorizontalOverflow('conversations');

  await chatButton.click();
  await expect(center).toBeVisible();
  await expect(left).toBeHidden();
  await expect(right).toBeHidden();
  await expect(chatButton).toHaveAttribute('aria-pressed', 'true');

  await detailButton.click();
  await expect(right).toBeVisible();
  await expect(detailButton).toHaveAttribute('aria-pressed', 'true');
  await chatButton.click();
  await expect(center).toBeVisible();
  await expect(left).toBeHidden();
  await expect(right).toBeHidden();
  await expect(chatButton).toHaveAttribute('aria-pressed', 'true');
  await expectNoHorizontalOverflow('chat after repeated switching');
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

test('chat request detector includes query-string URLs', async ({ page, request }) => {
  await resetScenario(request, 'core');
  let chatRequests = 0;
  page.on('request', r => {
    if (isAgentChatRequest(r.url(), r.method())) chatRequests++;
  });
  await page.goto('/');
  const status = await page.evaluate(async () => {
    const response = await fetch('/api/agents/live-codex/chat?e2e=pathname', {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ message: 'query-string sensitivity check' }),
    });
    return response.status;
  });
  expect(status).toBe(202);
  await expect.poll(() => chatRequests).toBe(1);
});

test('archived participant is detailed but read-only', async ({ page, request }) => {
  await resetScenario(request, 'core');
  let chatRequests = 0;
  page.on('request', r => {
    if (isAgentChatRequest(r.url(), r.method())) chatRequests++;
  });
  await page.goto('/');
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
