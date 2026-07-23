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
    this.innerHTML = "";
    this.textContent = "";
    this.value = "";
    this.disabled = false;
    this.readOnly = false;
    this.scrollHeight = 0;
    this.scrollTop = 0;
    this.clientHeight = 0;
  }
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

function makePage({ live, archived, participant, mobile = makeMobileFixture() }) {
  const elements = new Map();
  const domReady = [];
  const requests = [];
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

  const history = [{ turn: 1, speaker: "console", userText: "hello", reply: "world", status: "completed" }];
  async function fetch(url, options = {}) {
    const path = String(url).replace(/^\/api/, "");
    requests.push({ path, options });
    if (path === "/agents") return response(live);
    if (path === "/archived-agents") return response(archived);
    if (path === "/contexts") return response([{
      contextId: "ctx-1", title: "historical context", agents: [participant],
      turns: 1, lastActivity: "2026-07-01T00:00:00Z", lastStatus: "completed",
    }]);
    if (path === "/rooms") return response([]);
    if (path.startsWith("/invocations")) return response([]);
    if (path.includes("/history/")) return response(history);
    if (path.endsWith("/config")) return response({ path: "/tmp/agent-card.yaml", yaml: "name: " + participant });
    if (path.endsWith("/chat")) return response({ taskId: "task-1", contextId: "ctx-1" }, 202);
    if (path.includes("/tasks/")) return response({
      status: { state: "completed" },
      history: [{ role: "agent", parts: [{ kind: "text", text: "sent" }] }],
    });
    throw new Error("unexpected fetch " + path);
  }

  const window = {
    document,
    crypto: { randomUUID: () => "ctx-new" },
    addEventListener() {},
    removeEventListener() {},
  };
  const sandbox = {
    console, document, window, fetch, navigator: { clipboard: { writeText: async () => {} } },
    localStorage: { getItem: () => null, setItem() {} },
    Notification: function () {},
    FormData: class {},
    crypto: window.crypto,
    setTimeout: (fn) => { setImmediate(fn); return 1; },
    clearTimeout() {},
    setInterval: () => 1,
    clearInterval() {},
    encodeURIComponent,
  };
  sandbox.globalThis = sandbox;
  vm.createContext(sandbox);
  vm.runInContext(appSource, sandbox, { filename: appPath });
  assert.equal(domReady.length, 1, "app should register one DOMContentLoaded handler");
  domReady[0]();
  return { elements, requests };
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

async function testUnavailableParticipant() {
  const page = makePage({ live: [], archived: [], participant: "missing" });
  await openParticipant(page);
  const detail = page.elements.get("detailCard").innerHTML;
  const row = page.elements.get("agentRows").children[0].innerHTML;
  assert.match(detail, /详情不可用|unavailable/i);
  assert.doesNotMatch(detail, /选择一个 agent 查看详情/);
  assert.doesNotMatch(row, /unknown/i);
  assert.equal(page.elements.get("sendBtn").disabled, true);
}

(async () => {
  testRejectsPartialMobileSurfaces();
  testRejectsIncompleteMobileButtons();
  testRejectsMobileButtonsWithoutSurfaces();
  await testArchivedParticipant();
  await testLiveParticipant();
  await testUnavailableParticipant();
  console.log("participant selection regression tests passed");
})().catch((err) => {
  console.error(err.stack || err);
  process.exitCode = 1;
});
