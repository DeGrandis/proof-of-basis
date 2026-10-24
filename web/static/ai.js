"use strict";
// The assistant: a floating chat backed by the user's own Ollama server.
// It appears only once Settings has an Ollama address and model. Each
// question carries where the user is in the app; the server adds the data
// and runs the lookups the model asks for. Chats are saved on the server.

const ai = {
  enabled: false, ok: false, model: "", host: "",
  chat: null,          // open chat id
  busy: null,          // AbortController while an answer streams
  focus: "",           // a record the user asked about directly
};
let aiOpen = (() => { try { return localStorage.getItem("aiOpen") === "1"; } catch { return false; } })();
try { ai.chat = Number(localStorage.getItem("aiChat")) || null; } catch {}

const AI_ICONS = {
  spark: `<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M12 3l1.8 4.9L19 9.7l-5.2 1.8L12 16.4l-1.8-4.9L5 9.7l5.2-1.8z"/><path d="M18.5 15.5l.8 2.2 2.2.8-2.2.8-.8 2.2-.8-2.2-2.2-.8 2.2-.8z"/></svg>`,
  list: `<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M4 6h16M4 12h16M4 18h10"/></svg>`,
  plus: `<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M12 5v14M5 12h14"/></svg>`,
  close: `<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M6 6l12 12M18 6L6 18"/></svg>`,
  send: `<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M12 19V5M6 11l6-6 6 6"/></svg>`,
  stop: `<svg viewBox="0 0 24 24" aria-hidden="true"><rect x="7" y="7" width="10" height="10" rx="2"/></svg>`,
  pencil: `<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M4 20h4L19 9l-4-4L4 16zM14 6l4 4"/></svg>`,
  trash: `<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M5 7h14M10 7V4h4v3M7 7l1 13h8l1-13"/></svg>`,
};

// The floating button and panel are added to every page.
document.body.insertAdjacentHTML("beforeend", `
<button id="ai-fab" class="ai-fab" type="button" aria-label="Ask the assistant" title="Ask the assistant" hidden>${AI_ICONS.spark}</button>
<section id="ai-panel" class="ai-panel" role="dialog" aria-label="Assistant" hidden>
  <header class="ai-head">
    <button type="button" id="ai-list-btn" class="ai-icon" title="Your chats" aria-label="Your chats">${AI_ICONS.list}</button>
    <b id="ai-title">New chat</b>
    <button type="button" id="ai-new" class="ai-icon" title="New chat" aria-label="New chat">${AI_ICONS.plus}</button>
    <button type="button" id="ai-close" class="ai-icon" title="Close" aria-label="Close">${AI_ICONS.close}</button>
  </header>
  <div id="ai-list" class="ai-list" hidden></div>
  <div id="ai-warn" class="ai-warn" hidden></div>
  <div id="ai-msgs" class="ai-msgs" aria-live="polite"></div>
  <form id="ai-form" class="ai-form">
    <div id="ai-ctx" class="ai-ctx"></div>
    <div class="ai-input">
      <textarea id="ai-q" rows="1" placeholder="Ask about this page…" aria-label="Your question"></textarea>
      <button type="submit" id="ai-send" class="ai-send" title="Send" aria-label="Send">${AI_ICONS.send}</button>
    </div>
    <div id="ai-foot" class="ai-foot"></div>
  </form>
</section>`);

const PAGE_NAMES = { overview: "Overview", wallets: "Wallets", imports: "Imports", review: "Review", lightning: "Lightning",
  graph: "Flow", taxes: "Taxes", settings: "Settings", tx: "This transaction", addr: "This address" };
const SUGGEST = {
  overview: ["How did my holdings change this year?", "How much of my unrealized gain is long-term?", "Which purchases cost me the most per BTC?"],
  review: ["What are these items, and how should I label them?", "Which of these is the biggest?"],
  taxes: ["Explain my biggest gain this year.", "Why is my estimated tax what it is?", "Compare this year with last year."],
  graph: ["Explain the selected record.", "Where did most of my coins come from?"],
  tx: ["Explain this transaction.", "Was this taxable, and why?"],
  wallets: ["Which wallet holds the most, and what's its cost basis?"],
  imports: ["Are my exchange imports up to date?"],
  lightning: ["Summarize my Lightning activity."],
  settings: ["What does my cost basis method mean for me?"],
  addr: ["What happened with this address?"],
  _: ["What should I look at first?"],
};

function aiPage() {
  const hash = location.hash.slice(1) || "overview";
  const page = hash.split("/")[0];
  return { hash: decodeURIComponent(hash), page };
}

function aiContext() {
  const { hash } = aiPage();
  return {
    hash,
    year: Number($("#year-select")?.value) || new Date().getUTCFullYear(),
    selected: typeof selected === "string" && selected ? selected : "",
    focus: ai.focus,
  };
}

function aiRenderCtx() {
  const { page } = aiPage();
  const name = PAGE_NAMES[page] || page;
  $("#ai-ctx").innerHTML = `<span class="ai-ctx-chip" title="The assistant sees what's on this page">Looking at: ${esc(name)}</span>`
    + (ai.focus ? `<span class="ai-ctx-chip focus">About a record <button type="button" id="ai-unfocus" aria-label="Stop asking about this record">×</button></span>` : "");
  $("#ai-unfocus")?.addEventListener("click", () => { ai.focus = ""; aiRenderCtx(); });
  $("#ai-q").placeholder = `Ask about ${page === "tx" ? "this transaction" : name === "Overview" ? "your portfolio" : name}…`;
}

// A small, safe markdown subset: paragraphs, lists, headings, tables,
// code, bold and italic, and links only to pages inside this app.
function aiMd(src) {
  const inline = (s) => esc(s)
    .replace(/`([^`]+)`/g, "<code>$1</code>")
    .replace(/\*\*([^*]+)\*\*/g, "<b>$1</b>")
    .replace(/(^|[^*])\*([^*\s][^*]*)\*/g, "$1<i>$2</i>")
    .replace(/\[([^\]]+)\]\((#[A-Za-z0-9/:%._-]+)\)/g, '<a href="$2">$1</a>');
  const lines = String(src || "").replace(/\r/g, "").split("\n");
  let html = "", i = 0;
  while (i < lines.length) {
    const l = lines[i];
    if (/^```/.test(l)) {
      const buf = []; i++;
      while (i < lines.length && !/^```/.test(lines[i])) buf.push(lines[i++]);
      i++; html += `<pre><code>${esc(buf.join("\n"))}</code></pre>`; continue;
    }
    if (/^\s*\|.*\|\s*$/.test(l) && /^\s*\|?[\s:-]+\|[\s|:-]*$/.test(lines[i + 1] || "")) {
      const row = (r) => r.trim().replace(/^\||\|$/g, "").split("|").map((c) => c.trim());
      const head = row(l); i += 2; const body = [];
      while (i < lines.length && /^\s*\|.*\|\s*$/.test(lines[i])) body.push(row(lines[i++]));
      html += `<div class="ai-table"><table><thead><tr>${head.map((c) => `<th>${inline(c)}</th>`).join("")}</tr></thead><tbody>${body.map((r) => `<tr>${r.map((c) => `<td>${inline(c)}</td>`).join("")}</tr>`).join("")}</tbody></table></div>`;
      continue;
    }
    if (/^\s*[-*•]\s+/.test(l) || /^\s*\d+[.)]\s+/.test(l)) {
      const ordered = /^\s*\d/.test(l), items = [];
      while (i < lines.length && (ordered ? /^\s*\d+[.)]\s+/ : /^\s*[-*•]\s+/).test(lines[i])) items.push(lines[i++].replace(/^\s*(?:[-*•]|\d+[.)])\s+/, ""));
      html += `<${ordered ? "ol" : "ul"}>${items.map((t) => `<li>${inline(t)}</li>`).join("")}</${ordered ? "ol" : "ul"}>`;
      continue;
    }
    if (/^#{1,4}\s+/.test(l)) { html += `<h4>${inline(l.replace(/^#+\s+/, ""))}</h4>`; i++; continue; }
    if (!l.trim()) { i++; continue; }
    const para = [];
    while (i < lines.length && lines[i].trim() && !/^(```|#{1,4}\s|\s*[-*•]\s|\s*\d+[.)]\s|\s*\|)/.test(lines[i])) para.push(lines[i++]);
    if (!para.length) para.push(lines[i++]);
    html += `<p>${para.map(inline).join("<br>")}</p>`;
  }
  return html;
}

function aiChips(lookups) {
  if (!lookups?.length) return "";
  return `<div class="ai-chips">${aiChipItems(lookups)}</div>`;
}
function aiChipItems(lookups) {
  return `${lookups.filter((l) => !l.file).map((l) => l.link
    ? `<a class="ai-chip${l.error ? " bad" : ""}" href="${esc(l.link)}" title="${esc(l.error || "Open the page this came from")}">${esc(l.summary)}</a>`
    : `<span class="ai-chip${l.error ? " bad" : ""}" title="${esc(l.error || "")}">${esc(l.summary)}</span>`).join("")}`;
}

function aiThinkHTML(text, secs, live) {
  if (!text && !live) return "";
  const label = live ? "Thinking…" : secs >= 1 ? `Thought for ${Math.round(secs)} s` : "Thought it through";
  return `<details class="ai-think${live ? " live" : ""}"><summary>${label}</summary><div class="ai-think-body">${esc(text || "").replace(/\n/g, "<br>")}</div></details>`;
}

// A small bar chart for a lookup that asked for one.
const AI_FMT = { usd: (v) => usd(Math.round(v * 100)), btc: (v) => amt(Math.round(v * 1e8)), days: (v) => `${Math.round(v)} d`, count: (v) => String(Math.round(v)) };
function aiChartHTML(c) {
  if (!c || !c.labels?.length) return "";
  const W = 560, H = 190, L = 8, R = 8, T = 22, B = 34, n = c.values.length;
  const max = Math.max(...c.values, 0), min = Math.min(...c.values, 0), span = max - min || 1;
  const bw = (W - L - R) / n, y = (v) => T + (H - T - B) * (max - v) / span, fmt = AI_FMT[c.unit] || AI_FMT.count;
  const every = Math.ceil(n / 12);
  let g = `<text class="ai-chart-title" x="${L}" y="14">${esc(c.title)}</text>`;
  c.values.forEach((v, i) => {
    const x = L + i * bw + bw * 0.15, w = bw * 0.7, top = Math.min(y(v), y(0)), h = Math.max(1, Math.abs(y(v) - y(0)));
    g += `<rect class="ai-bar${v < 0 ? " neg" : ""}" x="${x.toFixed(1)}" y="${top.toFixed(1)}" width="${w.toFixed(1)}" height="${h.toFixed(1)}" rx="2"><title>${esc(c.labels[i])}: ${esc(fmt(v))}</title></rect>`;
    if (i % every === 0) g += `<text class="ai-chart-label" x="${(x + w / 2).toFixed(1)}" y="${H - 18}" text-anchor="middle">${esc(String(c.labels[i]).slice(0, 10))}</text>`;
  });
  g += `<text class="ai-chart-label" x="${W - R}" y="14" text-anchor="end">max ${esc(fmt(max))}</text>`;
  return `<div class="ai-chart"><svg viewBox="0 0 ${W} ${H}" role="img" aria-label="${esc(c.title)}">${g}</svg></div>`;
}
const aiCharts = (lookups) => (lookups || []).map((l) => aiChartHTML(l.chart)).join("");
// Files the assistant made: a card with an icon, the name and Download.
const FILE_ICON = `<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M14 3H7a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2V8z"/><path d="M14 3v5h5M9 13h6M9 17h6"/></svg>`;
function aiFileHTML(f) {
  if (!f) return "";
  const kind = (f.name.split(".").pop() || "file").toUpperCase();
  return `<a class="ai-file" href="${esc(f.url)}" download="${esc(f.name)}"><span class="ai-file-icon">${FILE_ICON}</span><span class="ai-file-meta"><b>${esc(f.name)}</b><small>${kind} · ${f.rows.toLocaleString("en-US")} row${f.rows === 1 ? "" : "s"}</small></span><span class="ai-file-dl">Download</span></a>`;
}
const aiFiles = (lookups) => (lookups || []).map((l) => aiFileHTML(l.file)).join("");

function aiMsgHTML(m) {
  let data = {};
  try { data = m.data ? JSON.parse(m.data) : {}; } catch {}
  if (m.role === "user") return `<div class="ai-msg user"><div class="ai-bubble">${esc(m.content).replace(/\n/g, "<br>")}</div></div>`;
  return `<div class="ai-msg bot">${aiThinkHTML(data.thinking, data.thinkSecs)}${aiChips(data.lookups)}${(data.actions || []).map((id) => `<div class="act compact loading" data-action="${id}"></div>`).join("")}<div class="ai-text">${aiMd(m.content)}</div>${aiFiles(data.lookups)}${aiCharts(data.lookups)}${data.error && data.error !== "stopped" ? `<div class="ai-err">${esc(data.error)}</div>` : ""}${data.error === "stopped" ? `<div class="ai-note">Stopped.</div>` : ""}</div>`;
}

// Change cards in saved chats show their current state (applied, undone…).
async function aiHydrateActions() {
  for (const el of $$("#ai-msgs .act.loading")) {
    try { const a = await api(`/api/actions/${el.dataset.action}`); el.outerHTML = actionHTML(a, true); }
    catch { el.remove(); }
  }
}

function aiEmpty() {
  const { page } = aiPage();
  const list = SUGGEST[page] || SUGGEST._;
  $("#ai-msgs").innerHTML = `<div class="ai-empty"><div class="ai-empty-icon">${AI_ICONS.spark}</div><p>Ask anything about your bitcoin history and taxes. The assistant sees the page you're on and can look up transactions, tax years and accounts.</p>
    <div class="ai-suggest">${list.map((q) => `<button type="button" data-q="${esc(q)}">${esc(q)}</button>`).join("")}</div></div>`;
}

let aiLoadSeq = 0;
async function aiLoadChat(id) {
  const seq = ++aiLoadSeq; // a later call wins over a slower earlier one
  ai.chat = id;
  try { localStorage.setItem("aiChat", id || ""); } catch {}
  $("#ai-list").hidden = true;
  if (!id) { $("#ai-title").textContent = "New chat"; aiEmpty(); return; }
  try {
    const d = await api(`/api/ai/chats/${id}`);
    if (seq !== aiLoadSeq) return;
    $("#ai-title").textContent = d.chat.title;
    if (!d.messages.length) aiEmpty();
    else { $("#ai-msgs").innerHTML = d.messages.map(aiMsgHTML).join(""); aiScroll(true); aiHydrateActions(); }
  } catch {
    if (seq !== aiLoadSeq) return;
    ai.chat = null; $("#ai-title").textContent = "New chat"; aiEmpty();
  }
}

function aiScroll(force) {
  const box = $("#ai-msgs");
  if (force || box.scrollHeight - box.scrollTop - box.clientHeight < 120) box.scrollTop = box.scrollHeight;
}

async function aiShowList() {
  const box = $("#ai-list");
  if (!box.hidden) { box.hidden = true; return; }
  let chats = [];
  try { chats = await api("/api/ai/chats"); } catch (e) { toast(e.message); return; }
  const when = (t) => new Date(t * 1000).toLocaleDateString(undefined, { month: "short", day: "numeric", year: new Date(t * 1000).getFullYear() === new Date().getFullYear() ? undefined : "numeric" });
  box.innerHTML = (chats.length ? `<ul>${chats.map((c) => `<li class="${c.id === ai.chat ? "on" : ""}">
      <button type="button" class="ai-open" data-id="${c.id}"><span>${esc(c.title)}</span><small>${when(c.updated)}</small></button>
      <button type="button" class="ai-icon" data-rename="${c.id}" title="Rename" aria-label="Rename">${AI_ICONS.pencil}</button>
      <button type="button" class="ai-icon" data-del="${c.id}" title="Delete" aria-label="Delete">${AI_ICONS.trash}</button></li>`).join("")}</ul>
      <div class="ai-list-foot"><button type="button" id="ai-del-all" class="link">Delete all chats</button></div>`
    : `<p class="muted ai-list-none">No saved chats yet.</p>`);
  box.hidden = false;
}

$("#ai-list").addEventListener("click", async (e) => {
  const open = e.target.closest("[data-id]"), ren = e.target.closest("[data-rename]"), del = e.target.closest("[data-del]");
  if (open) aiLoadChat(Number(open.dataset.id));
  else if (ren) {
    const t = prompt("Name this chat", ren.closest("li").querySelector("span").textContent);
    if (t && t.trim()) { try { await post(`/api/ai/chats/${ren.dataset.rename}/rename`, { title: t.trim() }); if (Number(ren.dataset.rename) === ai.chat) $("#ai-title").textContent = t.trim(); } catch (err) { toast(err.message); } $("#ai-list").hidden = true; aiShowList(); }
  } else if (del) {
    if (!confirm("Delete this chat?")) return;
    try { await api(`/api/ai/chats/${del.dataset.del}`, { method: "DELETE" }); } catch (err) { toast(err.message); }
    if (Number(del.dataset.del) === ai.chat) aiLoadChat(null);
    $("#ai-list").hidden = true; aiShowList();
  } else if (e.target.id === "ai-del-all") {
    if (!confirm("Delete every saved chat?")) return;
    try { await api("/api/ai/chats", { method: "DELETE" }); } catch (err) { toast(err.message); }
    aiLoadChat(null);
  }
});

function aiSetOpen(open) {
  aiOpen = open;
  try { localStorage.setItem("aiOpen", open ? "1" : "0"); } catch {}
  $("#ai-panel").hidden = !open;
  $("#ai-fab").hidden = open || !ai.enabled;
  document.body.classList.toggle("ai-on", open);
  if (open) {
    aiRenderCtx();
    if (!$("#ai-msgs").childElementCount) aiLoadChat(ai.chat);
    setTimeout(() => $("#ai-q").focus(), 30);
  }
}

$("#ai-fab").addEventListener("click", () => aiSetOpen(true));
$("#ai-close").addEventListener("click", () => aiSetOpen(false));
$("#ai-new").addEventListener("click", () => { ai.focus = ""; aiRenderCtx(); aiLoadChat(null); $("#ai-q").focus(); });
$("#ai-list-btn").addEventListener("click", aiShowList);
$("#ai-msgs").addEventListener("click", (e) => {
  const s = e.target.closest("[data-q]");
  if (s) { $("#ai-q").value = s.dataset.q; aiSend(); }
});
document.addEventListener("keydown", (e) => { if (e.key === "Escape" && aiOpen && !$("#ai-list").hidden) $("#ai-list").hidden = true; });
window.addEventListener("hashchange", () => { if (aiOpen) aiRenderCtx(); if (aiOpen && $(".ai-empty")) aiEmpty(); });

const q = $("#ai-q");
q.addEventListener("input", () => { q.style.height = "auto"; q.style.height = Math.min(q.scrollHeight, 160) + "px"; });
q.addEventListener("keydown", (e) => { if (e.key === "Enter" && !e.shiftKey && !e.isComposing) { e.preventDefault(); aiSend(); } });
$("#ai-form").addEventListener("submit", (e) => { e.preventDefault(); if (ai.busy) ai.busy.abort(); else aiSend(); });

// Anything in the app can open the chat about one record.
window.askAssistant = async (key, question) => {
  if (!ai.enabled) return;
  ai.focus = key || "";
  ai.chat = null; // a fresh chat about this record
  aiSetOpen(true);
  await aiLoadChat(null);
  aiRenderCtx();
  if (question) { $("#ai-q").value = question; aiSend(); }
};
document.addEventListener("click", (e) => {
  const b = e.target.closest("[data-ask]");
  if (b) { e.preventDefault(); window.askAssistant(b.dataset.ask, b.dataset.askQ || "Explain this record."); }
});

async function aiSend() {
  const text = q.value.trim();
  if (!text || ai.busy) return;
  if (!ai.chat) {
    try { ai.chat = (await post("/api/ai/chats", {})).id; try { localStorage.setItem("aiChat", ai.chat); } catch {} } catch (e) { toast(e.message); return; }
  }
  $("#ai-list").hidden = true;
  if ($(".ai-empty")) $("#ai-msgs").innerHTML = "";
  if ($("#ai-title").textContent === "New chat") $("#ai-title").textContent = text.replace(/\s+/g, " ").slice(0, 60);
  $("#ai-msgs").insertAdjacentHTML("beforeend", aiMsgHTML({ role: "user", content: text }) + `<div class="ai-msg bot live"><div class="ai-think-slot"></div><div class="ai-chips"></div><div class="ai-acts"></div><div class="ai-text"><span class="ai-dots"><i></i><i></i><i></i></span></div><div class="ai-files"></div><div class="ai-live-charts"></div></div>`);
  const live = $("#ai-msgs .ai-msg.live"), textBox = $(".ai-text", live), chipBox = $(".ai-chips", live), thinkSlot = $(".ai-think-slot", live);
  q.value = ""; q.style.height = "auto";
  aiScroll(true);
  ai.busy = new AbortController();
  $("#ai-send").innerHTML = AI_ICONS.stop; $("#ai-send").title = "Stop"; $("#ai-send").setAttribute("aria-label", "Stop");
  let answer = "", lookups = [], error = "", thought = "", thinkMs = 0, thinkAt = 0; const actions = [];
  const drawThink = () => {
    const open = $("details", thinkSlot)?.open;
    thinkSlot.innerHTML = aiThinkHTML(thought, thinkMs / 1000, !!thinkAt);
    if (open) $("details", thinkSlot).open = true;
    const body = $(".ai-think-body", thinkSlot);
    if (body) body.scrollTop = body.scrollHeight;
  };
  const endThink = () => { if (thinkAt) { thinkMs += Date.now() - thinkAt; thinkAt = 0; drawThink(); } };
  const draw = () => { textBox.innerHTML = answer ? aiMd(answer) : `<span class="ai-dots"><i></i><i></i><i></i></span>`; aiScroll(); };
  try {
    const r = await fetch(`/api/ai/chats/${ai.chat}/send`, {
      method: "POST", headers: { "Content-Type": "application/json" }, signal: ai.busy.signal,
      body: JSON.stringify({ content: text, page: aiContext() }),
    });
    if (r.status === 401) { location.href = "/login"; return; }
    if (!r.ok) { const b = await r.json().catch(() => ({})); throw new Error(b.error || r.statusText); }
    const rd = r.body.getReader(), dec = new TextDecoder();
    let buf = "";
    for (;;) {
      const { value, done } = await rd.read();
      if (done) break;
      buf += dec.decode(value, { stream: true });
      let nl;
      while ((nl = buf.indexOf("\n")) >= 0) {
        const line = buf.slice(0, nl); buf = buf.slice(nl + 1);
        if (!line.trim()) continue;
        let ev; try { ev = JSON.parse(line); } catch { continue; }
        if (ev.type === "thinking") { if (!thinkAt) thinkAt = Date.now(); thought += ev.text; drawThink(); aiScroll(); }
        else if (ev.type === "delta") { endThink(); answer += ev.text; draw(); }
        else if (ev.type === "action") { actions.push(ev.action.id); $(".ai-acts", live).insertAdjacentHTML("beforeend", actionHTML(ev.action, true)); aiScroll(); }
        else if (ev.type === "tool") {
          endThink(); lookups.push(ev.lookup); chipBox.innerHTML = aiChipItems(lookups);
          if (ev.lookup.file) $(".ai-files", live).insertAdjacentHTML("beforeend", aiFileHTML(ev.lookup.file));
          if (ev.lookup.chart) $(".ai-live-charts", live).insertAdjacentHTML("beforeend", aiChartHTML(ev.lookup.chart));
          aiScroll();
        }
        else if (ev.type === "error") error = ev.error;
      }
    }
  } catch (e) {
    error = e.name === "AbortError" ? "stopped" : e.message;
  } finally {
    ai.busy = null;
    $("#ai-send").innerHTML = AI_ICONS.send; $("#ai-send").title = "Send"; $("#ai-send").setAttribute("aria-label", "Send");
  }
  live.classList.remove("live");
  endThink();
  live.outerHTML = aiMsgHTML({ role: "assistant", content: answer || (error ? "" : "(No answer.)"), data: JSON.stringify({ lookups, error, thinking: thought.trim(), thinkSecs: thinkMs / 1000, actions }) });
  aiHydrateActions();
  aiScroll();
}

async function aiStatus() {
  let st = { enabled: false };
  try { st = await api("/api/ai/status"); } catch {}
  Object.assign(ai, { enabled: !!st.enabled, ok: !!st.ok, model: st.model || "", host: st.host || "" });
  document.body.classList.toggle("ai-ready", ai.enabled);
  $("#ai-fab").hidden = !ai.enabled || aiOpen;
  $("#ai-foot").textContent = ai.enabled ? `Answers come from ${ai.model} on ${ai.host}, using your data. Check figures before you file.` : "";
  $("#ai-warn").hidden = !ai.enabled || ai.ok;
  $("#ai-warn").textContent = st.error ? `Can't reach Ollama right now: ${st.error}` : "";
  if (!ai.enabled && aiOpen) aiSetOpen(false);
  else if (ai.enabled && aiOpen) aiSetOpen(true);
}

// Settings: address, models, context size.
(() => {
  const f = $("#ai-settings");
  if (!f) return;
  const line = $("#ai-settings-line");
  let known = []; // models from the last test, with what they can do
  // The thinking switch only matters for models that can reason first.
  const syncThink = () => {
    const m = known.find((x) => x.name === f.aiModel.value);
    const cant = !!m && !m.thinking;
    f.aiThink.disabled = cant;
    $("#ai-think-note").hidden = !cant;
  };
  f.aiModel.addEventListener("change", syncThink);
  const fillModels = (models, current) => {
    known = models;
    const sel = f.aiModel;
    const size = (b) => b ? ` · ${(b / 1e9).toFixed(1)} GB` : "";
    sel.innerHTML = `<option value="">Pick a model</option>` + models.map((m) => `<option value="${esc(m.name)}"${m.tools ? "" : " disabled"}${m.name === current ? " selected" : ""}>${esc(m.name)}${size(m.size)}${m.tools ? "" : " (can't use tools)"}</option>`).join("");
    if (current && !models.some((m) => m.name === current)) sel.insertAdjacentHTML("beforeend", `<option value="${esc(current)}" selected>${esc(current)} (not found)</option>`);
    syncThink();
  };
  const test = async () => {
    const url = f.aiUrl.value.trim();
    if (!url) { line.textContent = "Enter your Ollama server's address first."; return false; }
    line.textContent = "Checking…";
    try {
      const d = await post("/api/ai/models", { url });
      fillModels(d.models, f.aiModel.value || f.dataset.model);
      const usable = d.models.filter((m) => m.tools).length;
      line.textContent = `Connected to Ollama ${d.version}. ${usable} of ${d.models.length} models can use the lookup tools.` + (usable ? "" : " Pull one, for example: ollama pull qwen3:8b") + (d.public ? " Warning: this address is on the public internet, so your data would leave your network." : "");
      line.classList.toggle("warn", !!d.public || !usable);
      return true;
    } catch (e) { line.textContent = e.message; line.classList.add("warn"); return false; }
  };
  api("/api/settings").then((s) => {
    f.aiUrl.value = s.aiUrl || ""; f.dataset.model = s.aiModel || ""; f.aiCtx.value = s.aiCtx || "16384"; f.aiThink.value = s.aiThink || "true"; f.aiAutoApply.checked = s.aiAutoApply === "true";
    if (s.aiModel) fillModels([], s.aiModel);
    if (s.aiUrl) test();
  }).catch(() => {});
  $("#ai-test").addEventListener("click", test);
  f.addEventListener("submit", async (e) => {
    e.preventDefault();
    if (!f.aiModel.value) { line.textContent = "Test the connection, then pick a model."; line.classList.add("warn"); return; }
    try {
      await post("/api/settings", { aiUrl: f.aiUrl.value.trim(), aiModel: f.aiModel.value, aiCtx: f.aiCtx.value, aiThink: f.aiThink.value, aiAutoApply: f.aiAutoApply.checked ? "true" : "false" });
      f.dataset.model = f.aiModel.value;
      toast("Assistant saved. Look for the button in the corner.");
      aiStatus();
    } catch (err) { toast(err.message); }
  });
  $("#ai-off").addEventListener("click", async () => {
    try { await post("/api/settings", { aiUrl: "", aiModel: "" }); } catch (err) { toast(err.message); return; }
    f.aiUrl.value = ""; f.aiModel.innerHTML = `<option value="">Test the connection first</option>`; f.dataset.model = ""; known = []; syncThink();
    line.textContent = "The assistant is off. Saved chats are kept."; line.classList.remove("warn");
    aiStatus();
  });
})();

aiStatus();
