// ahsir web console — single-page client.
//
// Mental model (matches the scheduler, see internal/ui/server.go):
//   - A "conversation" in the left rail IS an A2A contextId. One contextId
//     chains several agents; each agent keeps its own session under it.
//   - You pick which agent to talk to (the composer's select). Sending posts an
//     async turn to that agent under the current contextId; we poll the task to
//     terminal, then reload the agent's per-context transcript.
//   - The right rail shows the agents that touched this context, the selected
//     agent's card, and the raw invocation trace.
//
// All data comes from /api/*, which the Go server proxies to the scheduler
// (plus the computed /api/contexts aggregation).

(function () {
  "use strict";

  const $ = (sel) => document.querySelector(sel);
  const api = (path) => "/api" + path;

  const state = {
    mode: "chat",      // "chat" | "room"
    contextId: null,   // null = unsaved "new conversation"
    agent: null,       // currently selected agent name
    agents: [],        // [{name, url, status, skills, ...}]
    archivedAgents: [],// offline agents with retained, read-only transcripts
    speaker: "console",
    roomId: null,      // active roundtable room
    roomPoll: null,    // interval handle while viewing a room
    roomRendered: null,// last room id rendered (to force scroll-to-bottom on open)
    roomSig: null,     // last rendered room fingerprint (skip redundant repaints)
    cfgCache: {},      // agent name -> {path, yaml} read-only config detail
    schedulerState: "loading", // "loading" | "ready" | "error"
    chatEmptyKind: null, // null | "no-agent" | "scheduler-error"
    contextSummary: null,
    contextTurns: [],
    liveEvents: [],
    liveSource: null,
    historyError: null,
  };

  // ---- tiny helpers -------------------------------------------------------

  async function getJSON(path) {
    const r = await fetch(api(path));
    if (!r.ok) throw new Error((await safeErr(r)) || r.statusText);
    return r.json();
  }
  async function postJSON(path, body) {
    const r = await fetch(api(path), {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    });
    if (!r.ok && r.status !== 202) throw new Error((await safeErr(r)) || r.statusText);
    return r.json();
  }
  async function apiDelete(path) {
    const r = await fetch(api(path), { method: "DELETE" });
    if (!r.ok && r.status !== 204) throw new Error((await safeErr(r)) || r.statusText);
    return r;
  }
  async function safeErr(r) {
    try { const j = await r.clone().json(); return j.error; } catch (_) { return null; }
  }
  // Wrap a value as a double-quoted shell token, escaping the four chars that
  // stay special inside double quotes. Used by the agent command generator so a
  // prompt with quotes/`$` pastes into a shell verbatim.
  function shq(s) {
    return '"' + String(s == null ? "" : s).replace(/[\\"$`]/g, (c) => "\\" + c) + '"';
  }
  async function copyText(text, okMsg, btn) {
    try {
      await navigator.clipboard.writeText(text);
      if (btn) flashBtn(btn, "✓ 已复制");
      if (okMsg) toast(okMsg);
      else if (!btn) toast("已复制到剪贴板");
    } catch (_) { toast("复制失败，请手动选择文本"); }
  }
  // Briefly swap a button's label to confirm an action, then restore it.
  function flashBtn(btn, label) {
    if (!btn || btn.dataset.flashing === "1") return;
    const orig = btn.textContent;
    btn.dataset.flashing = "1";
    btn.textContent = label;
    setTimeout(() => { btn.textContent = orig; btn.dataset.flashing = ""; }, 1500);
  }
  function el(tag, cls, html) {
    const e = document.createElement(tag);
    if (cls) e.className = cls;
    if (html != null) e.innerHTML = html;
    return e;
  }
  function esc(s) {
    return String(s == null ? "" : s)
      .replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
  }
  function short(id) { return id ? id.slice(0, 6) : "-"; }
  // A conversation IS an A2A contextId, and the client must own it: the
  // scheduler only records a turn against the contextId it was *given*. If we
  // let the agent auto-generate one (by sending none), the ledger records an
  // empty contextId and the transcript lands under a different id than the one
  // returned — so /api/contexts and history both come up empty. Generating it
  // here and sending it on every turn keeps the ledger, transcript, and UI in
  // agreement.
  function uuid() {
    if (window.crypto && crypto.randomUUID) return crypto.randomUUID();
    return "ctx-" + Date.now().toString(16) + "-" + Math.random().toString(16).slice(2, 10);
  }
  function timeAgo(iso) {
    if (!iso) return "";
    const d = (Date.now() - new Date(iso).getTime()) / 1000;
    if (d < 60) return Math.max(0, Math.floor(d)) + "s";
    if (d < 3600) return Math.floor(d / 60) + "m";
    if (d < 86400) return Math.floor(d / 3600) + "h";
    return Math.floor(d / 86400) + "d";
  }
  // Wall-clock HH:MM:SS for a timestamp in ms (local). "" for invalid input.
  function fmtClock(ms) {
    if (ms == null || isNaN(ms)) return "";
    const d = new Date(ms);
    const p = (n) => String(n).padStart(2, "0");
    return `${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
  }
  // A small message meta header: role + clock (full timestamp on hover).
  function msgHead(side, role, ms, iso) {
    return `<div class="msg-head ${side}"><span class="role">${esc(role)}</span>` +
      `<span class="t mono" title="${esc(iso || "")}">${fmtClock(ms)}</span></div>`;
  }

  // --- minimal, XSS-safe markdown -----------------------------------------
  // Agent replies are commonly markdown. We render a practical subset. Safety:
  // everything is HTML-escaped FIRST, then markdown tokens are turned into a
  // fixed set of tags — the model can never inject raw HTML/script, and links
  // are restricted to http(s)/mailto.
  function mdInline(s) {
    s = s.replace(/`([^`]+)`/g, (_, c) => `<code class="md-code">${c}</code>`);
    s = s.replace(/\*\*([^*]+)\*\*/g, "<strong>$1</strong>");
    s = s.replace(/__([^_]+)__/g, "<strong>$1</strong>");
    s = s.replace(/(^|[^*])\*([^*\s][^*]*)\*/g, "$1<em>$2</em>");
    s = s.replace(/\[([^\]]+)\]\((https?:\/\/[^\s)]+|mailto:[^\s)]+)\)/g,
      '<a href="$2" target="_blank" rel="noopener noreferrer">$1</a>');
    return s;
  }
  // GFM table helpers. Rows are already HTML-escaped (esc runs before block
  // parsing); the pipe `|` survives escaping, so cell splitting is safe here.
  function mdSplitRow(row) {
    let s = row.trim();
    if (s.startsWith("|")) s = s.slice(1);
    if (s.endsWith("|")) s = s.slice(0, -1);
    return s.split("|").map((c) => c.trim());
  }
  function mdIsSep(row) {
    if (row.indexOf("|") < 0) return false;
    const cells = mdSplitRow(row);
    return cells.length > 0 && cells.every((c) => /^:?-+:?$/.test(c));
  }
  function mdAlign(cell) {
    const l = cell.startsWith(":"), r = cell.endsWith(":");
    return l && r ? "center" : r ? "right" : l ? "left" : "";
  }

  // Lift GFM tables (header row + `|---|---|` separator + body rows) into the
  // blocks list as ready HTML, replacing each with a placeholder — same trick
  // as code fences, so the line-based block parser never has to look ahead.
  // Cells are pre-escape here, so esc() each before inline formatting.
  function mdExtractTables(text, blocks) {
    const lines = text.split("\n");
    const out = [];
    for (let i = 0; i < lines.length; i++) {
      if (lines[i].indexOf("|") >= 0 && i + 1 < lines.length && mdIsSep(lines[i + 1]) && !mdIsSep(lines[i])) {
        const headers = mdSplitRow(lines[i]);
        const aligns = mdSplitRow(lines[i + 1]).map(mdAlign);
        i += 2;
        const body = [];
        while (i < lines.length && lines[i].indexOf("|") >= 0 && lines[i].trim() !== "") {
          body.push(mdSplitRow(lines[i]));
          i++;
        }
        i--;
        const cell = (tag, c, idx) => `<${tag}${aligns[idx] ? ` style="text-align:${aligns[idx]}"` : ""}>${mdInline(esc(c || ""))}</${tag}>`;
        let html = '<table class="md-table"><thead><tr>';
        headers.forEach((h, idx) => { html += cell("th", h, idx); });
        html += "</tr></thead><tbody>";
        body.forEach((cells) => {
          html += "<tr>";
          for (let c = 0; c < headers.length; c++) html += cell("td", cells[c], c);
          html += "</tr>";
        });
        html += "</tbody></table>";
        blocks.push(html);
        out.push("\u0000B" + (blocks.length - 1) + "\u0000");
      } else {
        out.push(lines[i]);
      }
    }
    return out.join("\n");
  }

  function mdToHtml(src) {
    // 1. Lift fenced code blocks out so inline/block rules never touch them.
    const blocks = [];
    let text = String(src == null ? "" : src).replace(/```(\w*)\n?([\s\S]*?)```/g, (_, lang, code) => {
      blocks.push(`<pre class="md-pre"><code>${esc(code.replace(/\n$/, ""))}</code></pre>`);
      return "\u0000B" + (blocks.length - 1) + "\u0000";
    });
    text = mdExtractTables(text, blocks); // 1b. lift tables (cells self-escape)
    text = esc(text); // 2. escape everything else
    const out = [];
    let list = null; // 'ul' | 'ol'
    const closeList = () => { if (list) { out.push(`</${list}>`); list = null; } };
    for (const raw of text.split("\n")) {
      const ph = raw.match(/^\u0000B(\d+)\u0000$/);
      if (ph) { closeList(); out.push(blocks[+ph[1]]); continue; }
      let m;
      if ((m = raw.match(/^(#{1,6})\s+(.*)$/))) {
        closeList(); out.push(`<h${m[1].length} class="md-h">${mdInline(m[2])}</h${m[1].length}>`); continue;
      }
      if ((m = raw.match(/^&gt;\s?(.*)$/))) { closeList(); out.push(`<blockquote class="md-bq">${mdInline(m[1])}</blockquote>`); continue; }
      if (/^\s*([-*_])(\s*\1){2,}\s*$/.test(raw)) { closeList(); out.push('<hr class="md-hr">'); continue; }
      if ((m = raw.match(/^\s*[-*+]\s+(.*)$/))) {
        if (list !== "ul") { closeList(); out.push('<ul class="md-ul">'); list = "ul"; }
        out.push(`<li>${mdInline(m[1])}</li>`); continue;
      }
      if ((m = raw.match(/^\s*\d+\.\s+(.*)$/))) {
        if (list !== "ol") { closeList(); out.push('<ol class="md-ol">'); list = "ol"; }
        out.push(`<li>${mdInline(m[1])}</li>`); continue;
      }
      if (/^\s*$/.test(raw)) { closeList(); continue; }
      closeList();
      out.push(`<p class="md-p">${mdInline(raw)}</p>`);
    }
    closeList();
    return out.join("");
  }

  let toastTimer;
  function toast(msg) {
    $("#toastMsg").textContent = msg;
    $("#toast").classList.add("show");
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => $("#toast").classList.remove("show"), 2200);
  }

  function avatarColor(name) {
    let h = 0;
    for (let i = 0; i < name.length; i++) h = (h * 31 + name.charCodeAt(i)) % 360;
    return `hsl(${h},45%,42%)`;
  }
  function statusDot(status) {
    const map = { online: "s-ok", offline: "s-idle", unknown: "s-idle" };
    return map[status] || "s-idle";
  }

  // ---- agents -------------------------------------------------------------

  async function loadAgents() {
    let agents;
    try {
      agents = await getJSON("/agents");
    } catch (e) {
      state.schedulerState = "error";
      $("#schedLabel").textContent = "scheduler 不可达";
      toast("无法连接 scheduler：" + e.message);
      renderChatAvailability();
      return false;
    }
    state.schedulerState = "ready";
    state.agents = agents || [];
    $("#schedLabel").textContent = `scheduler · ${state.agents.length} agents`;

    const sel = $("#agentSel");
    sel.innerHTML = "";
    state.agents.forEach((a) => {
      const o = el("option");
      o.value = a.name;
      o.textContent = a.name;
      sel.appendChild(o);
    });
    if (!state.agents.length) {
      const o = el("option");
      o.value = "";
      o.textContent = "无可用 Agent";
      sel.appendChild(o);
    }
    if (!state.agent && state.agents.length) state.agent = state.agents[0].name;
    if (state.agent) sel.value = state.agent;
    renderDetail();
    renderChatAvailability();
    return true;
  }

  function agentByName(name) {
    return state.agents.find((a) => a.name === name);
  }

  function archivedAgentByName(name) {
    return state.archivedAgents.find((a) => a.name === name);
  }

  function setComposerWritable(writable, placeholder) {
    const ta = $("#ta"), wrap = document.querySelector(".cwrap");
    ta.readOnly = !writable;
    ta.disabled = !writable;
    ta.placeholder = writable
      ? "给 agent 下发任务…  ⌘↩ 发送 · 可拖入或粘贴文件插入路径"
      : (placeholder || "当前对话只读");
    $("#sendBtn").disabled = !writable;
    if (wrap) {
      wrap.classList.toggle("is-disabled", !writable);
      wrap.setAttribute("aria-disabled", String(!writable));
      if (!writable && state.chatEmptyKind) wrap.setAttribute("aria-describedby", "chatEmptyDescription");
      else if (typeof wrap.removeAttribute === "function") wrap.removeAttribute("aria-describedby");
    }
  }

  function renderChatAvailability() {
    if (state.mode !== "chat") return;
    const schedulerReady = state.schedulerState === "ready";
    const hasAgents = state.agents.length > 0;
    const writable = schedulerReady && !!agentByName(state.agent);
    const placeholder = !schedulerReady
      ? "连接 scheduler 后可开始对话"
      : !hasAgents
        ? "启动 Agent 后可开始对话"
        : "选择一个可用 Agent 后开始对话";
    setComposerWritable(writable, placeholder);
    $("#agentSel").disabled = !schedulerReady || !hasAgents;
  }

  function renderUnavailableDetail(name) {
    $("#detailName").textContent = name || "-";
    $("#detailCard").innerHTML =
      '<div class="muted-line">该 Agent 已离线，且没有可用的归档详情</div>';
  }

  function renderDetail() {
    const a = agentByName(state.agent);
    $("#detailName").textContent = state.agent || "-";
    const card = $("#detailCard");
    if (!a) {
      if (!state.agent) {
        if (!state.agents.length) {
          card.innerHTML =
            '<div class="muted-line">当前没有运行中的 Agent</div>' +
            '<div class="muted-line">启动 Agent 后，可在这里查看运行状态和配置信息</div>';
          return;
        }
        card.innerHTML = '<div class="muted-line">选择一个 agent 查看详情</div>';
        return;
      }
      if (archivedAgentByName(state.agent)) { renderArchivedDetail(state.agent); return; }
      renderUnavailableDetail(state.agent);
      return;
    }
    const skills = (a.skills || []).map((s) => `<span>${esc(s.name)}</span>`).join("");
    card.innerHTML = `
      <div class="ch"><span class="av" style="background:${avatarColor(a.name)}">${esc(a.name.slice(0, 2))}</span>
        <div class="t">${esc(a.name)}<small class="mono">${esc(a.description || "agent")}</small></div></div>
      <dl class="kv">
        <dt>status</dt><dd>${esc(a.status || "unknown")}</dd>
        <dt>endpoint</dt><dd>${esc(a.url || "")}</dd>
        <dt>version</dt><dd>${esc(a.version || "")}</dd>
      </dl>
      ${skills ? `<div class="skills">${skills}</div>` : ""}
      <div class="cfg-box" id="cfgBox"><div class="muted-line">加载配置…</div></div>
      <div class="detail-actions">
        <button class="stop" id="da-stop">停止下线</button>
        <button id="da-restart" title="重启该 agent，使手改的 agent-card.yaml 生效（会中断它当前那轮）">重启生效</button>
        <button class="linkbtn" id="da-del" title="复制 ahsir agent delete 命令，在 scheduler 主机执行可连同 ahsir.yaml 一起删除">复制删除命令</button>
      </div>
      <div class="da-note">改了 agent-card.yaml 后点「重启生效」(只重启该 agent，不影响 scheduler 和其他 agent)。「停止下线」会下线进程并移出注册表，但不删文件，scheduler 重启后会恢复。</div>`;
    $("#da-stop").addEventListener("click", (e) => armOrStop(e.currentTarget, a.name));
    $("#da-restart").addEventListener("click", (e) => armOrRestart(e.currentTarget, a.name));
    $("#da-del").addEventListener("click", (e) =>
      copyText(`ahsir agent delete ${shq(a.name)}`, "已复制删除命令 · 在 scheduler 主机执行", e.currentTarget));
    // The card header doubles as a drag handle — drop it on the composer to
    // insert "@<agent>". Only the header (not the whole card) so selecting the
    // config yaml below still works.
    setDragAgent($("#detailCard .ch"), a.name);
    loadAgentConfig(a.name);
  }

  // Read-only config detail: the full agent-card.yaml + its on-disk path (the
  // file to edit). Cached per agent so the roundtable poll's repeated
  // renderDetail calls don't refetch.
  async function loadAgentConfig(name) {
    if (!$("#cfgBox")) return;
    let cfg = state.cfgCache[name];
    if (!cfg) {
      try {
        cfg = await getJSON(`/agents/${encodeURIComponent(name)}/config`);
        state.cfgCache[name] = cfg;
      } catch (e) {
        if (state.agent === name && $("#cfgBox")) {
          $("#cfgBox").innerHTML = `<div class="muted-line">配置不可读：${esc(e.message)}</div>`;
        }
        return;
      }
    }
    if (state.agent !== name) return; // switched away while fetching
    const box = $("#cfgBox");
    if (!box) return;
    box.innerHTML =
      `<div class="cfg-head"><span>agent-card.yaml</span><button class="copy" id="cfg-copy">复制路径</button></div>` +
      `<code class="cfg-path">${esc(cfg.path || "-")}</code>` +
      (cfg.yaml ? `<details class="cfg-yaml"><summary>查看完整配置</summary><pre>${esc(cfg.yaml)}</pre></details>` : "");
    const cp = $("#cfg-copy");
    if (cp && cfg.path) cp.addEventListener("click", (e) => copyText(cfg.path, "已复制路径", e.currentTarget));
  }

  // Restart = re-read the (edited) agent-card.yaml. Inline two-step confirm like
  // stop, since it interrupts the agent's current turn.
  let restartArmTimer;
  function armOrRestart(btn, name) {
    if (btn.dataset.armed === "1") {
      clearTimeout(restartArmTimer);
      btn.dataset.armed = "";
      btn.classList.remove("armed");
      restartAgent(btn, name);
      return;
    }
    btn.dataset.armed = "1";
    btn.textContent = "确认重启？";
    btn.classList.add("armed");
    clearTimeout(restartArmTimer);
    restartArmTimer = setTimeout(() => {
      btn.dataset.armed = "";
      btn.textContent = "重启生效";
      btn.classList.remove("armed");
    }, 3000);
  }
  async function restartAgent(btn, name) {
    btn.textContent = "重启中…";
    btn.disabled = true;
    delete state.cfgCache[name]; // force a fresh config read after restart
    try {
      await postJSON(`/admin/agents/${encodeURIComponent(name)}/restart`, {});
      toast(`已重启 ${name}，配置已生效`);
      await loadAgents();
    } catch (e) {
      toast("重启失败：" + e.message);
      renderDetail();
    }
  }

  // Inline two-step confirm instead of a native confirm() (which breaks the dark
  // theme): first click arms the button ("确认下线？"), a second click within 3s
  // fires. The da-note under the buttons carries the semantics. Auto-disarms.
  let stopArmTimer;
  function armOrStop(btn, name) {
    if (btn.dataset.armed === "1") {
      clearTimeout(stopArmTimer);
      btn.dataset.armed = "";
      btn.classList.remove("armed");
      stopAgent(name);
      return;
    }
    btn.dataset.armed = "1";
    btn.textContent = "确认下线？";
    btn.classList.add("armed");
    clearTimeout(stopArmTimer);
    stopArmTimer = setTimeout(() => {
      btn.dataset.armed = "";
      btn.textContent = "停止下线";
      btn.classList.remove("armed");
    }, 3000);
  }

  // Stop = the one lifecycle action the console performs directly. It hits the
  // admin DELETE (proxied with the control-plane token); the agent then drops
  // out of /agents. It does NOT touch ahsir.yaml, so a scheduler restart brings
  // the agent back — full removal is the copied `agent delete` command instead.
  async function stopAgent(name) {
    try {
      await apiDelete(`/admin/agents/${encodeURIComponent(name)}`);
      toast(`已下线 ${name}`);
      if (state.agent === name) state.agent = null;
      await loadAgents();
    } catch (e) {
      toast("下线失败：" + e.message);
    }
  }

  // ---- new agent (command generator) -------------------------------------
  // Creating an agent needs files written ON the scheduler host (scaffold
  // agent-card.yaml + append ahsir.yaml) — work the remote console can't do.
  // So instead of a half-capable form, the console generates the exact
  // `ahsir agent new` command for the operator to paste into the host shell.

  function readAgentForm() {
    return {
      name: $("#af-name").value.trim(),
      prompt: $("#af-prompt").value.trim(),
      provider: $("#af-provider").value,
      model: $("#af-model").value.trim(),
    };
  }
  function buildNewCmd(f) {
    if (!f.name) return "# 先填写 agent 名称";
    let cmd = `ahsir agent new ${shq(f.name)}`;
    if (f.prompt) cmd += ` \\\n  --prompt ${shq(f.prompt)}`;
    // deepseek is the CLI default; only emit --provider when it differs.
    if (f.provider && f.provider !== "deepseek") cmd += ` \\\n  --provider ${f.provider}`;
    if (f.model) cmd += ` \\\n  --model ${shq(f.model)}`;
    return cmd;
  }
  function newAgentForm() {
    if (state.roomPoll) { clearInterval(state.roomPoll); state.roomPoll = null; }
    state.mode = "agentnew";
    state.chatEmptyKind = null;
    state.roomId = null;
    state.contextId = null;
    $("#agentSel").disabled = true;
    setComposerWritable(false, "请使用上方表单配置 Agent");
    document.querySelectorAll("#contexts .sess, #rooms .sess").forEach((s) => s.classList.remove("on"));
    $("#ctxTitle").textContent = "新 Agent";
    $("#ctxId").textContent = "command";

    const providers = ["deepseek", "zhipu", "anthropic", "codex"];
    const provOpts = providers.map((p) => `<option value="${p}">${p}</option>`).join("");
    $("#thread").innerHTML =
      `<div class="room-form agent-form">
        <h3>新建 Agent</h3>
        <p class="hint">控制台是远程进程，不能在 scheduler 主机上写文件。下面只生成命令——把它复制到 <b>scheduler 所在主机</b> 的终端执行，即可 scaffold + 注册（写入 ahsir.yaml）+ 热启动一个新 agent。</p>
        <label>名称 *</label>
        <input type="text" id="af-name" placeholder="例如：security-reviewer">
        <label>系统提示（persona 指令）</label>
        <textarea id="af-prompt" placeholder="例如：你是一名资深安全评审员，专注于…"></textarea>
        <div class="row2">
          <div><label>Provider</label><select id="af-provider">${provOpts}</select></div>
          <div><label>Model（留空用默认）</label><input type="text" id="af-model" placeholder="deepseek-v4-pro"></div>
        </div>
        <div class="cmd-preview">
          <div class="lbl">生成的命令<button class="copy" id="af-copy">复制</button></div>
          <pre id="af-cmd"></pre>
        </div>
        <p class="hint">更多选项（--skill、--mcp-config、--allow-fs、--timeout 等）见 <code class="md-code">ahsir agent new --help</code>。执行成功后刷新本页即可看到新 agent。</p>
      </div>`;
    const refresh = () => { $("#af-cmd").textContent = buildNewCmd(readAgentForm()); };
    ["af-name", "af-prompt", "af-model"].forEach((id) => $("#" + id).addEventListener("input", refresh));
    $("#af-provider").addEventListener("change", refresh);
    $("#af-copy").addEventListener("click", (e) => copyText($("#af-cmd").textContent, null, e.currentTarget));
    refresh();
    $("#af-name").focus();
  }

  // ---- context list (left rail) ------------------------------------------

  async function loadContexts() {
    let rows;
    try {
      rows = await getJSON("/contexts");
    } catch (e) {
      $("#contexts").innerHTML = `<div class="muted-line">${esc(e.message)}</div>`;
      return;
    }
    const box = $("#contexts");
    box.innerHTML = "";
    box.appendChild(el("div", "grp", `会话<span class="c">${rows.length}</span>`));
    if (!rows.length) {
      box.appendChild(el("div", "muted-line", "还没有对话。点「新对话」开始。"));
      return;
    }
    rows.forEach((c) => {
      const row = el("div", "sess" + (c.contextId === state.contextId ? " on" : ""));
      const dot = c.lastStatus === "failed" ? "s-wait" : "s-ok";
      row.innerHTML =
        `<span class="dot ${dot}"></span>` +
        `<span class="t">${esc(c.title || c.contextId)}</span>` +
        `<span class="meta">${esc(timeAgo(c.lastActivity))}</span>`;
      row.title = `${c.contextId}\n参与: ${(c.agents || []).join(", ")}`;
      row.addEventListener("click", () => openContext(c));
      box.appendChild(row);
    });
  }

  // ---- archived (offline) agents ------------------------------------------

  // Offline agents whose managed workspace still holds transcripts on disk
  // (deleted / stopped agents). Read-only: clicking a context replays it via the
  // same /agents/{name}/history/{contextId} path a live agent uses — the
  // scheduler falls back to the on-disk transcript when the agent isn't running.
  async function loadArchived() {
    const box = $("#archived");
    if (!box) return;
    let agents;
    try {
      agents = await getJSON("/archived-agents");
    } catch (_) {
      // Older scheduler without the endpoint, or transient error — just hide.
      box.innerHTML = "";
      return;
    }
    state.archivedAgents = agents || [];
    box.innerHTML = "";
    if (!agents || !agents.length) return;
    box.appendChild(el("div", "grp", `归档<span class="c">${agents.length}</span>`));
    agents.forEach((a) => {
      box.appendChild(el("div", "arch-agent mono", esc(a.name)));
      (a.contexts || []).forEach((c) => {
        const row = el(
          "div",
          "sess arch" +
            (state.agent === a.name && state.contextId === c.contextId ? " on" : "")
        );
        const dot = c.lastStatus === "failed" ? "s-wait" : "s-idle";
        row.innerHTML =
          `<span class="dot ${dot}"></span>` +
          `<span class="t">${esc(c.title || c.contextId)}</span>` +
          `<span class="meta">${esc(timeAgo(c.lastActivity))}</span>`;
        row.title = `${a.name} · ${c.contextId}\n${c.turns} 轮 · 已归档（只读）`;
        row.addEventListener("click", () => openArchivedContext(a.name, c));
        box.appendChild(row);
      });
    });
    if (state.agent && !agentByName(state.agent)) renderDetail();
  }

  // Minimal read-only detail for an agent that's no longer registered, so the
  // right rail doesn't fall back to the "选择一个 agent" placeholder.
  function renderArchivedDetail(name) {
    $("#detailName").textContent = name;
    const card = $("#detailCard");
    if (!card) return;
    card.innerHTML =
      `<div class="ch"><span class="av" style="background:${avatarColor(name)}">${esc(name.slice(0, 2))}</span>` +
      `<div class="t">${esc(name)}<small class="mono">已归档 · 只读</small></div></div>` +
      `<div class="da-note">该 agent 已下线/删除，工作区仍保留历史。此处为只读回放，不能再发消息。</div>`;
  }

  async function openArchivedContext(agentName, c) {
    enterChatMode();
    state.chatEmptyKind = null;
    state.agent = agentName;
    state.contextId = c.contextId;
    $("#agentSel").value = "";
    setComposerWritable(false, "归档会话只读");
    $("#ctxTitle").textContent = c.title || c.contextId;
    $("#ctxId").textContent = "归档 · " + agentName + " · " + short(c.contextId);
    renderArchivedDetail(agentName);
    try {
      const turns = await getJSON(
        `/agents/${encodeURIComponent(agentName)}/history/${encodeURIComponent(c.contextId)}`
      );
      renderThread(turns);
    } catch (e) {
      renderThread([]);
      toast("无法读取归档记录：" + e.message);
    }
    $("#trace").innerHTML = '<div class="muted-line">归档会话 · 无轨迹</div>';
    document.querySelectorAll("#contexts .sess, #archived .sess").forEach((s) => s.classList.remove("on"));
    loadArchived();
  }

  // ---- thread (center) ----------------------------------------------------

  function renderThread(turns) {
    const t = $("#thread");
    t.classList.remove("has-chat-empty");
    t.innerHTML = "";
    if (!turns || !turns.length) {
      t.appendChild(el("div", "thread-empty",
        state.agent ? `与 ${esc(state.agent)} 还没有对话` : "选择一个 agent，开始对话"));
      return;
    }
    const wrap = el("div", "tw");
    turns.forEach((turn) => {
      // The transcript stores one ts (turn completion) + duration; the question
      // landed ~duration earlier than the reply, so offset the user clock.
      const endMs = turn.ts ? new Date(turn.ts).getTime() : null;
      const askMs = endMs != null ? endMs - (turn.durationMs || 0) : null;

      // User message: who asked + when. (1:1 chat has a single fixed agent, so
      // no 直接回复 here, only 复制.)
      const ub = msgBlock("u");
      ub.appendChild(el("div", null, msgHead("u", turn.speaker || "user", askMs, turn.ts)));
      ub.appendChild(el("div", "umsg", `<div class="b">${mdToHtml(turn.userText)}</div>`));
      ub.appendChild(msgActions(turn.userText, "u"));
      wrap.appendChild(ub);

      // Agent reply: which agent + when, content rendered as markdown.
      const ab = msgBlock("a");
      if (endMs != null) ab.dataset.ts = endMs; // 轨迹 node → bubble jump key
      ab.dataset.agent = state.agent || "";
      ab.appendChild(el("div", null, msgHead("a", state.agent || "agent", endMs, turn.ts)));
      if (turn.status === "failed") {
        ab.appendChild(el("div", "amsg failed", `<p>✗ ${esc(turn.error || "失败")}</p>`));
        ab.appendChild(msgActions(turn.error || "失败", "a"));
      } else {
        ab.appendChild(el("div", "amsg", mdToHtml(turn.reply)));
        ab.appendChild(msgActions(turn.reply, "a"));
      }
      wrap.appendChild(ab);
    });
    t.appendChild(wrap);
    t.scrollTop = t.scrollHeight;
  }

  // Append a provisional user bubble + a pending agent placeholder while a
  // turn is in flight; returns the placeholder element to fill in on reply.
  function appendPending(message) {
    let wrap = $("#thread .tw");
    if (!wrap) { $("#thread").innerHTML = ""; wrap = el("div", "tw"); $("#thread").appendChild(wrap); }
    const now = Date.now();
    const ub = msgBlock("u");
    ub.appendChild(el("div", null, msgHead("u", state.speaker, now)));
    ub.appendChild(el("div", "umsg", `<div class="b">${mdToHtml(message)}</div>`));
    ub.appendChild(msgActions(message, "u"));
    wrap.appendChild(ub);
    const ab = msgBlock("a");
    ab.appendChild(el("div", null, msgHead("a", state.agent || "agent", now)));
    const pending = el("div", "amsg");
    pending.appendChild(el("div", "elapsed mono", `<span class="dot s-run"></span>${esc(state.agent)} 处理中…`));
    ab.appendChild(pending);
    wrap.appendChild(ab);
    $("#thread").scrollTop = $("#thread").scrollHeight;
    return pending;
  }

  async function openContext(c) {
    closeLiveSource();
    enterChatMode();
    state.chatEmptyKind = null;
    state.contextId = c.contextId;
    state.contextSummary = c;
    state.liveEvents = [];
    state.contextTurns = [];
    state.historyError = null;
    $("#ctxTitle").textContent = c.title || c.contextId;
    $("#ctxId").textContent = "ctx · " + short(c.contextId);
    // Prefer an agent that actually participated in this context.
    if (c.agents && c.agents.length && !c.agents.includes(state.agent)) {
      state.agent = c.agents[0];
    }
    $("#agentSel").value = agentByName(state.agent) ? state.agent : "";
    renderDetail();
    renderChatAvailability();
    await refreshContextViews();
    markActiveContext();
  }

  function markActiveContext() {
    document.querySelectorAll("#contexts .sess").forEach((s) => s.classList.remove("on"));
    // re-render is cheap; just reload to repaint the active marker
    loadContexts();
  }

  function renderChatEmpty(kind, feedbackText) {
    state.chatEmptyKind = kind;
    const schedulerError = kind === "scheduler-error";
    const t = $("#thread");
    t.innerHTML = "";
    t.classList.add("has-chat-empty");

    const wrap = el("section", "chat-empty-state");
    wrap.setAttribute("aria-labelledby", "chatEmptyTitle");
    const mark = el("div", "chat-empty-mark", '<span class="dot s-idle"></span>');
    mark.setAttribute("aria-hidden", "true");
    wrap.appendChild(mark);
    const title = el("h2", null, schedulerError ? "无法连接 scheduler" : "当前没有可用 Agent");
    title.setAttribute("id", "chatEmptyTitle");
    wrap.appendChild(title);
    const description = el("p", null, schedulerError
      ? "控制台暂时无法读取 Agent 状态。请确认 scheduler 正在运行，然后重新连接。"
      : "新对话需要一个正在运行的 Agent。你可以先配置一个，或在启动后重新检查。");
    description.setAttribute("id", "chatEmptyDescription");
    wrap.appendChild(description);

    const actions = el("div", "chat-empty-actions");
    if (!schedulerError) {
      const configure = el("button", "primary", "配置新 Agent");
      configure.setAttribute("id", "emptyConfigureAgent");
      configure.addEventListener("click", newAgentForm);
      actions.appendChild(configure);
    }
    const retry = el("button", schedulerError ? "primary" : "secondary",
      schedulerError ? "重新连接" : "重新检查");
    retry.setAttribute("id", "emptyRetryAgents");
    retry.addEventListener("click", () => retryAgents(retry));
    actions.appendChild(retry);
    wrap.appendChild(actions);

    const feedback = el("div", "chat-empty-feedback", feedbackText || "");
    feedback.setAttribute("id", "chatEmptyFeedback");
    feedback.setAttribute("aria-live", "polite");
    wrap.appendChild(feedback);
    t.appendChild(wrap);
  }

  async function retryAgents(button) {
    const oldLabel = button.textContent;
    button.disabled = true;
    button.textContent = state.chatEmptyKind === "scheduler-error" ? "连接中…" : "检查中…";
    const ok = await loadAgents();
    if (!ok) {
      renderChatEmpty("scheduler-error");
      renderChatAvailability();
      return;
    }
    if (state.agents.length) {
      const found = state.agents[0].name;
      state.agent = found;
      toast(`已发现 ${found}`);
      newConversation();
      return;
    }
    button.disabled = false;
    button.textContent = oldLabel;
    const feedback = $("#chatEmptyFeedback");
    if (feedback) feedback.textContent = "仍未发现运行中的 Agent";
  }

  function newConversation() {
    closeLiveSource();
    enterChatMode();
    state.contextSummary = null;
    state.liveEvents = [];
    state.contextTurns = [];
    state.historyError = null;
    $("#ctxTitle").textContent = "新对话";

    if (state.schedulerState !== "ready") {
      state.contextId = null;
      $("#ctxId").textContent = "等待连接";
      renderChatEmpty("scheduler-error");
      renderChatAvailability();
      showMobileSurface("center");
      return;
    }
    if (!state.agents.length) {
      state.agent = null;
      state.contextId = null;
      $("#agentSel").value = "";
      $("#ctxId").textContent = "等待 Agent";
      renderDetail();
      renderChatEmpty("no-agent");
      renderChatAvailability();
      showMobileSurface("center");
      return;
    }

    state.chatEmptyKind = null;
    if (!agentByName(state.agent)) state.agent = state.agents[0].name;
    state.contextId = uuid();
    $("#agentSel").value = state.agent;
    $("#ctxId").textContent = "ctx · " + short(state.contextId);
    renderThread([]);
    $("#trace").innerHTML = '<div class="muted-line">-</div>';
    document.querySelectorAll("#contexts .sess").forEach((s) => s.classList.remove("on"));
    renderDetail();
    renderChatAvailability();
    showMobileSurface("center");
    $("#ta").focus();
  }

  // Reload the selected agent's transcript for the current context + the trace.
  async function refreshContextViews() {
    if (!state.contextId || !state.agent) { renderThread([]); return; }
    const contextID = state.contextId;
    const historyPath = `/agents/${encodeURIComponent(state.agent)}/history/${encodeURIComponent(contextID)}`;
    const eventPath = `/context-events?contextId=${encodeURIComponent(contextID)}`;
    const [history, live] = await Promise.allSettled([getJSON(historyPath), getJSON(eventPath)]);
    if (state.contextId !== contextID) return;
    state.contextTurns = history.status === "fulfilled" ? (history.value || []) : [];
    state.historyError = history.status === "rejected" ? history.reason.message : null;
    state.liveEvents = live.status === "fulfilled" ? (live.value || []) : [];
    renderCurrentContext();
    if (isActiveInvocation(state.contextSummary)) startLiveSource();
    else closeLiveSource();
    loadTrace();
    loadContextAgents();
  }

  function isActiveInvocation(summary) {
    return !!summary && ["queued", "in_flight", "recovering"].includes(summary.lastStatus);
  }

  function closeLiveSource() {
    if (state.liveSource) {
      state.liveSource.close();
      state.liveSource = null;
    }
  }

  function renderCurrentContext() {
    renderThread(state.contextTurns || []);
    const summary = state.contextSummary;
    const terminalWithoutTranscript = summary && !isActiveInvocation(summary) &&
      (!state.contextTurns || !state.contextTurns.length) && summary.userText;
    if (isActiveInvocation(summary) || terminalWithoutTranscript) {
      appendLiveTurn(summary, state.liveEvents || []);
    }
    if (state.historyError) appendHistoryError(state.historyError);
  }

  function ensureThreadWrap() {
    let wrap = $("#thread .tw");
    if (!wrap) {
      $("#thread").innerHTML = "";
      wrap = el("div", "tw");
      $("#thread").appendChild(wrap);
    }
    return wrap;
  }

  function appendLiveTurn(summary, events) {
    if (!summary) return;
    const wrap = ensureThreadWrap();
    const startMs = summary.startedAt ? new Date(summary.startedAt).getTime() : Date.now();
    const ub = msgBlock("u");
    ub.appendChild(el("div", null, msgHead("u", summary.speaker || "user", startMs, summary.startedAt)));
    ub.appendChild(el("div", "umsg", `<div class="b">${mdToHtml(summary.userText || summary.title || "")}</div>`));
    wrap.appendChild(ub);

    const ab = msgBlock("a");
    ab.appendChild(el("div", null, msgHead("a", state.agent || "agent", Date.now())));
    const body = el("div", "amsg live-progress");
    const status = liveStatusLabel(summary.lastStatus);
    body.appendChild(el("div", "elapsed mono", `<span class="dot ${isActiveInvocation(summary) ? "s-run" : "s-wait"}"></span>${esc(state.agent)} · ${esc(status)}`));
    events.forEach((ev) => body.appendChild(renderLiveStep(ev)));
    if (summary.error) body.appendChild(el("div", "live-error", `✗ ${esc(summary.error)}`));
    ab.appendChild(body);
    wrap.appendChild(ab);
    $("#thread").scrollTop = $("#thread").scrollHeight;
  }

  function liveStatusLabel(status) {
    return ({ queued: "排队中", in_flight: "执行中", recovering: "恢复中", completed: "已完成",
      recovered: "已恢复", failed: "执行失败", recovery_failed: "恢复失败", canceled: "已取消" })[status] || status || "执行中";
  }

  function renderLiveStep(ev) {
    if (ev.type === "thinking") return el("div", "live-step thinking", "Agent 正在思考");
    if (ev.type === "status" || ev.type === "span_start" || ev.type === "span_end") {
      return el("div", "live-step muted-line", esc(ev.state || ev.type));
    }
    if (ev.type === "text_delta") return el("div", "live-step live-text", mdToHtml(ev.content || ""));
    const label = ev.name || ev.type || "step";
    let detail = "";
    if (ev.input != null) {
      try { detail = JSON.stringify(ev.input, null, 2); } catch (_) { detail = String(ev.input); }
    } else if (ev.content) detail = ev.content;
    return el("details", "live-step" + (ev.isError ? " failed" : ""),
      `<summary><span class="dot ${ev.isError ? "s-wait" : "s-ok"}"></span><code>${esc(label)}</code></summary>` +
      (detail ? `<pre>${esc(detail)}</pre>` : ""));
  }

  function appendHistoryError(message) {
    const wrap = ensureThreadWrap();
    wrap.appendChild(el("div", "history-error", `<b>无法读取会话记录</b><span>${esc(message)}</span>`));
  }

  function startLiveSource() {
    closeLiveSource();
    if (typeof EventSource === "undefined" || !state.contextId) return;
    const last = state.liveEvents.length ? state.liveEvents[state.liveEvents.length - 1].id : "";
    const path = `/context-events/stream?contextId=${encodeURIComponent(state.contextId)}` +
      (last ? `&after=${encodeURIComponent(last)}` : "");
    const source = new EventSource(api(path));
    state.liveSource = source;
    const types = ["status", "text_delta", "tool_use", "tool_result", "thinking", "span_start", "span_end", "terminal"];
    types.forEach((type) => source.addEventListener(type, (e) => {
      if (source !== state.liveSource) return;
      let ev;
      try { ev = JSON.parse(e.data); } catch (_) { return; }
      if (state.liveEvents.some((x) => x.id === ev.id)) return;
      state.liveEvents.push(ev);
      if (type === "terminal") {
        if (state.contextSummary) state.contextSummary.lastStatus = ev.state || "completed";
        closeLiveSource();
        setTimeout(() => refreshContextViews(), 50);
        return;
      }
      renderCurrentContext();
    }));
  }

  async function loadContextAgents() {
    const rows = $("#agentRows");
    rows.innerHTML = "";
    let agents = [];
    try {
      const list = await getJSON("/contexts");
      const c = list.find((x) => x.contextId === state.contextId);
      agents = (c && c.agents) || [];
    } catch (_) {}
    $("#agentCount").textContent = agents.length;
    agents.forEach((name) => {
      const a = agentByName(name) || archivedAgentByName(name) || { name, status: "unavailable" };
      const row = el("div", "agent-row");
      row.innerHTML =
        `<span class="av" style="background:${avatarColor(name)}">${esc(name.slice(0, 2))}</span>` +
        `<div class="nm"><div class="a">${esc(name)}</div><div class="b mono">${esc(a.url || "")}</div></div>` +
        `<span class="pill"><span class="dot ${statusDot(a.status)}"></span>${esc(a.status || "")}</span>`;
      row.addEventListener("click", () => selectAgent(name));
      setDragAgent(row, name);
      rows.appendChild(row);
    });
  }

  async function loadTrace(ctxId, perRound) {
    ctxId = ctxId || state.contextId;
    if (!ctxId) return;
    let recs;
    try {
      recs = await getJSON(`/invocations?contextId=${encodeURIComponent(ctxId)}`);
    } catch (e) { return; }
    const tl = $("#trace");
    tl.innerHTML = "";
    if (!recs || !recs.length) { tl.innerHTML = '<div class="muted-line">-</div>'; return; }
    // Roundtable: the room's trace is N participant invocations per round (the
    // moderator runs under a separate contextId). Sort chronologically and drop
    // a "第 X 轮" divider every N records.
    if (perRound > 0) {
      recs = recs.slice().sort((a, b) => (a.startedAt || "").localeCompare(b.startedAt || ""));
    }
    recs.forEach((r, i) => {
      if (perRound > 0 && i % perRound === 0) {
        tl.appendChild(el("div", "trace-round", `第 ${Math.floor(i / perRound) + 1} 轮`));
      }
      const ok = r.status === "completed" ? "ok" : (r.status === "failed" ? "" : "run");
      const dur = r.durationMs ? `· ${r.durationMs}ms` : "";
      const ev = el("div", "ev clk " + ok);
      ev.innerHTML =
        `<div class="h"><span class="a">${esc(r.agentName)}</span>` +
        `<span class="ts mono">${esc(r.speaker || r.source || "")}</span></div>` +
        `<div class="b mono"><span class="tg">${esc(r.status)}</span> ${esc(dur)} ${esc((r.userText || "").slice(0, 60))}</div>`;
      // Click a node to jump the center thread to its bubble. Match on the
      // turn's completion ts (startedAt + duration) so it works in both chat and
      // roundtable regardless of record ordering.
      const endTs = r.startedAt ? new Date(r.startedAt).getTime() + (r.durationMs || 0) : NaN;
      ev.title = "跳到对应气泡";
      ev.addEventListener("click", () => scrollThreadToTrace(r.agentName, endTs, i));
      tl.appendChild(ev);
    });
  }

  // Scroll the center thread to the agent bubble matching a 轨迹 node. Prefer the
  // bubble whose completion ts is closest (order-independent, same-agent first);
  // fall back to the i-th agent bubble when ts is missing. Flashes the target.
  function scrollThreadToTrace(agent, ts, idx) {
    const thread = $("#thread");
    if (!thread) return;
    const blocks = Array.from(thread.querySelectorAll(".msg.a[data-ts]"));
    if (!blocks.length) return;
    let best = null;
    if (!isNaN(ts)) {
      let bestDiff = Infinity;
      const same = blocks.filter((b) => !agent || !b.dataset.agent || b.dataset.agent === agent);
      (same.length ? same : blocks).forEach((b) => {
        const d = Math.abs(Number(b.dataset.ts) - ts);
        if (d < bestDiff) { bestDiff = d; best = b; }
      });
    }
    if (!best) best = blocks[Math.min(idx, blocks.length - 1)];
    if (!best) return;
    best.scrollIntoView({ behavior: "smooth", block: "start" });
    best.classList.remove("flash");
    void best.offsetWidth; // restart the flash animation if re-clicked
    best.classList.add("flash");
    setTimeout(() => { if (best) best.classList.remove("flash"); }, 1200);
  }

  function selectAgent(name) {
    state.agent = name;
    $("#agentSel").value = agentByName(name) ? name : "";
    renderDetail();
    renderChatAvailability();
    refreshContextViews();
  }

  // ---- sending ------------------------------------------------------------

  let sending = false;
  async function send() {
    const ta = $("#ta");
    const message = ta.value.trim();
    if (!message || sending) return;
    if (state.mode === "agentnew") { toast("把上面的命令复制到 scheduler 主机的终端执行即可创建 agent"); return; }
    if (state.mode === "room") { ta.value = ""; autosize(); return sayInRoom(message); }
    if (!agentByName(state.agent)) { toast("该参与者不可发送消息"); return; }
    sending = true;
    ta.value = "";
    autosize();

    // Always own the contextId (see uuid() for why). First turn of a fresh
    // conversation also titles it from the message.
    const firstTurn = !$("#thread .amsg");
    if (!state.contextId) state.contextId = uuid();
    if (firstTurn) $("#ctxTitle").textContent = message;
    $("#ctxId").textContent = "ctx · " + short(state.contextId);

    const pending = appendPending(message);
    try {
      const body = { message, async: true, speaker: state.speaker, contextId: state.contextId };
      const sub = await postJSON(`/agents/${encodeURIComponent(state.agent)}/chat`, body);
      const reply = await pollTask(state.agent, sub.taskId);
      pending.className = "amsg";
      pending.innerHTML = mdToHtml(reply);
      pending.after(msgActions(reply, "a"));
    } catch (e) {
      pending.className = "amsg failed";
      pending.innerHTML = `<p>✗ ${esc(e.message)}</p>`;
      pending.after(msgActions(e.message, "a"));
    } finally {
      sending = false;
      $("#thread").scrollTop = $("#thread").scrollHeight;
      loadContexts();
      loadTrace();
      loadContextAgents();
    }
  }

  // Poll a task to a terminal state and return the agent's reply text.
  async function pollTask(agent, taskId) {
    let delay = 600;
    for (;;) {
      await new Promise((r) => setTimeout(r, delay));
      delay = Math.min(delay * 1.4, 4000);
      let task;
      try {
        task = await getJSON(`/agents/${encodeURIComponent(agent)}/tasks/${encodeURIComponent(taskId)}`);
      } catch (e) {
        throw new Error("任务丢失（agent 可能重启）：" + e.message);
      }
      const st = task.status && task.status.state;
      if (st === "completed") return lastAgentText(task);
      if (st === "failed") throw new Error(taskText(task.status && task.status.message) || "任务失败");
      if (st === "canceled") throw new Error("任务已取消");
    }
  }
  function lastAgentText(task) {
    const h = task.history || [];
    for (let i = h.length - 1; i >= 0; i--) {
      if (h[i].role === "agent") { const t = taskText(h[i]); if (t) return t; }
    }
    return "";
  }
  function taskText(msg) {
    if (!msg || !msg.parts) return "";
    return msg.parts.filter((p) => p.kind === "text").map((p) => p.text).join("");
  }

  // ---- roundtable (multi-agent group chat) --------------------------------

  // Leaving room mode: stop polling and fall back to 1:1 chat.
  function enterChatMode() {
    closeLiveSource();
    state.mode = "chat";
    state.roomId = null;
    state.roomRendered = null;
    if (state.roomPoll) { clearInterval(state.roomPoll); state.roomPoll = null; }
    document.querySelectorAll("#rooms .sess").forEach((s) => s.classList.remove("on"));
    $("#agentSel").disabled = false;
  }

  // Render the room creation form into the thread area.
  function newRoom() {
    enterChatMode();
    state.mode = "room";
    state.roomId = null;
    if (state.roomPoll) { clearInterval(state.roomPoll); state.roomPoll = null; }
    $("#ctxTitle").textContent = "新多 Agent 协同";
    $("#ctxId").textContent = "relay";
    $("#agentSel").disabled = true;
    document.querySelectorAll("#contexts .sess, #rooms .sess").forEach((s) => s.classList.remove("on"));

    const agents = state.agents.map((a) => a.name);
    const chips = agents.map((n) => `<span class="pchip" data-p="${esc(n)}">${esc(n)}</span>`).join("");
    const orgOpts = ['<option value="operator">我（operator）</option>']
      .concat(agents.map((n) => `<option value="${esc(n)}">${esc(n)}（agent 主持）</option>`)).join("");
    const t = $("#thread");
    t.innerHTML =
      `<div class="room-form">
        <h3>发起多 Agent 协同</h3>
        <p class="hint">@ 点名驱动：消息里第一个 @某位参与者 成为下一个发言者，agent 之间可互相 @ 接力。无人被 @ 时交回组织者。</p>
        <label>参与者（多选）</label>
        <div class="roster" id="rf-roster">${chips || '<span class="muted-line">没有可用 agent</span>'}</div>
        <label>组织者：无人被 @ 时由谁继续</label>
        <select id="rf-org">${orgOpts}</select>
        <label>话题</label>
        <input type="text" id="rf-topic" maxlength="60" placeholder="例如：评审这个调度器设计（最长 60 字）">
        <label>开场白（可 @某位参与者 让他先发言）</label>
        <textarea id="rf-open" placeholder="例如：@teacher 先抛个观点，然后 @student 回应"></textarea>
        <button class="go" id="rf-go" disabled>开始</button>
      </div>`;
    const picked = new Set();
    const sync = () => { $("#rf-go").disabled = picked.size === 0; };
    t.querySelectorAll(".pchip").forEach((c) => c.addEventListener("click", () => {
      const n = c.dataset.p;
      if (picked.has(n)) { picked.delete(n); c.classList.remove("on"); }
      else { picked.add(n); c.classList.add("on"); }
      sync();
    }));
    $("#rf-go").addEventListener("click", () => createRoom([...picked]));
  }

  async function createRoom(participants) {
    const body = {
      topic: $("#rf-topic").value.trim(),
      participants,
      organizer: $("#rf-org").value,
      message: $("#rf-open").value.trim(),
    };
    try {
      const view = await postJSON("/rooms", body);
      enterRoom(view);
      loadRooms();
    } catch (e) {
      toast("发起失败：" + e.message);
    }
  }

  // The real round-table: consensus rounds. Every round asks all participants
  // (random order) — say something or reply 同意 — until a round is all-agree.
  // A dedicated moderator agent judges consensus each round and writes the
  // summary; it must not also be a participant.
  function newRoundtable() {
    enterChatMode();
    state.mode = "room";
    state.roomId = null;
    if (state.roomPoll) { clearInterval(state.roomPoll); state.roomPoll = null; }
    $("#ctxTitle").textContent = "新圆桌";
    $("#ctxId").textContent = "roundtable";
    $("#agentSel").disabled = true;
    document.querySelectorAll("#contexts .sess, #rooms .sess").forEach((s) => s.classList.remove("on"));

    const agents = state.agents.map((a) => a.name);
    const chips = agents.map((n) => `<span class="pchip" data-p="${esc(n)}">${esc(n)}</span>`).join("");
    const t = $("#thread");
    t.innerHTML =
      `<div class="room-form">
        <h3>发起圆桌（共识讨论）</h3>
        <p class="hint">每轮以随机顺序问遍所有参与者：有异议就说，无异议回『同意』。某一轮全员同意即达成共识、收敛。一个主持 agent 每轮判定共识并最终总结。约定由每轮的 prompt 注入，无需改 agent 的 system prompt。</p>
        <label>参与者（多选）</label>
        <div class="roster" id="rt-roster">${chips || '<span class="muted-line">没有可用 agent</span>'}</div>
        <label>主持 / 总结 agent（判共识 + 出结论；不能是参与者）</label>
        <select id="rt-mod"></select>
        <label>话题</label>
        <input type="text" id="rt-topic" maxlength="60" placeholder="例如：评审 mindpowers OKR">
        <label>最多轮数（留空 = 12）</label>
        <input type="text" id="rt-budget" placeholder="12">
        <label>开场问题（operator）</label>
        <textarea id="rt-open" placeholder="例如：请评审这份 OKR，逐条给出异议或回『同意』"></textarea>
        <button class="go" id="rt-go" disabled>开始圆桌</button>
      </div>`;
    const picked = new Set();
    const syncMod = () => {
      const opts = agents.filter((n) => !picked.has(n));
      $("#rt-mod").innerHTML = opts.length
        ? opts.map((n) => `<option value="${esc(n)}">${esc(n)}</option>`).join("")
        : '<option value="">先选参与者，剩下的可当主持</option>';
    };
    const sync = () => { $("#rt-go").disabled = picked.size === 0 || !$("#rt-mod").value; };
    t.querySelectorAll(".pchip").forEach((c) => c.addEventListener("click", () => {
      const n = c.dataset.p;
      if (picked.has(n)) { picked.delete(n); c.classList.remove("on"); }
      else { picked.add(n); c.classList.add("on"); }
      syncMod(); sync();
    }));
    $("#rt-mod").addEventListener("change", sync);
    syncMod(); sync();
    $("#rt-go").addEventListener("click", () => createRoundtable([...picked]));
  }

  async function createRoundtable(participants) {
    const budget = parseInt($("#rt-budget").value.trim(), 10);
    const body = {
      mode: "roundtable",
      topic: $("#rt-topic").value.trim(),
      participants,
      moderator: $("#rt-mod").value,
      budget: Number.isFinite(budget) ? budget : 0,
      message: $("#rt-open").value.trim(),
    };
    if (!body.moderator) { toast("请选择一个主持 agent"); return; }
    try {
      const view = await postJSON("/rooms", body);
      enterRoom(view);
      loadRooms();
    } catch (e) {
      toast("发起失败：" + e.message);
    }
  }

  function enterRoom(view) {
    state.mode = "room";
    state.roomId = view.id;
    $("#agentSel").disabled = true;
    renderRoom(view);
    if (state.roomPoll) clearInterval(state.roomPoll);
    state.roomPoll = setInterval(() => refreshRoom(), 2000);
  }

  async function openRoom(id) {
    enterChatMode();
    state.mode = "room";
    state.roomId = id;
    try { renderRoom(await getJSON("/rooms/" + encodeURIComponent(id))); } catch (e) { toast(e.message); }
    if (state.roomPoll) clearInterval(state.roomPoll);
    state.roomPoll = setInterval(() => refreshRoom(), 2000);
    loadRooms();
  }

  async function refreshRoom() {
    if (state.mode !== "room" || !state.roomId) return;
    try { renderRoom(await getJSON("/rooms/" + encodeURIComponent(state.roomId))); } catch (_) {}
  }

  // A compact fingerprint of everything renderRoom paints, so the 2s poll can
  // skip rebuilding innerHTML when nothing changed (a rebuild wipes the user's
  // text selection — making the transcript impossible to select/copy).
  function roomSignature(view) {
    const turns = (view.transcript || [])
      .map((tr) => `${tr.speaker}${tr.text || ""}${tr.error || ""}`).join("");
    return [view.status, view.next || "", view.speaking || "",
      (view.participants || []).join(","), view.organizer || "", turns].join("");
  }
  // True when the user has a live (non-collapsed) text selection in the thread.
  function selectionInThread() {
    const sel = window.getSelection && window.getSelection();
    if (!sel || sel.isCollapsed || sel.rangeCount === 0) return false;
    const thread = document.getElementById("thread");
    return !!(thread && sel.anchorNode && thread.contains(sel.anchorNode));
  }

  // ---- "@me" notifications (sound + popup + flashing tab) -----------------

  function mentionsOperator(text) {
    return /(^|[^\w@])@operator(?![\w-])/i.test(text || "");
  }

  let audioCtx = null;
  function ensureAudio() {
    if (!audioCtx) { try { audioCtx = new (window.AudioContext || window.webkitAudioContext)(); } catch (_) {} }
    if (audioCtx && audioCtx.state === "suspended") audioCtx.resume();
  }
  function beep() {
    ensureAudio();
    if (!audioCtx) return;
    try {
      const o = audioCtx.createOscillator(), g = audioCtx.createGain();
      o.type = "sine"; o.frequency.value = 880;
      o.connect(g); g.connect(audioCtx.destination);
      const t = audioCtx.currentTime;
      g.gain.setValueAtTime(0.0001, t);
      g.gain.exponentialRampToValueAtTime(0.22, t + 0.01);
      g.gain.exponentialRampToValueAtTime(0.0001, t + 0.4);
      o.start(t); o.stop(t + 0.42);
    } catch (_) {}
  }

  const FAV_NORMAL = "data:image/svg+xml," + encodeURIComponent(
    '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32"><rect width="32" height="32" rx="7" fill="#2b2b30"/><text x="16" y="22" font-family="monospace" font-size="15" fill="#cfcfd2" text-anchor="middle">ah</text></svg>');
  const FAV_PING = "data:image/svg+xml," + encodeURIComponent(
    '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32"><rect width="32" height="32" rx="7" fill="#1d5fbf"/><text x="16" y="23" font-family="monospace" font-size="20" fill="#fff" text-anchor="middle">@</text></svg>');
  function setFavicon(ping) {
    let l = document.getElementById("favicon");
    if (!l) { l = document.createElement("link"); l.id = "favicon"; l.rel = "icon"; document.head.appendChild(l); }
    l.href = ping ? FAV_PING : FAV_NORMAL;
  }

  let flashTimer = null, baseTitle = "ahsir";
  function startFlash(label) {
    if (flashTimer) return;
    let on = false;
    flashTimer = setInterval(() => {
      on = !on;
      document.title = on ? "🔔 " + label : baseTitle;
      setFavicon(on);
    }, 850);
  }
  function stopFlash() {
    if (flashTimer) { clearInterval(flashTimer); flashTimer = null; }
    document.title = baseTitle;
    setFavicon(false);
  }

  // Are we actively looking at this page right now? (tab visible AND window focused)
  function pageActive() {
    return !document.hidden && (document.hasFocus ? document.hasFocus() : true);
  }

  function pingOperator(speaker, text) {
    // Always show the in-page toast — it's a momentary cue, fine even when watching.
    toast(`🔔 ${speaker} @了你`);
    // If we're already on the page there's nothing to grab attention back to:
    // no beep, no OS popup, no blinking logo. The toast already covered it.
    if (pageActive()) return;
    beep();
    if (("Notification" in window) && Notification.permission === "granted") {
      try { new Notification(`${speaker} @了你`, { body: (text || "").slice(0, 140), tag: "ahsir-ping", renotify: true }); } catch (_) {}
    }
    // We're away — blink title + favicon until we come back (focus / visibilitychange stops it).
    startFlash(`${speaker} @了你`);
  }

  // Scan a room view for NEW agent "@operator" pings since we last checked, and
  // alert once. On first sight of a room, mark its history seen (no flood).
  function checkRoomPings(view) {
    const turns = view.transcript || [];
    if (state.pingSeenId !== view.id) {
      state.pingSeenId = view.id;
      state.pingSeenCount = turns.length;
      return;
    }
    for (let i = state.pingSeenCount; i < turns.length; i++) {
      const t = turns[i];
      if (t.speaker !== "operator" && !t.error && mentionsOperator(t.text)) { pingOperator(t.speaker, t.text); break; }
    }
    state.pingSeenCount = turns.length;
  }

  function renderRoom(view) {
    if (!view || state.roomId !== view.id) return;
    checkRoomPings(view); // before the render-skip, so pings aren't missed
    const firstRender = state.roomRendered !== view.id;
    // Skip this poll's rebuild when it changes nothing, or while the user is
    // selecting text in the thread — a rebuild would only wipe their selection.
    const sig = roomSignature(view);
    if (!firstRender && (selectionInThread() || sig === state.roomSig)) return;
    state.roomSig = sig;
    $("#ctxTitle").textContent = view.topic || "圆桌";
    $("#ctxId").textContent = "round-table · " + short(view.id);
    const t = $("#thread");
    // Preserve the reader's scroll position across the 2s poll re-render. Only
    // auto-stick to the bottom if they were already there (following the live
    // discussion); if they scrolled up to read history, keep them put instead
    // of yanking them back down every tick. The first render of a given room
    // (just opened) always starts at the bottom.
    state.roomRendered = view.id;
    const wasAtBottom = firstRender || t.scrollHeight - t.scrollTop - t.clientHeight < 80;
    const prevTop = t.scrollTop;
    const isRt = view.mode === "roundtable";
    const nextLine = view.next ? ` · 下一个：<b>${esc(view.next)}</b>` : "";
    const modePill = isRt
      ? `<span class="pill">圆桌 · 主持 ${esc(view.moderator || "?")}</span>`
      : `<span class="pill">协同 · 组织者 ${esc(view.organizer)}</span>`;
    const head =
      `<div class="rhdr">
        <span class="pill ${esc(view.status)}">${esc(roomStatusZh(view.status))}</span>
        ${modePill}
        <span class="pill">${view.participants.map(esc).join("、")}</span>
        <span class="sp"></span>
        ${view.status !== "stopped" ? '<button class="stop" id="room-stop">结束</button>' : ""}
        <span style="flex-basis:100%;font-size:11px;color:var(--muted)">${nextLine}</span>
      </div>`;
    t.innerHTML = head + `<div class="tw" id="room-thread"></div>`;
    renderRoomTranscript(view.transcript || [], view.participants || [], view.moderator);
    // Show a "thinking" bubble for whoever's turn is in flight. Key on
    // `speaking` (the agent whose LLM call is running) — `next` is cleared the
    // moment a turn starts, so it's empty during the long provider call. Fall
    // back to `next` for the brief scheduled-but-not-started window.
    const pendingWho = view.speaking || (view.status === "active" ? view.next : "");
    if (pendingWho) {
      const wrap = $("#room-thread");
      wrap.appendChild(el("div", null, msgHead("a", pendingWho, Date.now())));
      const p = el("div", "amsg");
      p.appendChild(el("div", "elapsed mono", `<span class="dot s-run"></span>${esc(pendingWho)} 思考中…`));
      wrap.appendChild(p);
    }
    const stop = $("#room-stop");
    if (stop) stop.addEventListener("click", stopRoom);
    // right rail: participant roster (详情) + invocation trace (轨迹). The room
    // id is the shared contextId, so the trace is keyed on it.
    renderRoomRoster(view);
    loadTrace(view.id, view.mode === "roundtable" ? (view.participants || []).length : 0);
    t.scrollTop = wasAtBottom ? t.scrollHeight : prevTop;
    updateJumpToBottom();
  }

  const ROOM_ME = "operator"; // the operator's identity inside a room

  // Highlight "@<name>" fragments in a rendered bubble. nameToClass maps each
  // lowercased name to a CSS class: "@operator" (= you) gets `mention-me`
  // (blue pill); other participants get `mention` (blue text, no pill) so a
  // peer ping is visible but not confused with one aimed at you. Walks text
  // nodes only (never tags/attributes), skips code/pre (a quoted token, not a
  // ping) and already-highlighted spans, and skips emails (a@name.io).
  function highlightMentions(root, nameToClass) {
    if (!root) return;
    const names = Object.keys(nameToClass);
    if (!names.length) return;
    names.sort((a, b) => b.length - a.length); // longer names win the alternation
    const alt = names.map((n) => n.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")).join("|");
    const re = new RegExp("@(" + alt + ")(?![\\w-])", "gi");
    const inSkippable = (node) => {
      for (let p = node.parentNode; p && p !== root; p = p.parentNode) {
        const t = p.nodeName;
        if (t === "CODE" || t === "PRE") return true;
        if (p.classList && (p.classList.contains("mention-me") || p.classList.contains("mention"))) return true;
      }
      return false;
    };
    // Inline-code mentions: an agent may wrap "@name" in backticks, which
    // markdown renders as a grey <code> pill — the text walk below then skips
    // it (code = a quoted token). But for a KNOWN participant it's still a ping,
    // so unwrap an inline <code> whose whole content is exactly one such mention
    // into the same blue span as a plain-text @, keeping rooms consistent
    // regardless of whether the agent used backticks. Real fenced code (<pre>)
    // and non-name code (e.g. `@param`, which won't match `alt`) are untouched.
    const fullRe = new RegExp("^\\s*@(" + alt + ")\\s*$", "i");
    root.querySelectorAll("code").forEach((code) => {
      for (let p = code.parentNode; p && p !== root; p = p.parentNode) {
        if (p.nodeName === "PRE") return; // leave real code blocks alone
      }
      const m = fullRe.exec(code.textContent || "");
      if (!m) return;
      const span = document.createElement("span");
      span.className = nameToClass[m[1].toLowerCase()] || "mention";
      span.textContent = "@" + m[1];
      code.parentNode.replaceChild(span, code);
    });
    const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT, null);
    const targets = [];
    for (let n = walker.nextNode(); n; n = walker.nextNode()) {
      if (n.nodeValue.indexOf("@") >= 0 && !inSkippable(n)) targets.push(n);
    }
    targets.forEach((node) => {
      const text = node.nodeValue;
      const frag = document.createDocumentFragment();
      let last = 0, m;
      re.lastIndex = 0;
      while ((m = re.exec(text))) {
        if (m.index > 0 && /\w/.test(text[m.index - 1])) continue; // email-ish
        if (m.index > last) frag.appendChild(document.createTextNode(text.slice(last, m.index)));
        const span = document.createElement("span");
        span.className = nameToClass[m[1].toLowerCase()] || "mention";
        span.textContent = m[0];
        frag.appendChild(span);
        last = m.index + m[0].length;
      }
      if (last === 0) return;
      if (last < text.length) frag.appendChild(document.createTextNode(text.slice(last)));
      node.parentNode.replaceChild(frag, node);
    });
  }

  function renderRoomTranscript(turns, participants, moderator) {
    const wrap = $("#room-thread");
    if (!wrap) return;
    wrap.innerHTML = "";
    // @operator → blue pill (you); each participant → blue text (peer ping).
    const nameToClass = { operator: "mention-me" };
    (participants || []).forEach((p) => { nameToClass[p.toLowerCase()] = "mention"; });
    let lastRound = 0;
    turns.forEach((tr) => {
      // Roundtable: a visible "第 X 轮" divider when the round number bumps.
      if (tr.round && tr.round !== lastRound) {
        wrap.appendChild(el("div", "round-sep", `第 ${tr.round} 轮`));
        lastRound = tr.round;
      }
      const ms = tr.ts ? new Date(tr.ts).getTime() : null;
      // The moderator's per-round consolidation (roundtable) — render it as a
      // distinct 小结 card, not a regular bubble.
      if (moderator && tr.speaker === moderator && !tr.error) {
        const mb = msgBlock("a");
        if (ms != null) mb.dataset.ts = ms;
        mb.dataset.agent = tr.speaker;
        const card = el("div", "consolidation");
        card.appendChild(el("div", "ctitle", tr.round ? `主持小结 · 第 ${tr.round} 轮` : "主持小结"));
        const body = el("div", "cbody", mdToHtml(tr.text));
        highlightMentions(body, nameToClass);
        card.appendChild(body);
        mb.appendChild(card);
        mb.appendChild(msgActions(tr.text, "a"));
        wrap.appendChild(mb);
        return;
      }
      const isOp = tr.speaker === "operator";
      const b = msgBlock(isOp ? "u" : "a");
      b.appendChild(el("div", null, msgHead(isOp ? "u" : "a", tr.speaker, ms, tr.ts)));
      if (isOp) {
        const ub = el("div", "umsg", `<div class="b">${mdToHtml(tr.text)}</div>`);
        highlightMentions(ub, nameToClass);
        b.appendChild(ub);
        b.appendChild(msgActions(tr.text, "u"));
        wrap.appendChild(b);
        return;
      }
      if (ms != null) b.dataset.ts = ms; // 轨迹 node → bubble jump key
      b.dataset.agent = tr.speaker;
      if (tr.error) {
        b.appendChild(el("div", "amsg failed", `<p>✗ ${esc(tr.error)}</p>`));
        b.appendChild(msgActions(tr.error || "失败", "a", tr.speaker));
      } else {
        const bub = el("div", "amsg", mdToHtml(tr.text));
        highlightMentions(bub, nameToClass); // @operator (pill) + peers (text)
        b.appendChild(bub);
        b.appendChild(msgActions(tr.text, "a", tr.speaker));
      }
      wrap.appendChild(b);
    });
  }

  // One message block: a wrapper around header + bubble + action row, so the
  // actions can reveal on hover/focus of the whole block (see .msg:hover in
  // app.css) instead of cluttering every bubble permanently. side = "u" | "a".
  function msgBlock(side) { return el("div", "msg " + (side === "u" ? "u" : "a")); }

  // Action row under a message bubble: a 复制 button (copies the raw source
  // text) always, plus a 直接回复 button when replySpeaker is set — the latter
  // only makes sense in a roundtable, where "@<speaker>" routes the next turn.
  // side ("u"|"a") aligns the row under its bubble (user bubbles are
  // right-aligned). Glyphs live in the button text (not SVG) so copyText's
  // flashBtn can swap the label to "✓ 已复制" and back without losing an icon.
  function msgActions(rawText, side, replySpeaker) {
    const row = el("div", "msg-actions " + (side === "u" ? "u" : "a"));
    const copy = el("button", "msg-act", "⧉");
    copy.title = "复制";
    copy.addEventListener("click", (e) => copyText(rawText, null, e.currentTarget));
    row.appendChild(copy);
    if (replySpeaker) {
      const reply = el("button", "msg-act reply", "↩");
      reply.title = `直接回复 @${replySpeaker}`;
      reply.addEventListener("click", () => replyTo(replySpeaker));
      row.appendChild(reply);
    }
    return row;
  }

  // Drop "@<name>" into the composer. Two placements:
  //   atCursor=false → prepend at the start, de-duplicated (the bubble's 直接回复)
  //   atCursor=true  → insert at the caret/drop point, space-padded (drag a card)
  // In 1:1 chat the mention is inert text (the agent is fixed by the select); in a
  // roundtable it routes the next turn to that agent.
  // Insert `snippet` at the composer's caret (replacing any selection),
  // space-padded so it never fuses with neighbouring words, then place the
  // caret right after it. Shared by @-mentions and dropped file paths.
  function insertAtCursor(snippet) {
    const ta = $("#ta");
    if (!ta || !snippet) return;
    const v = ta.value;
    const a = ta.selectionStart == null ? v.length : ta.selectionStart;
    const b = ta.selectionEnd == null ? v.length : ta.selectionEnd;
    const before = v.slice(0, a), after = v.slice(b);
    const lead = (!before || /\s$/.test(before)) ? "" : " ";
    const trail = /^\s/.test(after) ? "" : " ";
    const ins = lead + snippet + trail;
    ta.value = before + ins + after;
    const caret = (before + ins).length;
    ta.focus();
    ta.setSelectionRange(caret, caret);
    autosize();
  }

  function insertMention(name, atCursor) {
    const ta = $("#ta");
    if (!ta || !name) return;
    const tag = "@" + name;
    if (atCursor) return insertAtCursor(tag);
    const reTag = tag.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
    const has = new RegExp("(^|\\s)" + reTag + "(\\s|$)").test(ta.value);
    if (!has) ta.value = ta.value ? `${tag} ${ta.value}` : `${tag} `;
    ta.focus();
    ta.setSelectionRange(ta.value.length, ta.value.length);
    autosize();
  }
  function replyTo(name) { insertMention(name, false); }

  // Mark a right-rail element as a draggable agent handle: dropping it on the
  // composer inserts "@<name>" at the caret. A text/plain fallback means a drop
  // somewhere else still pastes the mention.
  function setDragAgent(node, name) {
    if (!node || !name) return;
    node.setAttribute("draggable", "true");
    node.classList.add("draggable");
    node.addEventListener("dragstart", (e) => {
      e.dataTransfer.setData("application/x-ahsir-agent", name);
      e.dataTransfer.setData("text/plain", "@" + name);
      e.dataTransfer.effectAllowed = "copy";
    });
  }

  // Pull absolute local paths out of a Finder (or file-manager) drag. macOS
  // hands web drop targets a text/uri-list of file:// URLs; decode each to a
  // plain filesystem path. The browser deliberately hides paths from the
  // `files` list, so this URI list is the only handle we get — which is why
  // this only works for drags that originate from the OS file manager.
  function pathsFromDrop(dt) {
    const out = [], seen = new Set();
    const addUri = (line) => {
      line = (line || "").trim();
      if (!line || line.startsWith("#") || !/^file:\/\//i.test(line)) return;
      let p = line.replace(/^file:\/\/(localhost)?/i, "");
      try { p = decodeURIComponent(p); } catch (_) {}
      if (p && !seen.has(p)) { seen.add(p); out.push(p); }
    };
    const uriList = dt.getData("text/uri-list");
    if (uriList) uriList.split(/\r?\n/).forEach(addUri);
    if (!out.length) (dt.getData("text/plain") || "").split(/\r?\n/).forEach(addUri);
    return out;
  }
  // Quote a path that contains whitespace so it reads as one token in the
  // prompt; leave clean paths bare to match what the operator would type.
  function fmtPath(p) { return /\s/.test(p) ? `'${p}'` : p; }

  // Wire the composer as a drop target (idempotent). Two kinds of payload:
  //   - a dragged agent handle (our custom type) → insert "@<agent>"
  //   - a file dragged from the OS file manager   → insert its local path(s)
  // Local path only — by design we do NOT upload or check the agent host (the
  // operator runs agents on this machine). claude reads the path itself.
  function initComposerDrop() {
    const ta = $("#ta");
    if (!ta || ta.dataset.dropWired === "1") return;
    ta.dataset.dropWired = "1";
    const accepts = (e) => {
      const types = e.dataTransfer && Array.from(e.dataTransfer.types || []);
      return !!types && (types.includes("application/x-ahsir-agent") ||
        types.includes("Files") || types.includes("text/uri-list"));
    };
    ta.addEventListener("dragover", (e) => {
      if (!accepts(e)) return;
      e.preventDefault(); // claim it: valid drop target + caret tracks the pointer
      e.dataTransfer.dropEffect = "copy";
      ta.classList.add("drag-over");
    });
    // Paste an image (or any file) from the clipboard: same pipeline as a drop.
    // Plain text/html pastes have no "file" item, so they fall through untouched.
    ta.addEventListener("paste", (e) => {
      const items = (e.clipboardData && e.clipboardData.items) || [];
      const files = [];
      for (const it of items) {
        if (it.kind === "file") { const f = it.getAsFile(); if (f) files.push(f); }
      }
      if (!files.length) return; // ordinary text paste
      e.preventDefault();
      uploadFiles(files);
    });
    ta.addEventListener("dragleave", () => ta.classList.remove("drag-over"));
    ta.addEventListener("drop", (e) => {
      const dt = e.dataTransfer;
      if (!dt) return;
      ta.classList.remove("drag-over");
      const name = dt.getData("application/x-ahsir-agent");
      if (name) { e.preventDefault(); insertMention(name, true); return; }
      const types = Array.from(dt.types || []);
      const hasFiles = (dt.files && dt.files.length) || types.includes("Files");
      if (!hasFiles && !types.includes("text/uri-list")) return; // not ours
      // We claimed a file drag in dragover — always preventDefault so the
      // browser never navigates away to open the dropped file.
      e.preventDefault();
      // Browsers hide the dragged file's local path (Chromium gives nothing,
      // Safari a file:// URL). Always upload the bytes so the server can copy
      // them into the agent-readable upload dir and hand back a usable path —
      // snapshot the FileList now (it empties after the event). Fall back to a
      // file:// path only if there are no File objects at all.
      const files = dt.files && dt.files.length ? Array.from(dt.files) : [];
      if (files.length) { uploadFiles(files); return; }
      const paths = pathsFromDrop(dt);
      if (paths.length) insertAtCursor(paths.map(fmtPath).join(" "));
      else toast("拿不到文件，请从 Finder/文件管理器拖拽");
    });
  }

  // Upload dropped files to the console, which copies them into the
  // agent-readable upload dir and returns absolute paths; insert those at the
  // caret. Keeps the operator's drag working in any browser despite the path
  // being hidden client-side.
  async function uploadFiles(files) {
    // Pasted clipboard images often arrive nameless; synthesize a name with the
    // right extension from the MIME type so the saved path is recognisable (and
    // claude can tell it's an image).
    const nameOf = (f, i) =>
      f.name || `pasted-${i + 1}.${(f.type.split("/")[1] || "bin").replace(/[^a-z0-9]/gi, "")}`;
    const label = files.length > 1 ? `上传 ${files.length} 个文件…` : `上传 ${nameOf(files[0], 0)}…`;
    toast(label);
    const fd = new FormData();
    files.forEach((f, i) => fd.append("file", f, nameOf(f, i)));
    try {
      const r = await fetch(api("/upload"), { method: "POST", body: fd });
      if (!r.ok) throw new Error((await safeErr(r)) || r.statusText);
      const data = await r.json();
      const paths = (data.files || []).map((x) => x.path).filter(Boolean);
      if (!paths.length) throw new Error("服务端未返回路径");
      insertAtCursor(paths.map(fmtPath).join(" "));
      toast(paths.length > 1 ? `已插入 ${paths.length} 个文件路径` : "已插入文件路径");
    } catch (e) {
      toast("上传失败：" + e.message);
    }
  }

  function renderRoomRoster(view) {
    const rows = $("#agentRows");
    if (!rows) return;
    // Tie the detail card to the room: if the selected agent isn't a participant
    // (e.g. the chat-mode default agent — teacher — left over), switch to the
    // first participant so the card below never shows an unrelated agent.
    if (!view.participants.includes(state.agent)) {
      state.agent = view.participants[0] || null;
    }
    $("#agentCount").textContent = view.participants.length;
    rows.innerHTML = "";
    view.participants.forEach((name) => {
      const a = agentByName(name) || { name, status: "unknown" };
      const isNext = view.next === name;
      const isOrg = view.organizer === name;
      const row = el("div", "agent-row" + (name === state.agent ? " on" : ""));
      row.innerHTML =
        `<span class="av" style="background:${avatarColor(name)}">${esc(name.slice(0, 2))}</span>` +
        `<div class="nm"><div class="a">${esc(name)}${isOrg ? " · 主持" : ""}</div><div class="b mono">${esc(a.url || "")}</div></div>` +
        `<span class="pill">${isNext ? "发言中" : esc(a.status || "")}</span>`;
      // Clicking a participant points the detail card at it. Room-safe: only
      // re-renders the roster + detail, never the thread (unlike selectAgent).
      row.addEventListener("click", () => { state.agent = name; renderRoomRoster(view); renderDetail(); });
      setDragAgent(row, name);
      rows.appendChild(row);
    });
    renderDetail();
  }

  async function sayInRoom(text) {
    if (!state.roomId) { toast("先发起或选择一个圆桌"); return; }
    try {
      renderRoom(await postJSON(`/rooms/${encodeURIComponent(state.roomId)}/say`, { text, speaker: "operator" }));
    } catch (e) {
      toast("发送失败：" + e.message);
    }
  }

  async function stopRoom() {
    if (!state.roomId) return;
    try { renderRoom(await postJSON(`/rooms/${encodeURIComponent(state.roomId)}/stop`, {})); loadRooms(); }
    catch (e) { toast(e.message); }
  }

  function roomStatusZh(s) {
    return { active: "进行中", waiting: "等待主持", stopped: "已结束" }[s] || s;
  }

  async function loadRooms() {
    let rooms;
    try { rooms = await getJSON("/rooms"); } catch (_) { return; }
    const box = $("#rooms");
    box.innerHTML = "";
    if (!rooms || !rooms.length) return;
    box.appendChild(el("div", "grp", `房间<span class="c">${rooms.length}</span>`));
    rooms.forEach((r) => {
      const dot = r.status === "active" ? "s-run" : r.status === "stopped" ? "s-idle" : "s-wait";
      const isRt = r.mode === "roundtable";
      const tag = isRt ? "圆桌" : "协同";
      const row = el("div", "sess" + (r.id === state.roomId ? " on" : ""));
      row.innerHTML =
        `<span class="dot ${dot}"></span><span class="t">${esc(r.topic || (isRt ? "圆桌" : "多 Agent 协同"))}</span>` +
        `<span class="meta">${esc(tag)} · ${r.participants.length}人</span>`;
      row.title = isRt
        ? `${r.participants.join(", ")} · 主持 ${r.moderator || "?"}（圆桌共识）`
        : `${r.participants.join(", ")} · 组织者 ${r.organizer}（多 Agent 协同）`;
      row.addEventListener("click", () => openRoom(r.id));
      box.appendChild(row);
    });
  }

  // ---- wiring -------------------------------------------------------------

  function autosize() {
    const ta = $("#ta");
    ta.style.height = "auto";
    ta.style.height = Math.min(ta.scrollHeight, 160) + "px";
  }

  // Show the "jump to bottom" button only when the thread is scrolled up.
  function updateJumpToBottom() {
    const t = $("#thread"), btn = $("#jumpBtn");
    if (!t || !btn) return;
    const atBottom = t.scrollHeight - t.scrollTop - t.clientHeight < 80;
    btn.classList.toggle("hidden", atBottom);
  }
  function scrollThreadToBottom() {
    const t = $("#thread");
    if (t) t.scrollTo({ top: t.scrollHeight, behavior: "smooth" });
  }

  function initTheme() {
    const root = document.documentElement;
    try { const sv = localStorage.getItem("ahsir-theme"); if (sv) root.setAttribute("data-theme", sv); } catch (_) {}
    $("#themeBtn").addEventListener("click", () => {
      const n = root.getAttribute("data-theme") === "dark" ? "light" : "dark";
      root.setAttribute("data-theme", n);
      try { localStorage.setItem("ahsir-theme", n); } catch (_) {}
    });
  }

  function showMobileSurface(surface) {
    const left = document.querySelector(".rail.left");
    const center = document.querySelector("main.center");
    const right = document.querySelector(".rail.right");
    const missingSurfaces = [
      ["left", left],
      ["center", center],
      ["right", right],
    ].filter(([, element]) => !element).map(([name]) => name);
    const buttons = Array.from(document.querySelectorAll(".mob button"));
    const resetButtons = () => buttons.forEach((button) => {
      button.classList.remove("on");
      button.setAttribute("aria-pressed", "false");
    });
    if (missingSurfaces.length === 3 && buttons.length === 0) return [];
    if (missingSurfaces.length > 0) {
      resetButtons();
      throw new Error(`mobile navigation missing layout surfaces: ${missingSurfaces.join(", ")}`);
    }
    const expectedButtonSurfaces = ["left", "center", "right"];
    const invalidButtonSurfaces = expectedButtonSurfaces.filter(
      (expected) => buttons.filter((button) => button.dataset.surface === expected).length !== 1,
    );
    const unexpectedButtons = buttons.filter(
      (button) => !expectedButtonSurfaces.includes(button.dataset.surface),
    );
    if (invalidButtonSurfaces.length > 0 || unexpectedButtons.length > 0) {
      resetButtons();
      throw new Error(
        `mobile navigation requires exactly one button for: ${invalidButtonSurfaces.join(", ") || "left, center, right"}`,
      );
    }
    left.classList.toggle("show", surface === "left");
    right.classList.toggle("show", surface === "right");
    center.classList.toggle("hide", surface !== "center");
    center.classList.toggle("show", surface === "center");
    buttons.forEach((button) => {
      const active = button.dataset.surface === surface;
      button.classList.toggle("on", active);
      button.setAttribute("aria-pressed", String(active));
    });
    return buttons;
  }

  function init() {
    const mobileButtons = showMobileSurface("center");
    mobileButtons.forEach((button) => {
      button.addEventListener("click", () => showMobileSurface(button.dataset.surface));
    });
    initTheme();
    initComposerDrop();
    // Notifications: set the favicon, stop the flash when the tab regains focus,
    // and on the first user gesture unlock audio + ask for popup permission
    // (browsers require a gesture for both).
    setFavicon(false);
    window.addEventListener("focus", stopFlash);
    document.addEventListener("visibilitychange", () => { if (!document.hidden) stopFlash(); });
    const unlock = () => {
      ensureAudio();
      if (("Notification" in window) && Notification.permission === "default") {
        try { Notification.requestPermission(); } catch (_) {}
      }
      window.removeEventListener("pointerdown", unlock);
      window.removeEventListener("keydown", unlock);
    };
    window.addEventListener("pointerdown", unlock);
    window.addEventListener("keydown", unlock);
    $("#thread").addEventListener("scroll", updateJumpToBottom);
    $("#jumpBtn").addEventListener("click", scrollThreadToBottom);
    $("#ta").addEventListener("input", autosize);
    $("#ta").addEventListener("keydown", (e) => {
      if ((e.metaKey || e.ctrlKey) && e.key === "Enter") { e.preventDefault(); send(); }
    });
    $("#sendBtn").addEventListener("click", send);
    $("#newConvo").addEventListener("click", newConversation);
    $("#newRoom").addEventListener("click", newRoom);
    $("#newRoundtable").addEventListener("click", newRoundtable);
    $("#newAgent").addEventListener("click", newAgentForm);
    $("#agentSel").addEventListener("change", (e) => selectAgent(e.target.value));
    document.querySelectorAll(".rtab").forEach((tab) => {
      tab.addEventListener("click", () => {
        document.querySelectorAll(".rtab").forEach((x) => x.classList.remove("on"));
        tab.classList.add("on");
        const k = tab.dataset.tab;
        document.querySelectorAll(".panel").forEach((p) => p.classList.toggle("on", p.dataset.panel === k));
      });
    });

    loadAgents().then(() => { loadContexts(); loadArchived(); loadRooms(); });
    // Light polling so external CLI activity shows up in the rails.
    setInterval(async () => {
      loadRooms();
      if (state.mode === "chat") {
        const waitingForAgent = state.chatEmptyKind === "no-agent";
        const agentLoadOK = waitingForAgent ? await loadAgents() : false;
        if (waitingForAgent && state.mode === "chat" && state.chatEmptyKind === "no-agent" &&
            agentLoadOK && state.agents.length) {
          const found = state.agents[0].name;
          state.agent = found;
          toast(`已发现 ${found}`);
          newConversation();
        }
        loadContexts();
        loadArchived();
        if (state.contextId) loadTrace();
      }
    }, 8000);
  }

  document.addEventListener("DOMContentLoaded", init);
})();
