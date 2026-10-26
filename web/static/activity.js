"use strict";
// Activity: every change to your data, by you or the assistant, newest
// first, with Undo (and Redo). The assistant's proposals wait here and in
// the chat until you Apply or Dismiss them.

let activityActor = (() => { try { return localStorage.getItem("activityActor") || ""; } catch { return ""; } })();

const ACT_ICONS = {
  user: `<svg viewBox="0 0 24 24" aria-hidden="true"><circle cx="12" cy="8" r="4"/><path d="M4 21c0-4 4-6 8-6s8 2 8 6"/></svg>`,
  assistant: `<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M12 3l1.8 4.9L19 9.7l-5.2 1.8L12 16.4l-1.8-4.9L5 9.7l5.2-1.8z"/></svg>`,
};
const STATUS_TEXT = { undone: "Undone", pending: "Waiting for you", dismissed: "Dismissed", info: "" };

function actWhen(t) {
  const d = new Date(t * 1000);
  return d.toLocaleTimeString(undefined, { hour: "numeric", minute: "2-digit" });
}
function actDay(t) {
  const d = new Date(t * 1000), today = new Date();
  const y = new Date(); y.setDate(today.getDate() - 1);
  if (d.toDateString() === today.toDateString()) return "Today";
  if (d.toDateString() === y.toDateString()) return "Yesterday";
  return d.toLocaleDateString(undefined, { weekday: "short", month: "short", day: "numeric", year: d.getFullYear() === today.getFullYear() ? undefined : "numeric" });
}

// One action, as a row in Activity or a card in the chat.
function actionHTML(a, compact) {
  const who = a.actor === "assistant" ? "Assistant" : "You";
  const buttons = a.status === "pending"
    ? `<button type="button" class="primary" data-act="apply" data-id="${a.id}">Apply</button><button type="button" data-act="dismiss" data-id="${a.id}">Dismiss</button>`
    : a.undoable && a.status === "done" ? `<button type="button" data-act="undo" data-id="${a.id}">Undo</button>`
    : a.undoable && a.status === "undone" ? `<button type="button" data-act="redo" data-id="${a.id}">Redo</button>` : "";
  const lines = (a.lines || []).filter(Boolean);
  const status = a.status === "info" ? "Can't be undone here" : STATUS_TEXT[a.status] || "";
  const many = lines.length > 3;
  return `<div class="act ${esc(a.status)} ${a.actor === "assistant" ? "by-ai" : "by-user"}${compact ? " compact" : ""}" data-action="${a.id}">
    ${compact ? "" : `<div class="act-who" title="${who}">${ACT_ICONS[a.actor] || ""}</div>`}
    <div class="act-body">
      <div class="act-top"><b>${esc(a.summary)}</b>${status ? `<span class="act-status">${esc(status)}</span>` : ""}</div>
      ${compact ? "" : `<div class="act-meta">${who} · ${actWhen(a.created)}${a.status === "undone" ? ` · undone ${actWhen(a.updated)}` : ""}</div>`}
      ${lines.length ? `<ul class="act-lines${many ? " clip" : ""}">${lines.map((l) => `<li>${esc(l)}</li>`).join("")}</ul>${many ? `<button type="button" class="link act-more">Show all ${lines.length}</button>` : ""}` : ""}
      ${buttons ? `<div class="act-buttons">${buttons}</div>` : ""}
    </div>
  </div>`;
}

async function actionDo(act, id) {
  const go = (force) => api(`/api/actions/${id}/${act}`, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ force: !!force }) });
  try {
    let a;
    try { a = await go(false); }
    catch (e) {
      if (!/changed again/.test(e.message)) throw e;
      if (!confirm(`${t(e.message)}.\n\n${t(act === "undo" ? "Undo anyway?" : "Redo anyway?")}`)) return null;
      a = await go(true);
    }
    toast({ apply: "Applied", dismiss: "Dismissed", undo: "Undone", redo: "Redone" }[act]);
    if (typeof refresh === "function") await refresh();
    return a;
  } catch (e) { toast(e.message); return null; }
}

// Buttons work the same wherever the action is shown.
document.addEventListener("click", async (e) => {
  const more = e.target.closest(".act-more");
  if (more) { more.previousElementSibling.classList.remove("clip"); more.remove(); return; }
  const b = e.target.closest("[data-act]");
  if (!b || !b.closest(".act")) return;
  b.disabled = true;
  const a = await actionDo(b.dataset.act, b.dataset.id);
  b.disabled = false;
  if (!a) return;
  $$(`.act[data-action="${a.id}"]`).forEach((el) => { el.outerHTML = actionHTML(a, el.classList.contains("compact")); });
  if ($("#activity").classList.contains("on")) renderActivity();
});

async function renderActivity() {
  const box = $("#activity-list");
  if (!box) return;
  $$("#activity-filter button").forEach((b) => b.classList.toggle("on", b.dataset.actor === activityActor));
  let list = [];
  try { list = await api("/api/actions" + (activityActor ? `?actor=${activityActor}` : "")); }
  catch (e) { box.innerHTML = `<p class="warn">${esc(e.message)}</p>`; return; }
  if (!list.length) {
    box.innerHTML = `<p class="muted act-empty">${activityActor === "assistant" ? "The assistant hasn't changed anything yet." : "No changes yet."}</p>`;
    return;
  }
  let html = "", lastDay = "";
  for (const a of list) {
    const d = actDay(a.created);
    if (d !== lastDay) { html += `<h3 class="act-day">${esc(d)}</h3>`; lastDay = d; }
    html += actionHTML(a, false);
  }
  box.innerHTML = html;
}

$("#activity-filter")?.addEventListener("click", (e) => {
  const b = e.target.closest("button");
  if (!b) return;
  activityActor = b.dataset.actor;
  try { localStorage.setItem("activityActor", activityActor); } catch {}
  renderActivity();
});
window.addEventListener("hashchange", () => { if (location.hash === "#activity") renderActivity(); });
if (location.hash === "#activity") renderActivity();
