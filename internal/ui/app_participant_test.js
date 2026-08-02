"use strict";

const assert = require("node:assert/strict");
const fs = require("node:fs");
const vm = require("node:vm");

const appPath = process.argv[2];
if (!appPath) throw new Error("usage: node app_participant_test.js <app.js>");
const appSource = fs.readFileSync(appPath, "utf8");

class FakeClassList {
  constructor(owner) { this.owner = owner; }
  _set() { return new Set((this.owner.className || "").split(/\s+/).filter(Boolean)); }
  add(...names) { const s = this._set(); names.forEach((n) => s.add(n)); this.owner.className = [...s].join(" "); }
  remove(...names) { const s = this._set(); names.forEach((n) => s.delete(n)); this.owner.className = [...s].join(" "); }
  contains(name) { return this._set().has(name); }
  toggle(name, force) {
    const on = force === undefined ? !this.contains(name) : force;
    if (on) this.add(name); else this.remove(name);
    return on;
  }
}

class FakeElement {
  constructor(tag = "div") {
    this.nodeName = tag.toUpperCase();
    this.children = [];
    this.listeners = new Map();
    this.dataset = {};
    this.style = {};
    this.className = "";
    this.classList = new FakeClassList(this);
    this._innerHTML = "";
    this.textContent = "";
    this.value = "";
    this.disabled = false;
    this.readOnly = false;
    this.scrollHeight = 0;
    this.scrollTop = 0;
    this.clientHeight = 0;
  }
  set innerHTML(value) { this._innerHTML = String(value); this.children = []; }
  get innerHTML() { return this._innerHTML; }
  appendChild(child) { this.children.push(child); child.parentNode = this; return child; }
  after() {}
  addEventListener(type, fn) { this.listeners.set(type, fn); }
  click() { const fn = this.listeners.get("click"); return fn && fn({ currentTarget: this, target: this }); }
  focus() {}
  setAttribute(name, value) { this[name] = String(value); }
  getAttribute(name) { return this[name] || null; }
  querySelector() { return null; }
  querySelectorAll() { return []; }
  scrollTo() {}
  scrollIntoView() {}
}

function response(value, status = 200) {
  return {
    ok: status >= 200 && status < 300,
    status,
    statusText: String(status),
    async json() { return value; },
    clone() { return this; },
  };
}

function makeMobileFixture(surfaceNames = [], buttonSurfaceNames = []) {
  const surfaces = new Map(surfaceNames.map((name) => [name, new FakeElement()]));
  const buttons = buttonSurfaceNames.map((surface) => {
    const button = new FakeElement("button");
    button.dataset.surface = surface;
    if (surface === "center") {
      button.className = "on";
      button.setAttribute("aria-pressed", "true");
    } else {
      button.setAttribute("aria-pressed", "false");
    }
    return button;
  });
  return { buttons, surfaces };
}

async function waitFor(check, label) {
  for (let i = 0; i < 100; i++) {
    if (check()) return;
    await new Promise((resolve) => setImmediate(resolve));
  }
  throw new Error("timed out waiting for " + label);
}

function makePage({ live, liveSequence, archived, participant, mobile = makeMobileFixture(), context, historyValue, historyError, liveEvents = [], agentsError }) {
  const elements = new Map();
  const domReady = [];
  const requests = [];
  const intervals = [];
  let agentFetches = 0;
  const ids = [
    "themeBtn", "thread", "jumpBtn", "ta", "sendBtn", "newConvo", "newRoom",
    "newRoundtable", "newAgent", "agentSel", "schedLabel", "contexts", "archived",
    "rooms", "agentRows", "agentCount", "detailName", "detailCard", "ctxTitle",
    "ctxId", "trace", "toast", "toastMsg",
  ];
  ids.forEach((id) => elements.set(id, new FakeElement(id === "ta" ? "textarea" : "div")));
  elements.get("agentSel").nodeName = "SELECT";
  elements.get("sendBtn").nodeName = "BUTTON";

  const document = {
    documentElement: new FakeElement("html"),
    head: new FakeElement("head"),
    hidden: false,
    title: "ahsir",
    addEventListener(type, fn) { if (type === "DOMContentLoaded") domReady.push(fn); },
    querySelector(sel) {
      if (["#detailCard .ch", "#da-stop", "#da-restart", "#da-del"].includes(sel)) return new FakeElement();
      if (sel === ".rail.left") return mobile.surfaces.get("left") || null;
      if (sel === "main.center") return mobile.surfaces.get("center") || null;
      if (sel === ".rail.right") return mobile.surfaces.get("right") || null;
      if (/^#[A-Za-z0-9_-]+$/.test(sel)) return elements.get(sel.slice(1)) || null;
      return null;
    },
    querySelectorAll(sel) { return sel === ".mob button" ? mobile.buttons : []; },
    getElementById(id) { return elements.get(id) || null; },
    createElement(tag) { return new FakeElement(tag); },
    hasFocus() { return true; },
  };

  const history = historyValue === undefined
    ? [{ turn: 1, speaker: "console", userText: "hello", reply: "world", status: "completed" }]
    : historyValue;
  async function fetch(url, options = {}) {
    const path = String(url).replace(/^\/api/, "");
    requests.push({ path, options });
    if (path === "/agents") {
      if (agentsError) return response({ error: agentsError }, 503);
      const sequence = liveSequence || [live];
      const value = sequence[Math.min(agentFetches, sequence.length - 1)];
      agentFetches++;
      return response(value);
    }
    if (path === "/archived-agents") return response(archived);
    if (path === "/contexts") return response([context || {
      contextId: "ctx-1", title: "historical context", agents: [participant],
      turns: 1, lastActivity: "2026-07-01T00:00:00Z", lastStatus: "completed",
    }]);
    if (path === "/rooms") return response([]);
    if (path.startsWith("/invocations")) return response([]);
    if (path.includes("/history/")) return historyError ? response({ error: historyError }, 502) : response(history);
    if (path.startsWith("/context-events")) return response(liveEvents);
    if (path.endsWith("/config")) return response({ path: "/tmp/agent-card.yaml", yaml: "name: " + participant });
    if (path.endsWith("/chat")) return response({ taskId: "task-1", contextId: "ctx-1" }, 202);
    if (path.includes("/tasks/")) return response({
      status: { state: "completed" },
      history: [{ role: "agent", parts: [{ kind: "text", text: "sent" }] }],
    });
    throw new Error("unexpected fetch " + path);
  }

  const eventSources = [];
  class FakeEventSource {
    constructor(url) { this.url = url; this.listeners = new Map(); this.closed = false; eventSources.push(this); }
    addEventListener(type, fn) { this.listeners.set(type, fn); }
    close() { this.closed = true; }
    emit(type, value) { const fn = this.listeners.get(type); if (fn) fn({ data: JSON.stringify(value), lastEventId: value.id || "" }); }
  }
  const window = {
    document,
    crypto: { randomUUID: () => "ctx-new" },
    addEventListener() {},
    removeEventListener() {}, EventSource: FakeEventSource,
  };
  const sandbox = {
    console, document, window, fetch, navigator: { clipboard: { writeText: async () => {} } },
    localStorage: { getItem: () => null, setItem() {} },
    Notification: function () {},
    FormData: class {},
    crypto: window.crypto,
    setTimeout: (fn) => { setImmediate(fn); return 1; },
    clearTimeout() {},
    setInterval: (fn) => { intervals.push(fn); return intervals.length; },
    clearInterval() {},
    encodeURIComponent, EventSource: FakeEventSource,
  };
  sandbox.globalThis = sandbox;
  vm.createContext(sandbox);
  vm.runInContext(appSource, sandbox, { filename: appPath });
  assert.equal(domReady.length, 1, "app should register one DOMContentLoaded handler");
  domReady[0]();
  return { elements, requests, eventSources, intervals };
}

function treeText(node) {
  if (!node) return "";
  return [node.textContent || "", node.innerHTML || "", ...(node.children || []).map(treeText)].join(" ");
}

function testRejectsPartialMobileSurfaces() {
  const mobile = makeMobileFixture(["left", "center"], ["left", "center", "right"]);
  assert.throws(
    () => makePage({ live: [], archived: [], participant: "missing", mobile }),
    /mobile navigation.*right/i,
  );
}

function testRejectsIncompleteMobileButtons() {
  const mobile = makeMobileFixture(["left", "center", "right"], ["left", "center"]);
  assert.throws(
    () => makePage({ live: [], archived: [], participant: "missing", mobile }),
    /mobile navigation.*right/i,
  );
  const centerButton = mobile.buttons.find((button) => button.dataset.surface === "center");
  assert.equal(centerButton.classList.contains("on"), false, "invalid navigation must clear static selection");
  assert.equal(centerButton.getAttribute("aria-pressed"), "false", "invalid navigation must clear pressed state");
}

function testRejectsMobileButtonsWithoutSurfaces() {
  const mobile = makeMobileFixture([], ["left", "center", "right"]);
  assert.throws(
    () => makePage({ live: [], archived: [], participant: "missing", mobile }),
    /mobile navigation.*left.*center.*right/i,
  );
  mobile.buttons.forEach((button) => {
    assert.equal(button.classList.contains("on"), false, "orphaned navigation must clear static selection");
    assert.equal(button.getAttribute("aria-pressed"), "false", "orphaned navigation must clear pressed state");
  });
}

async function openParticipant(page) {
  await waitFor(() => page.elements.get("contexts").children.length >= 2, "context row");
  await page.elements.get("contexts").children[1].click();
  await waitFor(() => page.elements.get("agentRows").children.length === 1, "participant row");
  await page.elements.get("agentRows").children[0].click();
  await waitFor(() => page.requests.some((r) => r.path.includes("/history/ctx-1")), "transcript request");
}

async function testArchivedParticipant() {
  const page = makePage({
    live: [], participant: "ghost",
    archived: [{ name: "ghost", contexts: [{ contextId: "ctx-1", title: "historical context", turns: 1, lastStatus: "completed" }] }],
  });
  await openParticipant(page);
  const detail = page.elements.get("detailCard").innerHTML;
  const row = page.elements.get("agentRows").children[0].innerHTML;
  assert.match(detail, /已归档|archived/i);
  assert.doesNotMatch(detail, /选择一个 agent 查看详情/);
  assert.doesNotMatch(row, /unknown/i);
  assert.equal(page.elements.get("agentSel").value, "", "archived agent must not be a send target");
  assert.ok(page.elements.get("ta").readOnly || page.elements.get("ta").disabled, "archived composer must be read-only");
  assert.equal(page.elements.get("sendBtn").disabled, true, "archived send button must be disabled");
}

async function testLiveParticipant() {
  const page = makePage({
    participant: "teacher", archived: [],
    live: [{ name: "teacher", url: "http://127.0.0.1:9802", status: "online", description: "live agent" }],
  });
  await openParticipant(page);
  assert.match(page.elements.get("detailCard").innerHTML, /live agent/);
  assert.equal(page.elements.get("agentSel").value, "teacher");
  assert.equal(page.elements.get("ta").readOnly, false);
  assert.equal(page.elements.get("ta").disabled, false);
  assert.equal(page.elements.get("sendBtn").disabled, false);

  page.elements.get("ta").value = "dispatch me";
  await page.elements.get("sendBtn").click();
  await waitFor(() => page.requests.some((r) => r.path.endsWith("/teacher/chat")), "live chat dispatch");
}

async function testExternalInvocationShowsLiveProgress() {
  const page = makePage({
    participant: "coder", archived: [], historyValue: [],
    live: [{ name: "coder", status: "online" }],
    context: {
      contextId: "ctx-1", title: "build it", agents: ["coder"], turns: 1,
      lastActivity: "2026-08-02T00:00:00Z", lastStatus: "in_flight",
      invocationId: "inv-1", userText: "build it", speaker: "hetairoi",
    },
    liveEvents: [{ id: "live-1", type: "tool_use", name: "command_execution", input: { command: "go test ./..." } }],
  });
  await openParticipant(page);
  await waitFor(() => treeText(page.elements.get("thread")).includes("command_execution"), "live progress");
  const text = treeText(page.elements.get("thread"));
  assert.match(text, /build it/);
  assert.match(text, /go test \.\/\.\.\./);
  assert.match(text, /执行中|处理中/);
  assert.equal(page.eventSources.length, 1, "active invocation should subscribe to SSE");
}

async function testHistoryFailureIsNotRenderedAsEmptyConversation() {
  const page = makePage({
    participant: "coder", archived: [], historyError: "connection refused",
    live: [{ name: "coder", status: "online" }],
  });
  await openParticipant(page);
  await waitFor(() => treeText(page.elements.get("thread")).includes("connection refused"), "history error");
  const text = treeText(page.elements.get("thread"));
  assert.match(text, /无法读取会话记录/);
  assert.doesNotMatch(text, /还没有对话/);
}

async function testUnavailableParticipant() {
  const page = makePage({ live: [], archived: [], participant: "missing" });
  await openParticipant(page);
  const detail = page.elements.get("detailCard").innerHTML;
  const row = page.elements.get("agentRows").children[0].innerHTML;
  assert.match(detail, /该 Agent 已离线，且没有可用的归档详情/);
  assert.doesNotMatch(detail, /选择一个 agent 查看详情/);
  assert.doesNotMatch(row, /unknown/i);
  assert.equal(page.elements.get("sendBtn").disabled, true);
}

async function testActiveButUnselectedAgent() {
  const page = makePage({
    participant: "teacher", archived: [],
    live: [{ name: "teacher", url: "http://127.0.0.1:9802", status: "online" }],
  });
  await waitFor(() => page.elements.get("schedLabel").textContent === "scheduler · 1 agents", "active agents");
  const select = page.elements.get("agentSel");
  select.value = "";
  await select.listeners.get("change")({ target: select });
  assert.match(page.elements.get("detailCard").innerHTML, /选择一个 agent 查看详情/);
  assert.doesNotMatch(page.elements.get("detailCard").innerHTML, /当前没有运行中的 Agent/);
}

async function testNoActiveAgentDetail() {
  const page = makePage({ live: [], archived: [], participant: "missing" });
  await waitFor(() => page.elements.get("schedLabel").textContent === "scheduler · 0 agents", "empty agents");
  const detail = page.elements.get("detailCard").innerHTML;
  assert.match(detail, /当前没有运行中的 Agent/);
  assert.match(detail, /启动 Agent 后，可在这里查看运行状态和配置信息/);
  assert.doesNotMatch(detail, /选择一个 agent 查看详情/);
}

async function testNoAgentNewConversationShowsRecoveryState() {
  const page = makePage({ live: [], archived: [], participant: "missing" });
  await waitFor(() => page.elements.get("schedLabel").textContent === "scheduler · 0 agents", "empty agents");
  await page.elements.get("newConvo").click();
  const text = treeText(page.elements.get("thread"));
  assert.match(text, /当前没有可用 Agent/);
  assert.match(text, /配置新 Agent/);
  assert.match(text, /重新检查/);
  assert.equal(page.elements.get("ctxId").textContent, "等待 Agent", "must not expose a phantom context id");
  assert.equal(page.elements.get("ta").placeholder, "启动 Agent 后可开始对话");
  assert.equal(page.elements.get("ta").disabled, true);
  assert.equal(page.elements.get("sendBtn").disabled, true);
}

async function testSchedulerFailureNewConversationShowsReconnectState() {
  const page = makePage({ live: [], archived: [], participant: "missing", agentsError: "fixture scheduler unavailable" });
  await waitFor(() => page.elements.get("schedLabel").textContent === "scheduler 不可达", "scheduler error");
  await page.elements.get("newConvo").click();
  const text = treeText(page.elements.get("thread"));
  assert.match(text, /无法连接 scheduler/);
  assert.match(text, /重新连接/);
  assert.doesNotMatch(text, /当前没有可用 Agent/);
  assert.equal(page.elements.get("ctxId").textContent, "等待连接");
}

async function testNoAgentDraftRecoversWhenPollingFindsAgent() {
  const teacher = { name: "teacher", url: "http://127.0.0.1:9802", status: "online" };
  const page = makePage({
    live: [], liveSequence: [[], [teacher]], archived: [], participant: "teacher",
  });
  await waitFor(() => page.elements.get("schedLabel").textContent === "scheduler · 0 agents", "empty agents");
  await page.elements.get("newConvo").click();
  assert.match(treeText(page.elements.get("thread")), /当前没有可用 Agent/);
  assert.equal(page.intervals.length, 1, "chat rail polling interval");

  await page.intervals[0]();
  assert.equal(page.elements.get("schedLabel").textContent, "scheduler · 1 agents");
  assert.equal(page.elements.get("agentSel").value, "teacher");
  assert.equal(page.elements.get("ta").disabled, false);
  assert.equal(page.elements.get("sendBtn").disabled, false);
  assert.equal(page.elements.get("ctxId").textContent, "ctx · ctx-ne");
  assert.match(treeText(page.elements.get("thread")), /与 teacher 还没有对话/);
}

(async () => {
  testRejectsPartialMobileSurfaces();
  testRejectsIncompleteMobileButtons();
  testRejectsMobileButtonsWithoutSurfaces();
  await testArchivedParticipant();
  await testLiveParticipant();
  await testExternalInvocationShowsLiveProgress();
  await testHistoryFailureIsNotRenderedAsEmptyConversation();
  await testUnavailableParticipant();
  await testActiveButUnselectedAgent();
  await testNoActiveAgentDetail();
  await testNoAgentNewConversationShowsRecoveryState();
  await testSchedulerFailureNewConversationShowsReconnectState();
  await testNoAgentDraftRecoversWhenPollingFindsAgent();
  console.log("participant selection regression tests passed");
})().catch((err) => {
  console.error(err.stack || err);
  process.exitCode = 1;
});
