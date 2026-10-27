"use strict";
// Plain JS, no dependencies. Everything talks only to this server.

const $ = (s, el = document) => el.querySelector(s);
const $$ = (s, el = document) => [...el.querySelectorAll(s)];
const esc = (s) => String(s ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c]);
// Display preferences, kept in this browser: dollars with or without cents
// (cents are dropped, never rounded), and bitcoin as BTC, sats or the
// bitcoin sign (sats written like dollars: ₿123,456).
const prefs = (() => {
  const p = { cents: true, unit: "btc", colors: "standard" };
  try { const s = JSON.parse(localStorage.getItem("displayPrefs") || "{}"); if (s.cents === false) p.cents = false; if (["btc", "sats", "bitcoin"].includes(s.unit)) p.unit = s.unit; if (s.colors === "cb") p.colors = "cb"; } catch {}
  return p;
})();
const UNIT_NAME = { btc: "BTC", sats: "Sats", bitcoin: "₿" };
const groupInt = (n) => n.toLocaleString("en-US", { maximumFractionDigits: 0 });
// btc(): the number alone, for columns headed with the unit.
const btc = (sats) => {
  const neg = sats < 0 ? "-" : "", a = Math.abs(sats);
  if (prefs.unit === "btc") return neg + (a / 1e8).toFixed(8);
  return neg + (prefs.unit === "bitcoin" ? "₿" : "") + groupInt(a);
};
// amt(): the number with its unit, for prose.
const amt = (sats) => prefs.unit === "btc" ? btc(sats) + " BTC" : prefs.unit === "sats" ? btc(sats) + (Math.abs(sats) === 1 ? " sat" : " sats") : btc(sats);
// Short axis labels: 0.25, 25M, ₿25M.
const axisAmt = (sats) => {
  if (prefs.unit === "btc") return String(+(sats / 1e8).toFixed(4));
  const k = sats >= 1e6 ? +(sats / 1e6).toFixed(2) + "M" : sats >= 1e3 ? +(sats / 1e3).toFixed(1) + "k" : String(sats);
  return (prefs.unit === "bitcoin" ? "₿" : "") + k;
};
const usd = (cents) => {
  const a = Math.abs(cents);
  if (prefs.cents) return (cents < 0 ? "-$" : "$") + (a / 100).toLocaleString("en-US", { minimumFractionDigits: 2, maximumFractionDigits: 2 });
  const whole = Math.trunc(a / 100); // drop the cents, don't round
  return (cents < 0 && whole ? "-$" : "$") + groupInt(whole);
};
function setUnitLabels() { $$(".unit-name").forEach((el) => (el.textContent = UNIT_NAME[prefs.unit])); }
const signed = (cents) => `<span class="${cents < 0 ? "neg" : cents > 0 ? "pos" : ""}">${usd(cents)}</span>`;
const cssVar = (name) => getComputedStyle(document.documentElement).getPropertyValue(name).trim() || "#888"; // theme color, for canvas drawing
// Dates follow the time zone in Settings, like the tax years do.
const tzName = () => report?.settings?.timezone || "UTC";
const dayFmt = {};
const day = (t) => {
  const z = tzName();
  dayFmt[z] ||= new Intl.DateTimeFormat("en-CA", { timeZone: z, year: "numeric", month: "2-digit", day: "2-digit" });
  return dayFmt[z].format(new Date(typeof t === "number" ? t * 1000 : t));
};

let report = null;

async function api(path, opts = {}) {
  const r = await fetch(path, opts);
  if (r.status === 401 && !path.startsWith("/api/auth/")) { location.href = "/login"; throw new Error("Please log in"); } // session ended
  const body = r.headers.get("content-type")?.includes("json") ? await r.json() : await r.text();
  if (!r.ok) throw new Error(body?.error || r.statusText);
  return body;
}
const post = (path, data) => api(path, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(data) });

// Two fingers on a chart zoom the chart, not the page. onStart(x) runs as
// the second finger lands, onMove(k, x) as they move: k is how much the
// shown span grows (below 1 zooms in), x the client x between the fingers.
function pinchZoom(el, onStart, onMove) {
  const pts = new Map();
  let base = 0;
  const two = () => [...pts.values()];
  const gap = () => { const [a, b] = two(); return Math.hypot(a.x - b.x, a.y - b.y); };
  const mid = () => { const [a, b] = two(); return (a.x + b.x) / 2; };
  el.addEventListener("pointerdown", (e) => {
    if (e.pointerType !== "touch") return;
    // A new gesture's first finger: forget any finger whose lift was never
    // seen, or two fingers would count as three and pinch never start again.
    if (e.isPrimary) pts.clear();
    // Hold the finger on the chart itself: the shape under it may be
    // redrawn away, and Safari stops sending a touch's events once its
    // target leaves the page.
    try { el.setPointerCapture(e.pointerId); } catch {}
    pts.set(e.pointerId, { x: e.clientX, y: e.clientY });
    if (pts.size === 2) { base = Math.max(gap(), 1); onStart(mid()); }
  });
  window.addEventListener("pointermove", (e) => {
    if (!pts.has(e.pointerId)) return;
    pts.set(e.pointerId, { x: e.clientX, y: e.clientY });
    if (pts.size === 2 && base) onMove(base / Math.max(gap(), 1), mid());
  });
  const up = (e) => { pts.delete(e.pointerId); if (pts.size < 2) base = 0; };
  window.addEventListener("pointerup", up);
  window.addEventListener("pointercancel", up);
  el.addEventListener("lostpointercapture", up);
  // Safari zooms the page from its own gesture and touch events.
  el.addEventListener("gesturestart", (e) => e.preventDefault());
  el.addEventListener("touchmove", (e) => { if (e.touches.length > 1) e.preventDefault(); }, { passive: false });
}

// ⓘ: a short note's longer explanation, on hover, or on tap (stays open
// until a tap elsewhere, so its text can be selected).
const info = (html) => `<span class="info"><button type="button" class="info-btn" aria-label="More">ⓘ</button><span class="info-tip" role="tooltip">${html}</span></span>`;
document.addEventListener("click", (e) => {
  const btn = e.target.closest(".info-btn"), mine = e.target.closest(".info");
  $$(".info.open").forEach((x) => { if (x !== mine) x.classList.remove("open"); });
  if (btn) { e.preventDefault(); btn.parentElement.classList.toggle("open"); }
});

function toast(msg) {
  const t = $("#toast");
  t.textContent = msg;
  t.hidden = false;
  clearTimeout(toast.timer);
  toast.timer = setTimeout(() => (t.hidden = true), 4000);
}

function show(route) {
  const [tab, ...rest] = route.split("/");
  // The record popup is modal: left open over a page that's gone, it would
  // leave everything unclickable.
  if ($("#graph-detail")?.open) $("#graph-detail").close();
  const arg = decodeURIComponent(rest.join("/"));
  if (tab === "tx" || tab === "addr") { showLookup(tab, arg); return; }
  $$(".tab").forEach((s) => s.classList.toggle("on", s.id === tab));
  $$("#tabs button").forEach((b) => b.classList.toggle("on", b.dataset.tab === tab));
  // Settings lives in the header, by the theme toggle, and lights up the same way.
  $("#settings-link").classList.toggle("on", tab === "settings");
  if (tab === "settings") $("#settings-link").setAttribute("aria-current", "page"); else $("#settings-link").removeAttribute("aria-current");
  $("#tab-title").textContent = $(`#tabs button[data-tab="${tab}"] .lbl`)?.textContent || $(`#${CSS.escape(tab)}`)?.dataset.title || "";
  document.body.dataset.tab = tab;
  document.body.classList.toggle("flow", tab === "graph");
  scrollTo(0, 0); // the section's id matches the hash, so the browser scrolled it under the header
  requestAnimationFrame(() => scrollTo(0, 0));
  if (tab === "overview" && report) { fitHero(); drawHistory(); if (typeof drawSummary === "function") drawSummary(); }
  if (tab === "graph") drawGraph();
}
function showLookup(kind, arg) {
  $$(".tab").forEach((s) => s.classList.toggle("on", s.id === "lookup"));
  $$("#tabs button").forEach((b) => b.classList.remove("on"));
  $("#tab-title").textContent = kind === "tx" ? "Record" : "Address";
  document.body.dataset.tab = "lookup";
  document.body.classList.remove("flow");
  lookupRoute = [kind, arg];
  renderLookup();
}
let lookupRoute = null;

// Search: a txid, an address, or a reference from an export (R-...).
// Answers come from your own records, then from your own Fulcrum server;
// nothing else is contacted.
$("#search").addEventListener("submit", (e) => {
  e.preventDefault();
  const q = e.target.q.value.trim();
  if (!q) return;
  const byRef = /^R-[0-9A-F]{10}$/i.test(q) && (report?.graph || []).find((g) => g.ref === q.toUpperCase());
  if (byRef) location.hash = pageLink(byRef);
  else if (/^[0-9a-fA-F]{64}$/.test(q)) location.hash = "tx/" + q.toLowerCase();
  else if (/^(bc1|tb1|bcrt1|[13mn2])[0-9A-Za-z]{20,90}$/.test(q)) location.hash = "addr/" + encodeURIComponent(q);
  else toast("Enter a txid, an address or an R- reference.");
});

// Theme: light or dark, remembered; without a choice it follows the OS.
// Charts read their colors from CSS, so they are redrawn on a change.
function applyTheme(t, remember) {
  document.documentElement.dataset.theme = t;
  $("#theme-toggle").setAttribute("aria-label", t === "dark" ? "Switch to light theme" : "Switch to dark theme");
  if (remember) try { localStorage.setItem("theme", t); } catch {}
  if (!report) return;
  if ($("#overview").classList.contains("on")) { drawHistory(); if (typeof drawSummary === "function") drawSummary(); }
  if ($("#graph").classList.contains("on")) { if (typeof tgRebuild === "function" && flowMode === "custody") tgRebuild(); else drawGraph(); }
  if (selected && lastFlow) { const e = (report.graph || []).find((g) => g.key === selected); if (e) drawTxFlow(e, lastFlow); }
}
$("#theme-toggle").addEventListener("click", () => applyTheme(document.documentElement.dataset.theme === "dark" ? "light" : "dark", true));
matchMedia("(prefers-color-scheme: dark)").addEventListener("change", (e) => {
  let saved = null; try { saved = localStorage.getItem("theme"); } catch {}
  if (saved !== "light" && saved !== "dark") applyTheme(e.matches ? "dark" : "light", false);
});
$("#tabs").addEventListener("click", (e) => {
  const b = e.target.closest("button");
  if (b) location.hash = b.dataset.tab;
});

// Fulcrum status dot: green when the server answers, red when it does not.
async function checkFulcrum(fresh) {
  let st;
  try { st = await api("/api/fulcrum/status" + (fresh ? "?fresh=1" : "")); } catch (e) { st = { ok: false, error: e.message }; }
  const notSet = !st.ok && /address first/.test(st.error || "");
  $("#fulcrum-dot").classList.toggle("up", !!st.ok);
  $("#fulcrum-dot").classList.toggle("down", !st.ok && !notSet);
  const line = st.ok ? `Connected, block ${st.height.toLocaleString("en-US")}` : notSet ? "Not set up yet" : `Not reachable: ${st.error}`;
  $("#fulcrum-status").title = "Fulcrum: " + line + ". Click to check again.";
  $("#fulcrum-status").setAttribute("aria-label", "Fulcrum: " + line);
}
$("#fulcrum-status").addEventListener("click", () => { $("#fulcrum-status").title = "Fulcrum: checking…"; checkFulcrum(true); });
setInterval(() => { if (!document.hidden) checkFulcrum(); }, 60000);
window.addEventListener("hashchange", () => show(location.hash.slice(1) || "overview"));
window.addEventListener("load", () => scrollTo(0, 0)); // the browser's own anchor scroll lands after the page is shown

async function refresh() {
  try {
    report = await api("/api/report");
  } catch (e) {
    toast(e.message);
    return;
  }
  // One broken section must not stop the others from drawing.
  for (const f of [renderOverview, renderWallets, renderReview, renderTaxes, renderImports, renderLightning]) {
    try { await f(); } catch (e) { console.error(e); toast(`Could not draw the page: ${e.message}`); }
  }
  if ($("#graph").classList.contains("on")) drawGraph();
  if ($("#lookup").classList.contains("on")) renderLookup();
  showDemo();
  showDisclaimer();
}

// The disclaimer, once per install: it has to be accepted before first use,
// and stays on its page, linked from the footer.
function showDemo() {
  const on = report?.settings?.demo === "true";
  document.body.classList.toggle("demo", on);
  if (on) {
    $("#demo-ai-note").textContent = report.settings.demoAllowance
      ? `In the demo, the assistant runs on ${report.settings.demoModel}, up to ${report.settings.demoAllowance}. Your own copy uses a model on your own Ollama server.`
      : "The assistant is off in this demo. Your own copy uses a model on your own Ollama server.";
  }
}
$("#demo-reset").addEventListener("click", async () => {
  if (!confirm(t("Start over with a fresh copy of the demo? Your changes here are dropped."))) return;
  try { await post("/api/demo/reset", {}); } catch {}
  location.hash = "";
  location.reload();
});

function showDisclaimer() {
  const box = $("#disclaimer-pop");
  if (report?.settings?.disclaimerAccepted || box.open) return;
  $(".disclaimer-body", box).innerHTML = $("#disclaimer .disclaimer-text").innerHTML;
  box.showModal();
  // Open at the top (focus is on the heading), so it reads down to the button.
  box.scrollTop = 0;
}
$("#disclaimer-pop").addEventListener("cancel", (e) => e.preventDefault()); // Escape doesn't dismiss it
$("#disclaimer-ok").addEventListener("click", async () => {
  try {
    await post("/api/disclaimer", {});
    report.settings.disclaimerAccepted = new Date().toISOString();
    $("#disclaimer-pop").close();
  } catch (err) { toast(err.message); }
});

let pickedYear = (() => { try { return Number(localStorage.getItem("year")) || 0; } catch { return 0; } })();
$("#year-select").addEventListener("change", (e) => {
  pickedYear = Number(e.target.value);
  try { localStorage.setItem("year", pickedYear); } catch {}
  renderOverview();
});

let heroBTC = (() => { try { return localStorage.getItem("heroFirst") === "btc"; } catch { return false; } })();

// fitHero shrinks the big amount to fit its row when it's too wide (a long
// sats or BTC figure on a phone) instead of letting it wrap.
function fitHero() {
  const w = $("#hero .worth");
  if (!w) return;
  w.style.fontSize = "";
  const row = w.parentElement, cs = getComputedStyle(row);
  const room = row.clientWidth - parseFloat(cs.paddingLeft) - parseFloat(cs.paddingRight);
  if (room > 0 && w.scrollWidth > room) w.style.fontSize = Math.floor(parseFloat(getComputedStyle(w).fontSize) * room / w.scrollWidth) + "px";
}
window.addEventListener("resize", fitHero);
document.fonts?.ready.then(fitHero); // measured again once the display font is in

function renderOverview() {
  const r = report;
  const thisYear = new Date().getFullYear();
  const active = (r.graph || []).map((e) => new Date(e.t * 1000).getUTCFullYear()); // any year with activity
  const years = [...new Set([thisYear, ...(r.years || []).map((y) => y.year), ...active])].sort((a, b) => b - a);
  const yr = years.includes(pickedYear) ? pickedYear : thisYear;
  $("#year-select").innerHTML = years.map((y) => `<option value="${y}"${y === yr ? " selected" : ""}>${y}${y === thisYear ? " (this year)" : ""}</option>`).join("");
  const ys = (r.years || []).find((y) => y.year === yr);
  const worth = r.price ? Math.round(r.holdingsSats * r.price / 1e8) : 0;
  // The swap button puts dollars or bitcoin first; the choice is remembered.
  const swapTip = heroBTC ? "Show dollars first" : "Show bitcoin first";
  const swap = `<button type="button" id="hero-swap" class="hero-swap" title="${swapTip}" aria-label="${swapTip}"><svg viewBox="0 0 24 24" aria-hidden="true"><path d="M7 4v14m0 0-3-3m3 3 3-3M17 20V6m0 0-3 3m3-3 3 3"/></svg></button>`;
  // The unit (BTC, sats) is set smaller so the number carries the size.
  const big = (v, label) => `<div class="worth-row"><div class="worth">${v.replace(/ (BTC|sats?)$/, ' <span class="unit">$1</span>')}</div>${r.price ? swap : ""}</div><div class="label">${label}</div>`;
  const priceLine = `${usd(r.price)} per BTC <span class="muted">(${esc(r.priceDay)})</span>`;
  $("#hero").innerHTML = r.price
    ? (heroBTC ? big(amt(r.holdingsSats), "Bitcoin held") + `<div class="held">${usd(worth)} · ${priceLine}</div>`
               : big(usd(worth), "Portfolio value") + `<div class="held">${amt(r.holdingsSats)} · ${priceLine}</div>`)
      + `<div class="gain ${r.unrealized < 0 ? "neg" : "pos"}">${r.unrealized >= 0 ? "+" : "-"}${usd(Math.abs(r.unrealized))} unrealized</div>`
    : big(amt(r.holdingsSats), "Bitcoin held") + `<div class="held">No price · <a href="#settings">Update</a></div>`;
  $("#hero-swap")?.addEventListener("click", () => {
    heroBTC = !heroBTC;
    try { localStorage.setItem("heroFirst", heroBTC ? "btc" : "usd"); } catch {}
    renderOverview();
  });
  fitHero();
  const tiles = [
    [`${yr} realized`, ys ? usd(ys.shortProceeds - ys.shortBasis + ys.longProceeds - ys.longBasis) : usd(0)],
    [`${yr} est. tax`, ys ? usd(ys.estimatedTax) : usd(0)],
  ];
  $("#tiles").innerHTML = tiles.map(([k, v, sub]) => `<div class="tile${/tax/.test(k) ? " tax" : ""}"><div class="k">${esc(k)}</div><div class="v">${esc(v)}</div>${sub ? `<div class="sub">${esc(sub)}</div>` : ""}</div>`).join("");
  const held = (r.accounts || []).filter((a) => a.sats > 0 && a.kind !== "pool").sort((a, b) => b.sats - a.sats);
  const cur = $("#whatif-account").value;
  $("#whatif-account").innerHTML = held.map((a) => `<option value="${esc(a.name)}"${a.name === cur ? " selected" : ""}>${esc(a.name)} (${amt(a.sats)})</option>`).join("") || `<option value="">No BTC held</option>`;
  // Unlabeled items haven't moved coins yet, so an exchange's traced balance
  // can sit above what the exchange shows by exactly the unlabeled sends.
  const pend = (a) => (a.pendingOut || a.pendingIn)
    ? `<span class="warn" title="${amt(a.sats - (a.pendingOut || 0) + (a.pendingIn || 0))} once Review is done">${a.pendingOut ? `−${btc(a.pendingOut)}` : ""}${a.pendingOut && a.pendingIn ? " " : ""}${a.pendingIn ? `+${btc(a.pendingIn)}` : ""}</span><div class="small muted">→ ${btc(a.sats - (a.pendingOut || 0) + (a.pendingIn || 0))}</div>`
    : `<span class="muted">–</span>`;
  $("#accounts-table tbody").innerHTML = (r.accounts || []).map((a) => `<tr>
    <td>${esc(a.name)}</td><td>${esc(a.kind)}</td><td class="num">${btc(a.sats)}</td>
    <td class="num">${a.chainSats == null ? "" : btc(a.chainSats)}${a.chainSats != null && a.chainSats !== a.sats ? ' <span class="warn" title="Doesn&#39;t match chain">!</span>' : ""}</td>
    <td class="num">${usd(a.basis)}</td><td class="num">${pend(a)}</td></tr>`).join("") || `<tr><td colspan="6" class="muted">No accounts yet.</td></tr>`;
  const nLabel = r.unmatched?.length || 0;
  // Where each account first went below zero: what it spent before then
  // never came in, so the missing or misread record is just before this one.
  const dips = (r.dips || []).map((d) => {
    const g = (r.graph || []).find((x) => x.key === d.key);
    const what = !g ? d.key : g.kind === "transfer" && g.account === d.account ? `moved ${amt(g.sats)} to ${g.to}` : `${KIND_NAMES[g.label || g.kind] || g.kind} ${amt(g.sats)}`;
    const out = d.beforeSats - d.balanceSats;
    let why = `It had ${amt(d.beforeSats)} counted coming in before this, and this took ${amt(out)}.`;
    if (d.chainSats != null) {
      why += ` On chain it really held ${amt(d.chainSats)} just before.`;
      if (d.chainSats < out) why += ` That isn't enough either, so this send also spent coins from an address none of your wallets cover: open it, and add the inputs with no wallet name if they're yours.`;
      else if (d.chainSats > d.beforeSats) why += ` So ${amt(d.chainSats - d.beforeSats)} that reached it on chain was never counted in.`;
    }
    if (d.uncounted.length) why += ` ${d.uncounted.length} incoming record${d.uncounted.length === 1 ? "" : "s"} for it up to then (${amt(d.uncountedSats)}) ${d.uncounted.length === 1 ? "is" : "are"} still in Review and count${d.uncounted.length === 1 ? "s" : ""} for nothing until labeled: <a href="#review">label ${d.uncounted.length === 1 ? "it" : "them"}</a>.`;
    else if (d.chainSats != null && d.chainSats > d.beforeSats) why += ` None are waiting in Review, so check how its earlier incoming records are labeled or linked on Flow.`;
    else if (d.chainSats == null) why += ` Look for a purchase, deposit or conversion into it that is missing or misread just before.`;
    const unl = d.uncounted.length ? ` · <a href="#review">${d.uncounted.length} unlabeled incoming</a>` : "";
    return `<div class="warn"><b>${esc(d.account)} below zero on ${day(d.t)}</b>${unl} · <a href="${recordLink(d.key)}" data-flowpick="${esc(d.key)}">Open it</a> ${info(`At ${esc(what)}, balance ${amt(d.balanceSats)}. ${why}`)}</div>`;
  }).join("");
  $("#warnings").innerHTML = (r.warnings || []).length || nLabel || dips
    ? `<h2>Warnings</h2><div class="card small">${dips}${nLabel ? `<div class="warn"><b>${nLabel} unlabeled</b> · <a href="#review">Review</a></div>` : ""}${(r.warnings || []).map((w) => `<div class="warn">${esc(w)}</div>`).join("")}</div>` : "";
  const n = r.unmatched?.length || 0;
  $("#review-count").hidden = !n;
  $("#review-count").textContent = n;
  $("#setup-hint").hidden = !!r.settings.fulcrumHost;
  renderIdents();
  drawHistory();
  if (typeof drawSummary === "function") drawSummary();
}

// What would I owe: a sale today from one account, at the latest price,
// with the chosen method and protected purchases honoured. Nothing is saved.
// Plan a sale or spend: the coins it would use, the gain, the tax impact,
// and a way to record that choice before it happens.
let lastPlan = null;
$("#whatif-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const f = new FormData(e.target), n = Number(String(f.get("amount")).replace(/[$,\s]/g, ""));
  const out = $("#whatif-out");
  try {
    const r = await post("/api/whatif", { account: f.get("account"), [f.get("unit")]: n, strategy: f.get("strategy") || "" });
    lastPlan = r;
    const g = (s) => s.proceeds - s.basis;
    const tile = (k, v, sub) => `<div class="tile"><div class="k">${k}</div><div class="v">${v}</div>${sub ? `<div class="sub">${sub}</div>` : ""}</div>`;
    const soon = (r.lots || []).filter((l) => l.longTermIn > 0).sort((a, b) => a.longTermIn - b.longTermIn);
    out.innerHTML = `<div class="whatif-grid">
      ${tile("Selling", `${amt(r.sats)}`, `for ${usd(r.proceeds)} at ${usd(r.price)}`)}
      ${tile("Short-term gain", signed(g(r.short)), r.short.sats ? `${amt(r.short.sats)} held a year or less` : "none")}
      ${tile("Long-term gain", signed(g(r.long)), r.long.sats ? `${amt(r.long.sats)} held over a year` : "none")}
      ${tile("Tax impact this year", usd(Math.max(r.extraTax, 0)), r.extraTax < 0 ? `lowers this year's estimate by ${usd(-r.extraTax)}` : `at your rates, after this year's other results`)}
    </div>
    ${soon.length ? `<p class="small">${amt(soon[0].sats)} of these coins turn long-term in ${soon[0].longTermIn} day${soon[0].longTermIn === 1 ? "" : "s"}${soon.length > 1 ? `; all the short-term ones within ${soon.at(-1).longTermIn} days` : ""}. Waiting would tax that part at the long-term rate.</p>` : ""}
    <details><summary class="small">The coins this would use (${(r.lots || []).length})</summary>
      <div class="scroll"><table class="small"><thead><tr><th>Bought</th><th class="num"><span class="unit-name">BTC</span></th><th class="num">Basis</th><th class="num">Gain</th><th>Held</th></tr></thead><tbody>
      ${(r.lots || []).map((l) => `<tr><td>${l.missing ? '<span class="warn">unknown</span>' : day(l.acquired)}</td><td class="num">${btc(l.sats)}</td><td class="num">${usd(l.basis)}</td><td class="num">${signed(l.proceeds - l.basis)}</td>
        <td>${l.longTerm ? "long" : `short${l.longTermIn ? ` (long in ${l.longTermIn} d)` : ""}`}</td></tr>`).join("")}</tbody></table></div></details>
    <p class="small muted">${r.strategy ? esc(r.strategy.replaceAll("_", " ")) : esc(r.method.toUpperCase())} · price ${esc(r.priceDay)} · estimate ${info("Protected purchases are skipped unless nothing else is left. For planning only; your tax professional prepares the return.")}</p>`
      + (r.missingSats ? `<p class="warn small">${amt(r.missingSats)} has no known purchase ($0 basis).</p>` : "")
      + (r.warnings || []).map((w) => `<p class="warn small">${esc(w)}</p>`).join("")
      + ((r.lots || []).some((l) => !l.missing) ? `<div class="row"><input id="ident-note" placeholder="Note (optional): what this is for" size="28">
        <button type="button" id="ident-record" class="primary">Record this choice</button></div>
        <p class="small muted">Dated record of these coins (IRS FAQ 82). ${info("Made before the sale, as the IRS asks. When the sale shows up, these are the coins counted.")}</p>` : "");
  } catch (err) { out.innerHTML = `<p class="warn">${esc(err.message)}</p>`; }
});
$("#whatif-out").addEventListener("click", async (e) => {
  if (e.target.id !== "ident-record" || !lastPlan) return;
  const picks = lastPlan.lots.filter((l) => !l.missing).map((l) => ({ originId: l.originId, sats: l.sats }));
  try {
    await post("/api/identifications", { Account: lastPlan.account, Strategy: lastPlan.strategy || lastPlan.method, Note: $("#ident-note").value, Picks: picks,
      Estimate: { price: lastPlan.price, proceeds: lastPlan.proceeds, basis: lastPlan.short.basis + lastPlan.long.basis, shortSats: lastPlan.short.sats, longSats: lastPlan.long.sats, extraTax: lastPlan.extraTax } });
    toast("Choice recorded");
    e.target.disabled = true;
    refresh();
  } catch (err) { toast(err.message); }
});
// Recorded choices and what became of them.
function renderIdents() {
  const ids = report.identifications || [];
  const status = { waiting: "Waiting for the sale", applied: "Used for", late: "Recorded after the sale: not used", cancelled: "Cancelled" };
  $("#idents").innerHTML = ids.length ? `<h3>Recorded coin choices</h3><div class="scroll"><table class="small"><thead><tr><th>Recorded</th><th>From</th><th class="num"><span class="unit-name">BTC</span></th><th>Status</th><th></th></tr></thead><tbody>
    ${[...ids].reverse().map((i) => `<tr><td>${esc(when(Math.floor(new Date(i.createdAt) / 1000)))}</td><td>${esc(i.account)}</td><td class="num">${btc(i.sats)}</td>
      <td class="${i.status === "late" || i.warning ? "warn" : ""}">${esc(status[i.status] || i.status)}${i.appliedTo ? ` <a href="${recordLink(i.appliedTo)}" data-flowpick="${esc(i.appliedTo)}">the sale</a>` : ""}${i.note ? `<br><span class="muted">${esc(i.note)}</span>` : ""}${i.warning ? `<br>${esc(i.warning)}` : ""}</td>
      <td>${i.status === "waiting" ? `<button class="link small ident-cancel" data-id="${i.id}">Cancel</button>` : ""}</td></tr>`).join("")}</tbody></table></div>` : "";
}
$("#idents").addEventListener("click", async (e) => {
  const id = e.target.closest(".ident-cancel")?.dataset.id;
  if (!id) return;
  try { await post(`/api/identifications/${id}`, { Cancelled: true }); refresh(); } catch (err) { toast(err.message); }
});

// Lightning: the node as a checking account. Each bookkeeper event is a
// ledger line; the running balance is worked back from the node's current
// balance. Payments count as spends and receipts as income unless labeled.
const LN_WHAT = { deposit: "On-chain deposit", withdrawal: "On-chain withdrawal", onchain_fee: "On-chain fee", invoice_fee: "Routing fee paid",
  routed: "Routing fees earned", lease_fee: "Liquidity lease fee", rebalance_fee: "Rebalance fee" };
function lnLedger() {
  const ln = report.lightning;
  if (!ln) return [];
  const byKey = Object.fromEntries((report.graph || []).map((g) => [g.key, g]));
  const linkedTo = {};
  for (const l of report.links || []) { linkedTo[l.From] = l.To; linkedTo[l.To] = l.From; }
  const rows = ln.events.map((e) => {
    const key = "ln:" + e.uid, g = byKey[key];
    const what = e.tag === "invoice" ? (e.credit ? "Received" : "Paid") + (e.description ? `: ${e.description}` : "") : LN_WHAT[e.tag] || e.tag;
    let counts = "";
    if (linkedTo[key]) counts = `Self transfer with ${esc(linkedTo[key].startsWith("ex:") ? "an exchange" : "your wallet")}`;
    else if (g?.label) counts = `${esc(KIND_NAMES[g.label] || g.label)}${g.auto ? ' <span class="muted">(automatic)</span>' : ""}`;
    else if (g?.kind === "unmatched") counts = '<span class="warn">Needs a label</span>';
    else if (e.tag === "routed") counts = "Income";
    else if (e.tag.endsWith("fee")) counts = "Fee";
    return { e, key, g, what, counts, delta: e.credit - e.debit };
  });
  let bal = ln.balanceSats;
  for (let i = rows.length - 1; i >= 0; i--) { rows[i].balance = bal; bal -= rows[i].delta; }
  return rows.reverse(); // newest first
}
function renderLightning() {
  const ln = report.lightning;
  const nodeCard = $("#node-card");
  $('#tabs button[data-tab="lightning"]').hidden = !ln;
  $("#node-card-wrap").hidden = !ln;
  if (!ln) {
    nodeCard.innerHTML = "";
    $("#ln-head").innerHTML = `<div class="card">No Lightning node connected. Add your Core Lightning node in <a href="#settings">Settings</a>; it's optional.</div>`;
    $("#ln-table tbody").innerHTML = ""; $("#ln-channels tbody").innerHTML = "";
    return;
  }
  const worth = report.price ? usd(Math.round(ln.balanceSats * report.price / 1e8)) : "";
  const rows = lnLedger();
  const line = (r) => `<tr><td>${day(r.e.t)}</td><td>${r.g ? `<a href="${pageLink(r.g)}" data-flowpick="${esc(r.g.key)}">${esc(r.what)}</a>` : esc(r.what)}</td>
    <td class="num in">${r.delta > 0 ? btc(r.delta) : ""}</td><td class="num out">${r.delta < 0 ? btc(-r.delta) : ""}</td><td class="num">${btc(r.balance)}</td><td class="small">${r.counts}</td></tr>`;
  const head = `<div class="bal"><div><div class="small muted">Balance</div><div class="big">${amt(ln.balanceSats)}</div></div>${worth ? `<div><div class="small muted">Worth</div><div class="big">${worth}</div></div>` : ""}
    <div class="small muted">${ln.alias ? esc(ln.alias) + " · " : ""}${ln.synced ? `synced ${esc(ln.synced.replace("T", " ").slice(0, 16))} UTC` : "not synced yet"}</div></div>`;
  $("#node-card-title").textContent = ln.account;
  nodeCard.innerHTML = `<div class="node-card">${head}
    <div class="scroll"><table class="ledger"><thead><tr><th>Date</th><th>What</th><th class="num">In</th><th class="num">Out</th><th class="num">Balance</th><th>Counts as</th></tr></thead>
    <tbody>${rows.slice(0, 8).map(line).join("") || `<tr><td colspan="6" class="muted">No activity yet. Sync from Settings.</td></tr>`}</tbody></table></div></div>`;
  // The Lightning tab
  const pay = rows.filter((r) => r.e.tag === "invoice" && r.delta < 0), recv = rows.filter((r) => r.e.tag === "invoice" && r.delta > 0);
  const sum = (a, f) => a.reduce((s, r) => s + f(r), 0);
  const routed = sum(rows.filter((r) => r.e.tag === "routed"), (r) => r.delta), feesPaid = sum(rows.filter((r) => r.e.tag.endsWith("fee") && r.e.tag !== "lease_fee"), (r) => -r.delta);
  const tile = (k, v, sub) => `<div class="tile"><div class="k">${k}</div><div class="v">${v}</div>${sub ? `<div class="sub">${sub}</div>` : ""}</div>`;
  $("#ln-head").innerHTML = `<div class="card node-card"><div class="row"><b>${esc(ln.account)}</b></div>${head}<div class="tiles">
    ${tile("Paid out", `${amt(-sum(pay, (r) => r.delta))}`, `${pay.length} payments`)}${tile("Received", `${amt(sum(recv, (r) => r.delta))}`, `${recv.length} payments`)}
    ${tile("Routing fees earned", `${amt(routed)}`, "counted as income")}${tile("Fees paid", `${amt(feesPaid)}`, "routing and on-chain")}</div></div>`;
  const f = $("#ln-filter").value;
  const shown = rows.filter((r) => !f || (f === "out" && r.e.tag === "invoice" && r.delta < 0) || (f === "in" && r.e.tag === "invoice" && r.delta > 0)
    || (f === "chain" && (r.e.tag === "deposit" || r.e.tag === "withdrawal")) || (f === "fee" && (r.e.tag.endsWith("fee") || r.e.tag === "routed")));
  $("#ln-table tbody").innerHTML = shown.map(line).join("") || `<tr><td colspan="6" class="muted">Nothing here.</td></tr>`;
  $("#ln-channels tbody").innerHTML = ln.channels.map((c) => `<tr><td>${c.account === "wallet" ? "On-chain wallet" : `<span class="mono small">${esc(c.account.slice(0, 16))}…</span>`}${c.closed ? ' <span class="muted">(closed)</span>' : ""}</td>
    <td class="mono small">${c.peerId ? esc(c.peerId.slice(0, 16)) + "…" : ""}</td><td class="num">${btc(c.sats)}</td></tr>`).join("") || `<tr><td colspan="3" class="muted">None.</td></tr>`;
}
$("#ln-filter").addEventListener("change", renderLightning);
async function syncLightning() {
  try {
    const r = await post("/api/lightning/sync", {});
    toast(`Synced ${r.alias || "your node"}: ${r.events} events (${r.added} new)`);
    await loadSettings();
    refresh();
  } catch (e) { toast("Lightning sync failed: " + e.message); }
}
$("#ln-sync").addEventListener("click", syncLightning);
$("#ln-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const f = Object.fromEntries(new FormData(e.target));
  try {
    await post("/api/settings", f);
    e.target.elements.clnRune.value = "";
    await syncLightning();
  } catch (err) { toast(err.message); }
});
$("#ln-forget").addEventListener("click", async () => {
  if (!confirm(t("Disconnect the node and forget its rune? Its synced history stays until you remove it with Wipe everything."))) return;
  await post("/api/settings", { clnForget: "1" });
  toast("Node disconnected; rune forgotten.");
  loadSettings(); refresh();
});

// Overview cards can be dragged into any order by their grip; the order is
// remembered. By default the chart sits above the summary.
const OV_DEFAULT = ["now", "history", "summary", "lightning", "accounts", "whatif"];
function applyCardOrder() {
  let order = OV_DEFAULT;
  try { const saved = JSON.parse(localStorage.getItem("ovOrder") || "null"); if (Array.isArray(saved)) order = [...saved.filter((c) => OV_DEFAULT.includes(c)), ...OV_DEFAULT.filter((c) => !saved.includes(c))]; } catch {}
  const wrap = $("#ov-cards");
  for (const name of order) { const el = $(`.ov-card[data-card="${name}"]`); if (el) wrap.append(el); }
}
applyCardOrder();
(() => {
  let lift = null;
  $("#ov-cards").addEventListener("pointerdown", (e) => {
    const grip = e.target.closest(".ov-grip");
    if (!grip) return;
    e.preventDefault();
    lift = { card: grip.closest(".ov-card"), target: null, after: false };
    lift.card.classList.add("dragging");
    grip.setPointerCapture?.(e.pointerId);
  });
  window.addEventListener("pointermove", (e) => {
    if (!lift) return;
    $$(".ov-card").forEach((c) => c.classList.remove("drop-before", "drop-after"));
    const el = document.elementFromPoint(e.clientX, e.clientY)?.closest(".ov-card");
    if (!el || el === lift.card) { lift.target = null; return; }
    const r = el.getBoundingClientRect();
    lift.target = el; lift.after = e.clientY > r.top + r.height / 2;
    el.classList.add(lift.after ? "drop-after" : "drop-before");
  });
  window.addEventListener("pointerup", () => {
    if (!lift) return;
    $$(".ov-card").forEach((c) => c.classList.remove("drop-before", "drop-after", "dragging"));
    if (lift.target) {
      lift.target[lift.after ? "after" : "before"](lift.card);
      try { localStorage.setItem("ovOrder", JSON.stringify($$(".ov-card").map((c) => c.dataset.card))); } catch {}
      if (report) { drawHistory(); if (typeof drawSummary === "function") drawSummary(); }
    }
    lift = null;
  });
})();

// BTC held over time, with the BTC price in a second panel on the same
// time axis (two scales never share one axis). The range presets, the
// wheel and dragging pick the window; hover reads both lines.
const hist = { t0: 0, t1: 0, preset: "all" };
const DAY = 86400;
function histBounds() {
  const pts = report?.history || [];
  return pts.length ? [pts[0].t, pts.at(-1).t] : [0, 0];
}
function setHistRange(preset) {
  const [a, b] = histBounds();
  hist.preset = preset;
  if (preset === "all") { hist.t0 = a; hist.t1 = b; }
  else { hist.t1 = b; hist.t0 = Math.max(a, b - Number(preset) * DAY); }
  $$("#history-range button").forEach((x) => x.classList.toggle("on", x.dataset.range === preset));
  drawHistory();
}
$("#history-range").addEventListener("click", (e) => { const r = e.target.closest("[data-range]")?.dataset.range; if (r) setHistRange(r); });
// A smooth line through points, monotone in each segment (Fritsch–Carlson),
// so it never overshoots a step in the holdings.
function smoothPath(P) {
  const n = P.length;
  if (n < 3) return P.map((p, i) => `${i ? "L" : "M"}${p[0].toFixed(1)},${p[1].toFixed(1)}`).join("");
  const dx = [], m = [], t = new Array(n).fill(0);
  for (let i = 0; i < n - 1; i++) { dx[i] = Math.max(P[i + 1][0] - P[i][0], 1e-6); m[i] = (P[i + 1][1] - P[i][1]) / dx[i]; }
  t[0] = m[0]; t[n - 1] = m[n - 2];
  for (let i = 1; i < n - 1; i++) t[i] = m[i - 1] * m[i] <= 0 ? 0 : (m[i - 1] + m[i]) / 2;
  for (let i = 0; i < n - 1; i++) {
    if (m[i] === 0) { t[i] = 0; t[i + 1] = 0; continue; }
    const a = t[i] / m[i], b = t[i + 1] / m[i], s = a * a + b * b;
    if (s > 9) { const k = 3 / Math.sqrt(s); t[i] = k * a * m[i]; t[i + 1] = k * b * m[i]; }
  }
  let d = `M${P[0][0].toFixed(1)},${P[0][1].toFixed(1)}`;
  for (let i = 0; i < n - 1; i++) {
    const h = dx[i] / 3;
    d += `C${(P[i][0] + h).toFixed(1)},${(P[i][1] + t[i] * h).toFixed(1)} ${(P[i + 1][0] - h).toFixed(1)},${(P[i + 1][1] - t[i + 1] * h).toFixed(1)} ${P[i + 1][0].toFixed(1)},${P[i + 1][1].toFixed(1)}`;
  }
  return d;
}
const pct = (from, to) => from > 0 ? (to - from) / from * 100 : 0;
const pctPill = (p) => `<span class="tip-pill ${p < 0 ? "neg" : "pos"}">${p >= 0 ? "+" : "−"}${Math.abs(p).toFixed(Math.abs(p) < 10 ? 1 : 0)}%</span>`;
// Pointer drags on the chart and the scrubber are handled at the window, so
// a redraw in the middle of a drag (every move redraws) loses nothing.
hist.drag = null; // {mode: pan|scrub|left|right, x, t0, t1, pxPerSec}
function histDragMove(e) {
  const d = hist.drag; if (!d) return;
  const [a, b] = histBounds(), dt = (e.clientX - d.x) / d.pxPerSec, span = d.t1 - d.t0;
  if (d.mode === "pan") { hist.t0 = Math.min(Math.max(a, d.t0 - dt), b - span); hist.t1 = hist.t0 + span; }
  else if (d.mode === "scrub") { hist.t0 = Math.min(Math.max(a, d.t0 + dt), b - span); hist.t1 = hist.t0 + span; }
  else if (d.mode === "left") hist.t0 = Math.min(Math.max(a, d.t0 + dt), d.t1 - 2 * DAY);
  else if (d.mode === "right") hist.t1 = Math.max(Math.min(b, d.t1 + dt), d.t0 + 2 * DAY);
  d.moved = true;
  hist.preset = ""; $$("#history-range button").forEach((x) => x.classList.remove("on"));
  drawHistory();
}
window.addEventListener("pointermove", histDragMove);
window.addEventListener("pointerup", () => { hist.dragged = !!hist.drag?.moved; hist.drag = null; $("#history")?.classList.remove("dragging"); });
window.addEventListener("pointercancel", () => { hist.drag = null; });

function drawHistory() {
  const svg = $("#history"), all = report.history || [];
  if (all.length < 2) { svg.setAttribute("height", 40); svg.innerHTML = `<text x="8" y="24">Nothing to chart yet.</text>`; return; }
  const [a, b] = histBounds();
  if (!hist.t1 || hist.t1 > b || hist.t0 < a) { hist.t0 = hist.preset === "all" ? a : Math.max(a, b - (Number(hist.preset) || 1e9) * DAY); hist.t1 = b; }
  const t0 = hist.t0, t1 = Math.max(hist.t1, t0 + DAY);
  // Points in view, plus one either side so lines reach the edges.
  let i0 = all.findIndex((p) => p.t >= t0), i1 = all.findIndex((p) => p.t > t1);
  if (i0 < 0) i0 = all.length - 1;
  if (i1 < 0) i1 = all.length;
  const pts = all.slice(Math.max(i0 - 1, 0), Math.min(i1 + 1, all.length));
  const W = Math.max(320, $("#history-wrap").clientWidth), L = 58, R = 16, PW = W - L - R;
  const top1 = 22, h1 = 150, top2 = top1 + h1 + 36, h2 = 150, axisY = top2 + h2 + 18, scrubTop = axisY + 14, scrubH = 36, H = scrubTop + scrubH + 10;
  const x = (t) => L + ((t - t0) / (t1 - t0)) * PW;
  hist.geo = { L, PW };
  const inView = pts.filter((p) => p.t >= t0 && p.t <= t1);
  const maxS = Math.max(...(inView.length ? inView : pts).map((p) => p.sats), 1);
  const priced = pts.filter((p) => p.price > 0), pricedIn = priced.filter((p) => p.t >= t0 && p.t <= t1);
  const span = pricedIn.length ? pricedIn : priced;
  // The price axis fits the window, with a little headroom, so a zoomed-in
  // week isn't a flat line at the bottom.
  const minP0 = Math.min(...span.map((p) => p.price), Infinity), maxP0 = Math.max(...span.map((p) => p.price), 1);
  const pad = Math.max((maxP0 - minP0) * 0.1, maxP0 * 0.02);
  const minP = hist.preset === "all" ? 0 : Math.max(0, minP0 - pad), maxP = maxP0 + (hist.preset === "all" ? 0 : pad);
  const y1 = (s) => top1 + h1 - (s / maxS) * h1, y2 = (c) => top2 + h2 - ((c - minP) / Math.max(maxP - minP, 1)) * h2;
  const nice = (m) => { const e = Math.pow(10, Math.floor(Math.log10(m)) - 1); return [1, 2, 2.5, 5, 10, 20, 25, 50, 100].map((k) => k * e).find((s) => m / s <= 5); };
  let g = "";
  const sStep = nice(maxS / 1e8) * 1e8;
  for (let s = 0; s <= maxS * 1.001; s += sStep) g += `<line class="grid" x1="${L}" x2="${W - R}" y1="${y1(s)}" y2="${y1(s)}"/><text x="${L - 8}" y="${y1(s) + 4}" text-anchor="end">${axisAmt(s)}</text>`;
  const pStep = nice(Math.max(maxP - minP, 100) / 100) * 100;
  for (let c = Math.ceil(minP / pStep) * pStep; c <= maxP * 1.001; c += pStep) g += `<line class="grid" x1="${L}" x2="${W - R}" y1="${y2(c)}" y2="${y2(c)}"/><text x="${L - 8}" y="${y2(c) + 4}" text-anchor="end">$${c >= 100000 ? (c / 100000).toFixed(c % 100000 ? 1 : 0) + "k" : Math.round(c / 100)}</text>`;
  // Time labels suited to the window: years, months or days.
  const days = (t1 - t0) / DAY;
  const d0 = new Date(t0 * 1000);
  let marks = [];
  if (days > 3 * 365) for (let yr = d0.getUTCFullYear() + 1; Date.UTC(yr, 0, 1) / 1000 <= t1; yr++) marks.push([Date.UTC(yr, 0, 1) / 1000, String(yr)]);
  else if (days > 45) for (let m = new Date(Date.UTC(d0.getUTCFullYear(), d0.getUTCMonth() + 1, 1)); m / 1000 <= t1; m.setUTCMonth(m.getUTCMonth() + 1)) marks.push([m / 1000, m.getUTCMonth() === 0 ? String(m.getUTCFullYear()) : m.toLocaleString("en-US", { month: "short", timeZone: "UTC" })]);
  else { const step = days > 10 ? 7 : 1; for (let d = new Date(Date.UTC(d0.getUTCFullYear(), d0.getUTCMonth(), d0.getUTCDate() + 1)); d / 1000 <= t1; d.setUTCDate(d.getUTCDate() + step)) marks.push([d / 1000, d.toISOString().slice(5, 10)]); }
  let lastX = -1e9;
  for (const [t, label] of marks) { const xx = x(t); if (xx - lastX < 36) continue; g += `<text x="${xx}" y="${axisY}" text-anchor="middle">${label}</text>`; lastX = xx; }
  // One sample per day in view (or per 3px when days outnumber pixels):
  // each becomes a thin stroke, and together they carry the smooth line.
  const sampleStep = Math.max(DAY, (t1 - t0) / (PW / 3));
  const sample = (series, val) => {
    const out = []; let k = 0;
    for (let t = t0; t <= t1 + 1; t += sampleStep) {
      while (k + 1 < series.length && series[k + 1].t <= t) k++;
      if (series[k].t > t) continue;
      out.push([x(Math.min(t, t1)), val(series[k]), series[k]]);
    }
    return out;
  };
  const strokes = (S, base) => S.map(([sx, sy]) => sy < base - 0.5 ? `M${sx.toFixed(1)},${sy.toFixed(1)}V${base}` : "").join("");
  const S1 = sample(pts, (p) => y1(p.sats)), S2 = priced.length > 1 ? sample(priced, (p) => y2(p.price)) : [];
  g += `<defs><clipPath id="hclip"><rect x="${L}" y="0" width="${PW}" height="${H}"/></clipPath><clipPath id="hband"><rect id="history-band" x="0" y="0" width="0" height="${H}"/></clipPath></defs><g clip-path="url(#hclip)">`;
  const bars1 = strokes(S1, top1 + h1), bars2 = strokes(S2, top2 + h2);
  g += `<path class="bar" d="${bars1}"/><path class="bar hot" clip-path="url(#hband)" d="${bars1}"/>`;
  g += `<path class="btc" d="${smoothPath(S1.map(([sx, sy]) => [sx, sy]))}"/>`;
  if (S2.length > 1) g += `<path class="bar" d="${bars2}"/><path class="bar hot" clip-path="url(#hband)" d="${bars2}"/><path class="px" d="${smoothPath(S2.map(([sx, sy]) => [sx, sy]))}"/>`;
  // Buys and sales as rings and dots on the price line, sized by amount, at
  // the price paid or received. Hovering one shows its card.
  const isTrade = (e) => e.kind === "buy" || ["sell", "sold", "spend"].includes(e.kind);
  const trades = (report.graph || []).filter((e) => isTrade(e) && e.t >= t0 - DAY && e.t <= t1 + DAY);
  const maxT = Math.max(...(report.graph || []).filter(isTrade).map((e) => e.sats), 1);
  const nearest = (t) => { let i = 0; for (let k = 1; k < pts.length; k++) if (Math.abs(pts[k].t - t) < Math.abs(pts[i].t - t)) i = k; return pts[i]; };
  const priceAt = (t) => { let i = 0; for (let k = 1; k < priced.length; k++) if (Math.abs(priced[k].t - t) < Math.abs(priced[i].t - t)) i = k; return priced[i]?.price || 0; };
  const dotR = 2.5 + (days > 365 ? 7 : 13) * Math.min(1, W / 900); // smaller dots on narrow charts
  const dots = [];
  trades.forEach((e, i) => {
    const p = e.cents && e.sats ? Math.round(e.cents * 1e8 / e.sats) : priceAt(e.t);
    if (!p) return;
    const buy = e.kind === "buy", r = 2.5 + (dotR - 2.5) * Math.sqrt(e.sats / maxT), cx = x(e.t);
    const cyP = y2(Math.min(Math.max(p, minP), maxP)), cyH = y1(nearest(e.t).sats);
    dots.push({ e, p, r, cx, cyP, cyH, i });
    g += `<circle class="${buy ? "buy-dot" : "sell-dot"}" data-dot="${i}" cx="${cx.toFixed(1)}" cy="${cyP.toFixed(1)}" r="${r.toFixed(1)}"/>`;
  });
  g += `</g>`;
  g += `<text class="title" x="${L}" y="${top1 - 8}">${prefs.unit === "btc" ? "BTC" : prefs.unit === "sats" ? "Sats" : "Bitcoin (₿)"} held</text><text class="title" x="${L}" y="${top2 - 10}">BTC price (USD)</text>`;
  g += `<g id="history-cross" visibility="hidden"><line class="cross" y1="${top1}" y2="${top2 + h2}"/><circle class="pt-btc" r="4"/><circle class="pt-px" r="4"/></g>`;
  g += `<rect x="${L}" y="${top1}" width="${PW}" height="${top2 + h2 - top1}" fill="transparent" id="history-hit"/>`;
  // The scrubber: the whole history as a faint area, with the visible
  // window as a draggable frame with a handle at each end.
  const sx = (t) => L + ((t - a) / Math.max(b - a, 1)) * PW;
  const maxAll = Math.max(...all.map((p) => p.sats), 1), sy = (s) => scrubTop + scrubH - (s / maxAll) * (scrubH - 4);
  const stepAll = Math.max(DAY, (b - a) / (PW / 2));
  let area = `M${L},${scrubTop + scrubH}`, k = 0;
  for (let t = a; t <= b; t += stepAll) { while (k + 1 < all.length && all[k + 1].t <= t) k++; area += `L${sx(t).toFixed(1)},${sy(all[k].sats).toFixed(1)}`; }
  area += `L${sx(b).toFixed(1)},${sy(all.at(-1).sats).toFixed(1)}L${W - R},${scrubTop + scrubH}Z`;
  const wx0 = sx(t0), wx1 = sx(t1), hw = 10;
  g += `<g id="history-scrub"><rect class="scrub-bg" x="${L}" y="${scrubTop}" width="${PW}" height="${scrubH}" rx="6"/><path class="scrub-area" d="${area}"/>
    <rect class="scrub-dim" x="${L}" y="${scrubTop}" width="${Math.max(wx0 - L, 0)}" height="${scrubH}"/><rect class="scrub-dim" x="${wx1}" y="${scrubTop}" width="${Math.max(W - R - wx1, 0)}" height="${scrubH}"/>
    <rect class="scrub-win" data-scrub="scrub" x="${wx0}" y="${scrubTop}" width="${Math.max(wx1 - wx0, 2)}" height="${scrubH}" rx="6"/>
    <g class="scrub-handle" data-scrub="left"><rect class="hit" x="${wx0 - 14}" y="${scrubTop - 6}" width="28" height="${scrubH + 12}"/><rect class="grip" x="${wx0 - hw / 2}" y="${scrubTop + 8}" width="${hw}" height="${scrubH - 16}" rx="3"/><line x1="${wx0}" x2="${wx0}" y1="${scrubTop + 13}" y2="${scrubTop + scrubH - 13}"/></g>
    <g class="scrub-handle" data-scrub="right"><rect class="hit" x="${wx1 - 14}" y="${scrubTop - 6}" width="28" height="${scrubH + 12}"/><rect class="grip" x="${wx1 - hw / 2}" y="${scrubTop + 8}" width="${hw}" height="${scrubH - 16}" rx="3"/><line x1="${wx1}" x2="${wx1}" y1="${scrubTop + 13}" y2="${scrubTop + scrubH - 13}"/></g></g>`;
  svg.setAttribute("width", W); svg.setAttribute("height", H); svg.setAttribute("viewBox", `0 0 ${W} ${H}`);
  svg.innerHTML = g;
  const cross = $("#history-cross"), tip = $("#history-tip"), band = $("#history-band"), hit = $("#history-hit");
  const bandW = Math.max(28, PW * 0.08);
  const placeTip = (px, py) => {
    const tw = tip.offsetWidth, th = tip.offsetHeight;
    let tx = px + 16; if (tx + tw > W - 4) tx = px - 16 - tw; if (tx < 4) tx = 4;
    tip.style.left = tx + "px"; tip.style.top = Math.max(2, Math.min(py - th / 2, H - th - 2)) + "px";
  };
  const first = inView[0] || pts[0], firstPriced = pricedIn[0] || priced[0];
  let hot = null;
  hit.addEventListener("pointermove", (e) => {
    if (hist.drag) return;
    const rect = svg.getBoundingClientRect(), mx = e.clientX - rect.left, my = e.clientY - rect.top;
    band.setAttribute("x", mx - bandW / 2); band.setAttribute("width", bandW);
    // A dot under the pointer wins over the plain readout.
    let best = null, bd = Infinity;
    for (const d of dots) { const dist = Math.hypot(d.cx - mx, d.cyP - my) - d.r; if (dist < bd) { bd = dist; best = d; } }
    svg.classList.toggle("dot-hover", !!best && bd < 6);
    if (hot != null) $$(`#history [data-dot="${hot}"]`).forEach((c) => c.classList.remove("hot"));
    hot = null;
    tip.hidden = false;
    cross.setAttribute("visibility", "visible");
    const [cb, cp] = $$("circle", cross);
    if (best && bd < 6) {
      hot = best.i;
      $$(`#history [data-dot="${hot}"]`).forEach((c) => c.classList.add("hot"));
      const ev = best.e, buy = ev.kind === "buy", now = report.price || priceAt(b);
      $("line", cross).setAttribute("x1", best.cx); $("line", cross).setAttribute("x2", best.cx);
      cb.setAttribute("cx", best.cx); cb.setAttribute("cy", best.cyH); cp.setAttribute("cx", best.cx); cp.setAttribute("cy", best.cyP);
      tip.innerHTML = `<div class="tip-row"><b>${buy ? "Bought" : esc(KIND_NAMES[ev.kind] || "Sold")} ${amt(ev.sats)} at ${usd(best.p)}</b>${now ? pctPill(pct(best.p, now)) : ""}</div>
        <div class="tip-line">${ev.cents ? `${buy ? "Cost" : "Proceeds"} ${usd(ev.cents)} · ` : ""}${esc(ev.account)}${now ? ` · ${usd(now)} today` : ""}</div><div class="tip-cap">${day(ev.t)} · ${amt(nearest(ev.t).sats)} held after</div>`;
      placeTip(best.cx, best.cyP);
      return;
    }
    const t = t0 + ((mx - L) / PW) * (t1 - t0);
    const p = nearest(t), xx = x(p.t);
    $("line", cross).setAttribute("x1", xx); $("line", cross).setAttribute("x2", xx);
    cb.setAttribute("cx", xx); cb.setAttribute("cy", y1(p.sats));
    cp.setAttribute("cx", xx); cp.setAttribute("cy", p.price ? y2(p.price) : -20);
    const change = p.price && firstPriced ? pct(firstPriced.price, p.price) : pct(first.sats, p.sats);
    tip.innerHTML = `<div class="tip-row"><b>${amt(p.sats)}</b>${pctPill(change)}</div>
      <div class="tip-line">${p.price ? `${usd(p.price)} per BTC · worth ${usd(Math.round(p.sats * p.price / 1e8))}` : "No price"}</div>
      <div class="tip-cap">${day(p.t)} · ${p.price && firstPriced ? "price" : "holdings"} since ${day((p.price && firstPriced ? firstPriced : first).t)}</div>`;
    placeTip(xx, y1(p.sats));
  });
  hit.addEventListener("pointerleave", () => { svg.classList.remove("dot-hover"); cross.setAttribute("visibility", "hidden"); tip.hidden = true; band.setAttribute("width", 0); });
  // Wheel zooms around the pointer; dragging pans; the scrubber's frame
  // pans and its handles resize. Each leaves the presets.
  hit.addEventListener("wheel", (e) => {
    e.preventDefault();
    const rect = svg.getBoundingClientRect(), f = Math.min(Math.max((e.clientX - rect.left - L) / PW, 0), 1);
    const at = t0 + f * (t1 - t0), k = Math.exp(e.deltaY * 0.0015);
    let span = Math.min(Math.max((t1 - t0) * k, 2 * DAY), b - a);
    hist.t0 = Math.max(a, at - f * span); hist.t1 = Math.min(b, hist.t0 + span); hist.t0 = Math.max(a, hist.t1 - span);
    hist.preset = ""; $$("#history-range button").forEach((x) => x.classList.remove("on"));
    drawHistory();
  }, { passive: false });
  const startDrag = (e, mode, pxPerSec) => { e.preventDefault(); hist.drag = { mode, x: e.clientX, t0, t1, pxPerSec }; svg.classList.add("dragging"); cross.setAttribute("visibility", "hidden"); tip.hidden = true; band.setAttribute("width", 0); };
  hit.addEventListener("pointerdown", (e) => startDrag(e, "pan", PW / (t1 - t0)));
  $("#history-scrub").addEventListener("pointerdown", (e) => { const m = e.target.closest("[data-scrub]")?.dataset.scrub; if (m) startDrag(e, m, PW / Math.max(b - a, 1)); });
  // Clicking a buy or sale opens its record; a finger gets more room.
  hit.addEventListener("click", (e) => {
    if (hist.dragged) return;
    const rect = svg.getBoundingClientRect(), mx = e.clientX - rect.left, my = e.clientY - rect.top;
    let best = null, bd = Infinity;
    for (const d of dots) { const dist = Math.hypot(d.cx - mx, d.cyP - my) - d.r; if (dist < bd) { bd = dist; best = d; } }
    if (best && bd < (e.pointerType === "touch" ? 16 : 6)) selectEvent(best.e.key, false);
  });
  // Clicking the strip outside the frame jumps the window there.
  $("#history-scrub").addEventListener("click", (e) => {
    if (hist.dragged || e.target.closest("[data-scrub]")) return;
    const rect = svg.getBoundingClientRect(), at = a + ((e.clientX - rect.left - L) / PW) * (b - a), span = t1 - t0;
    hist.t0 = Math.min(Math.max(a, at - span / 2), b - span); hist.t1 = hist.t0 + span; hist.preset = "";
    $$("#history-range button").forEach((x) => x.classList.remove("on")); drawHistory();
  });
}
window.addEventListener("resize", () => { if ($("#overview").classList.contains("on") && report) drawHistory(); });
// Pinch zooms the history around the fingers, and moving them pans.
(() => {
  let s = null;
  const frac = (cx) => { const { L, PW } = hist.geo; return Math.min(Math.max((cx - $("#history").getBoundingClientRect().left - L) / PW, 0), 1); };
  pinchZoom($("#history"), (cx) => {
    hist.drag = null;
    $("#history-tip").hidden = true;
    s = { t0: hist.t0, t1: hist.t1 };
    s.at = s.t0 + frac(cx) * (s.t1 - s.t0);
  }, (k, cx) => {
    const [a, b] = histBounds(), span = Math.min(Math.max((s.t1 - s.t0) * k, 2 * DAY), b - a);
    hist.t0 = Math.min(Math.max(a, s.at - frac(cx) * span), b - span);
    hist.t1 = hist.t0 + span;
    hist.preset = ""; $$("#history-range button").forEach((x) => x.classList.remove("on"));
    drawHistory();
  });
})();

// Wallets
function renderWallets() {
  const ws = (report.accounts || []).filter((a) => a.kind === "wallet");
  $("#wallets-table tbody").innerHTML = ws.map((a) => `<tr>
    <td>${esc(a.name)}</td><td>${esc(a.script)}</td><td class="num">${btc(a.chainSats ?? 0)}</td><td class="small muted">${esc(a.scannedAt || "never")}</td>
    <td><button data-scan="${a.id}">Scan</button> <button class="link" data-del="${a.id}">Remove</button></td></tr>`).join("")
    || `<tr><td colspan="5" class="muted">No wallets yet.</td></tr>`;
}
$("#wallets-table").addEventListener("click", async (e) => {
  const scan = e.target.dataset.scan, del = e.target.dataset.del;
  if (scan) {
    e.target.disabled = true;
    e.target.textContent = "Scanning…";
    try {
      const r = await post(`/api/wallets/${scan}/scan`, {});
      toast(`Found ${r.txs} transactions, balance ${amt(r.balance)}`);
    } catch (err) { toast(err.message); }
    refresh();
  }
  if (del && confirm(t("Remove this wallet and its scanned transactions?"))) {
    await api(`/api/accounts/${del}`, { method: "DELETE" });
    refresh();
  }
});
// Wallet form: single key, multisig built from cosigner keys, or a pasted
// output descriptor. The server turns a multisig form into a descriptor.
function cosignerRow(v = "") {
  const d = document.createElement("div");
  d.className = "cosigner";
  d.innerHTML = `<input placeholder="Cosigner key: xpub…, Zpub… or [fingerprint/48h/0h/0h/2h]xpub…" autocomplete="off" spellcheck="false" value="${esc(v)}"><button type="button" class="link" title="Remove">✕</button>`;
  $("button", d).addEventListener("click", () => { d.remove(); numberCosigners(); });
  $("#cosigners").append(d);
  numberCosigners();
}
function numberCosigners() {
  const rows = $$("#cosigners .cosigner");
  rows.forEach((r, i) => ($("input", r).ariaLabel = `Cosigner ${i + 1}`));
  $("#wallet-form").elements.threshold.max = Math.max(rows.length, 1);
}
for (let i = 0; i < 3; i++) cosignerRow();
$("#add-cosigner").addEventListener("click", () => cosignerRow());
function walletKind() { return $("#wallet-form").elements.kind.value; }
function showWalletKind() {
  const k = walletKind();
  $$("#wallet-form [class*='kind-']").forEach((el) => { el.hidden = !el.classList.contains("kind-" + k); });
  $("#wallet-form").elements.key.placeholder = k === "descriptor" ? "wsh(sortedmulti(2,[…]xpub…/<0;1>/*,…))#checksum" : "zpub…";
  $("#preview").textContent = "";
}
$$("#wallet-form input[name=kind]").forEach((r) => r.addEventListener("change", showWalletKind));
showWalletKind();
const walletData = () => {
  const f = new FormData($("#wallet-form"));
  const base = { name: f.get("name"), gap: Number(f.get("gap")) };
  if (walletKind() === "multisig") {
    return { ...base, multisig: { threshold: Number(f.get("threshold")), script: f.get("msScript"), sorted: f.get("sorted") === "on",
      keys: $$("#cosigners input").map((i) => i.value.trim()).filter(Boolean) } };
  }
  return { ...base, key: f.get("key"), script: walletKind() === "single" ? f.get("script") : "" };
};
$("#preview-btn").addEventListener("click", async () => {
  try {
    const r = await post("/api/wallets/preview", walletData());
    $("#preview").innerHTML = `Detected ${esc(r.prefix)} (${esc(r.script)}). First receive addresses, check they match your wallet:<br><span class="mono">${r.addresses.map(esc).join("<br>")}</span>`
      + (walletKind() === "multisig" ? `<div class="muted" style="margin-top:6px">Descriptor: <span class="mono">${esc(r.descriptor)}</span></div>` : "");
  } catch (e) { $("#preview").textContent = e.message; }
});
$("#wallet-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  try {
    const { id } = await post("/api/wallets", walletData());
    e.target.reset();
    $("#cosigners").innerHTML = ""; for (let i = 0; i < 3; i++) cosignerRow();
    showWalletKind();
    $("#preview").textContent = "";
    toast("Wallet added. Scanning…");
    await refresh(); // show it now; the scan can take a while
    await post(`/api/wallets/${id}/scan`, {}).then((r) => toast(`Found ${r.txs} transactions`)).catch((err) => toast("Added, but the scan failed: " + err.message));
    refresh();
  } catch (err) { toast(err.message); }
});

// Imports
// Suggest a fresh export when a provider's newest imported row is old.
async function renderFreshness() {
  const rows = await api("/api/imports/freshness").catch(() => []);
  const stale = rows.filter((r) => r.days > 30);
  const age = (d) => (d === 0 ? "today" : d === 1 ? "yesterday" : `${d} days ago`);
  $("#freshness").innerHTML = !rows.length ? "" : stale.length
    ? `<div class="card fresh stale"><b>Fresh exports?</b>${stale.map((r) => `<div>${esc(r.account)}: newest row is from ${esc(r.last)}, ${age(r.days)}.</div>`).join("")}
       <div class="small muted">Drop new CSVs below. Duplicates skipped.</div></div>`
    : `<p class="small muted">Up to date: ${rows.map((r) => `${esc(r.account)} (${age(r.days)})`).join(", ")}</p>`;
}

async function renderImports() {
  renderFreshness();
  const list = await api("/api/imports").catch(() => []);
  $("#imports-table tbody").innerHTML = (list || []).map((i) => `<tr>
    <td>${esc(i.filename)}</td><td>${esc(i.account)}</td><td>${esc(i.handler)}</td><td class="num">${i.rows}</td>
    <td class="small">${(i.warnings || []).length ? `<details><summary class="warn">${i.warnings.length}</summary>${i.warnings.map((w) => `line ${w.Row}: ${esc(w.Message)}`).join("<br>")}</details>` : ""}</td>
    <td><button class="link" data-del-import="${i.id}">Remove</button></td></tr>`).join("") || `<tr><td colspan="6" class="muted">Nothing imported yet.</td></tr>`;
}
$("#imports-table").addEventListener("click", async (e) => {
  const id = e.target.dataset.delImport;
  if (id && confirm(t("Remove this import and all its rows?"))) {
    await api(`/api/imports/${id}`, { method: "DELETE" });
    refresh();
  }
});
$("#import-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  try {
    const r = await api("/api/imports", { method: "POST", body: new FormData(e.target) });
    toast(`Imported ${r.events} rows from ${r.handler}` + (r.alreadyImported ? `, skipped ${r.alreadyImported} already imported` : "") + (r.warnings?.length ? `, ${r.warnings.length} warnings` : ""));
    e.target.reset();
    refresh();
  } catch (err) { toast(err.message); }
});
// Drag and drop: import many files, detecting each platform; ask about
// the ones that can't be recognised.
let handlers = [];
const queue = []; // {file, state: wait|ok|skip|ask|dropped, msg}
const importOne = async (q, handler = "") => {
  const fd = new FormData();
  fd.append("file", q.file);
  fd.append("handler", handler);
  const r = await fetch("/api/imports", { method: "POST", body: fd });
  const body = await r.json().catch(() => ({}));
  if (r.ok) {
    q.state = "ok";
    q.msg = `${body.events} rows into ${body.account}` + (body.alreadyImported ? `, ${body.alreadyImported} already imported` : "") + (body.warnings?.length ? `, ${body.warnings.length} warnings` : "");
  } else if (r.status === 409) {
    q.state = "skip"; q.msg = "Already imported";
  } else if (r.status === 422) {
    q.state = "ask"; q.msg = "Couldn't tell which platform this is from.";
  } else {
    q.state = "ask"; q.msg = body.error || r.statusText;
  }
};
function renderQueue() {
  $("#drop-queue").hidden = !queue.length;
  const opts = handlers.map((h) => `<option value="${esc(h.id)}">${esc(h.name)}</option>`).join("");
  $("#drop-queue tbody").innerHTML = queue.map((q, i) => `<tr><td>${esc(q.file.name)}</td><td class="small">${
    q.state === "wait" ? '<span class="muted">Importing…</span>'
    : q.state === "ok" ? `<span class="ok">Imported</span> ${esc(q.msg)}`
    : q.state === "skip" ? `<span class="muted">${esc(q.msg)}</span>`
    : q.state === "dropped" ? '<span class="muted">Skipped</span>'
    : `<span class="warn">${esc(q.msg)}</span> <span class="row"><select data-q="${i}"><option value="">Which platform?</option>${opts}</select><button data-q-go="${i}">Import</button><button class="link" data-q-skip="${i}">Skip</button></span>`
  }</td></tr>`).join("");
}
async function importFiles(files) {
  const added = [...files].map((file) => ({ file, state: "wait", msg: "" }));
  // Keep earlier files still waiting for an answer; drop finished ones.
  queue.splice(0, queue.length, ...queue.filter((q) => q.state === "ask"), ...added);
  renderQueue();
  for (const q of added) { // one at a time, so each account is created once
    try { await importOne(q); } catch (e) { q.state = "ask"; q.msg = e.message; }
    renderQueue();
  }
  const n = added.filter((q) => q.state === "ok").length, ask = queue.filter((q) => q.state === "ask").length;
  toast(`Imported ${n} of ${added.length} files` + (ask ? `. ${ask} need${ask === 1 ? "s" : ""} a platform.` : "."));
  refresh();
}
const drop = $("#drop");
drop.addEventListener("click", () => $("#drop-input").click());
drop.addEventListener("keydown", (e) => { if (e.key === "Enter" || e.key === " ") { e.preventDefault(); $("#drop-input").click(); } });
$("#drop-input").addEventListener("change", (e) => { if (e.target.files.length) importFiles(e.target.files); e.target.value = ""; });
["dragenter", "dragover"].forEach((t) => drop.addEventListener(t, (e) => { e.preventDefault(); drop.classList.add("over"); }));
["dragleave", "drop"].forEach((t) => drop.addEventListener(t, () => drop.classList.remove("over")));
drop.addEventListener("drop", (e) => { e.preventDefault(); if (e.dataTransfer.files.length) importFiles(e.dataTransfer.files); });
// A file dropped anywhere else should not make the browser open it.
window.addEventListener("dragover", (e) => e.preventDefault());
window.addEventListener("drop", (e) => e.preventDefault());
$("#drop-queue").addEventListener("click", async (e) => {
  const go = e.target.dataset.qGo, skip = e.target.dataset.qSkip;
  if (skip) { queue[skip].state = "dropped"; renderQueue(); return; }
  if (!go) return;
  const h = $(`select[data-q="${go}"]`).value;
  if (!h) { toast("Pick a platform first."); return; }
  const q = queue[go];
  q.state = "wait"; renderQueue();
  try { await importOne(q, h); } catch (err) { q.state = "ask"; q.msg = err.message; }
  renderQueue();
  refresh();
});

$("#price-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  try {
    const r = await api("/api/prices", { method: "POST", body: new FormData(e.target) });
    toast(`Imported ${r.days} daily prices`);
    refresh();
  } catch (err) { toast(err.message); }
});

// Review: the labeling queue
// A self transfer needs no basis: the coins keep their original basis and
// purchase dates. Out, they park in "untracked"; in, they come back from it.
const OUT_CATS = [["self-transfer", "Self transfer to my own wallet (not added here)"], ["self", "Self transfer, matches another row…"], ["spend", "Paid for something"],
  ["sold", "Sold to a person"], ["gift", "Gift sent"], ["donation", "Donation"], ["lost", "Lost or stolen"]];
const IN_CATS = [["self-transfer", "Self transfer from my own wallet"], ["self", "Self transfer, matches another row…"], ["income", "Income / payment received"],
  ["gift-in", "Gift received"], ["purchase", "Bought from a person"], ["refund", "Refund"], ["untracked", "From an old wallet of mine, enter its basis"]];
const needsAmount = { "gift-in": "Donor's basis (USD)", purchase: "Cost (USD)", refund: "Basis (USD)", untracked: "Basis (USD)" };
const MARKET_VALUE = { spend: "Value of what you paid for (USD)", sold: "Amount the buyer paid (USD)", income: "Value received (USD)" };

// How an item moved, as a colored icon with a word beside it.
const NET_ICON = {
  onchain: ["On-chain", '<svg viewBox="0 0 20 20" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><path d="M8.5 11.5a3 3 0 0 0 4.2 0l2.6-2.6a3 3 0 0 0-4.2-4.2l-1 1"/><path d="M11.5 8.5a3 3 0 0 0-4.2 0l-2.6 2.6a3 3 0 0 0 4.2 4.2l1-1"/></svg>'],
  lightning: ["Lightning", '<svg viewBox="0 0 20 20" fill="currentColor"><path d="M11.5 1.5 4 11h5l-1 7.5L16 9h-5l.5-7.5z"/></svg>'],
  unknown: ["Not stated", '<svg viewBox="0 0 20 20" fill="none" stroke="currentColor" stroke-width="2"><circle cx="10" cy="10" r="7"/><path d="M8 8a2 2 0 1 1 2.5 2c-.5.2-.5.6-.5 1.2M10 14h.01" stroke-linecap="round"/></svg>'],
};
const netBadge = (n) => { const k = NET_ICON[n] ? n : "unknown"; return `<span class="net ${k}" title="${NET_ICON[k][0]}">${NET_ICON[k][1]}</span>`; };
const dirTag = (d) => d === "out"
  ? '<span class="tag out"><svg viewBox="0 0 20 20" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M10 16V4M5 9l5-5 5 5"/></svg>Out</span>'
  : '<span class="tag in"><svg viewBox="0 0 20 20" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M10 4v12M5 11l5 5 5-5"/></svg>In</span>';
let reviewSort = { key: "date", dir: 1 };
$("#review-table thead").addEventListener("click", (e) => {
  const k = e.target.closest("[data-sort]")?.dataset.sort;
  if (!k) return;
  reviewSort = { key: k, dir: reviewSort.key === k ? -reviewSort.dir : 1 };
  renderReview();
});
const reviewKey = { date: (i) => new Date(i.Time).getTime(), account: (i) => i.Account.toLowerCase(), amount: (i) => i.Sats,
  type: (i) => (i.Network || "zz") + (i.Dir === "out" ? "1" : "0") };

function renderReview() {
  const items = [...(report.unmatched || [])];
  const f = reviewKey[reviewSort.key];
  items.sort((a, b) => { const x = f(a), y = f(b); return (x < y ? -1 : x > y ? 1 : 0) * reviewSort.dir; });
  $$("#review-table .sort").forEach((b) => { b.classList.toggle("asc", b.dataset.sort === reviewSort.key && reviewSort.dir > 0); b.classList.toggle("desc", b.dataset.sort === reviewSort.key && reviewSort.dir < 0); });
  $("#review-table tbody").innerHTML = items.map((it) => {
    const cats = it.Dir === "out" ? OUT_CATS : IN_CATS;
    const detail = [it.Source === "chain" ? "" : esc(it.Kind), it.Address && `<span class="mono">${esc(it.Address)}</span>`,
      it.Txid && `<span class="mono" title="${esc(it.Txid)}">${esc(it.Txid.slice(0, 12))}…</span>`, it.Note && esc(it.Note)].filter(Boolean).join(" · ");
    const net = it.Network || "unknown";
    return `<tr data-key="${esc(it.Key)}" data-dir="${esc(it.Dir)}" data-value="${it.Value || 0}">
      <td>${day(it.Time)}</td><td>${esc(it.Account)}</td><td><span class="tags"><span class="tag ${NET_ICON[net] ? net : "unknown"}">${(NET_ICON[net] || NET_ICON.unknown)[1]}${(NET_ICON[net] || NET_ICON.unknown)[0]}</span>${dirTag(it.Dir)}</span></td>
      <td class="num">${btc(it.Sats)}${it.FeeSats ? `<div class="small muted">+${amt(it.FeeSats)} fee</div>` : ""}${it.Value ? `<div class="small muted">≈ ${usd(it.Value)}</div>` : ""}</td>
      <td class="row"><select class="cat"><option value="">Choose…</option>${cats.map(([v, l]) => `<option value="${v}">${esc(l)}</option>`).join("")}</select>
        <span class="extra"></span><button class="save" hidden>Save</button></td>
      <td class="small">${detail}<button type="button" class="link ai-ask" data-ask="${esc(it.Key)}" data-ask-q="What is this item, and how should I label it?">Ask the assistant</button></td></tr>`;
  }).join("") || `<tr><td colspan="6" class="muted">Nothing to label.</td></tr>`;
  // A record as people read it: date, account, amount.
  const byKey = Object.fromEntries((report.graph || []).map((g) => [g.key, g]));
  const unm = Object.fromEntries((report.unmatched || []).map((i) => [i.Key, i]));
  const rec = (k) => { const g = byKey[k]; const i = unm[k]; return g ? `${day(g.t)} ${esc(g.account)} ${amt(g.sats)}` : i ? `${day(i.Time)} ${esc(i.Account)} ${amt(i.Sats)}` : `<span class="mono">${esc(k)}</span>`; };
  $("#links-table tbody").innerHTML = (report.links || []).map((l) => `<tr><td class="small"><a href="${recordLink(l.From)}" data-flowpick="${esc(l.From)}">${rec(l.From)}</a></td>
      <td class="small">${byKey[l.From]?.to ? `${day(byKey[l.From].t)} ${esc(byKey[l.From].to)} ${amt(byKey[l.From].sats)}` : rec(l.To)}</td><td class="num small">${l.FeeSats ? amt(l.FeeSats) : ""}</td><td class="small">${esc(l.Method)}</td>
      <td>${l.Method === "manual" ? `<button class="link" data-unlink="${esc(l.From)}" data-unlink2="${esc(l.To)}">undo</button>` : `<button class="link" data-autounlink="${esc(l.From)}" data-autounlink2="${esc(l.To)}">Unlink</button>`}</td></tr>`).join("")
    || `<tr><td colspan="5" class="muted">No transfers matched yet.</td></tr>`;
  if (typeof renderDeck === "function") renderDeck();
  $("#unlinked-list").innerHTML = (report.unlinked || []).length
    ? `<h3>Not to be matched</h3>` + report.unlinked.map((p) => { const [a, b] = p.split(">"); return `<div>${rec(a)} → ${rec(b)} <button class="link small" data-allow="${esc(a)}" data-allow2="${esc(b)}">Allow again</button></div>`; }).join("")
    : "";
}
$("#review-table").addEventListener("change", (e) => {
  if (!e.target.classList.contains("cat")) return;
  const tr = e.target.closest("tr"), cat = e.target.value, extra = $(".extra", tr);
  extra.innerHTML = "";
  if (cat === "self") {
    const opp = (report.unmatched || []).filter((i) => i.Dir !== tr.dataset.dir);
    extra.innerHTML = `<select class="cp">${opp.map((i) => `<option value="${esc(i.Key)}">${day(i.Time)} ${esc(i.Account)} ${amt(i.Sats)}</option>`).join("")}</select>`;
  } else if (cat === "self-transfer") {
    extra.innerHTML = `<span class="small muted">${tr.dataset.dir === "out" ? "Keeps its basis and dates." : "Returns with its original basis and dates."}</span>`;
  } else if (MARKET_VALUE[cat]) {
    // Sales, payments and income are valued at that day's market price
    // unless the user knows the actual amount.
    const v = Number(tr.dataset.value);
    extra.innerHTML = `<input class="amt" inputmode="decimal" placeholder="${v ? usd(v) : MARKET_VALUE[cat]}" size="11" title="${MARKET_VALUE[cat]}">`
      + `<span class="small muted hint-line">${v ? `Uses ${usd(v)}, the market value that day. Type the actual amount only if it was different.` : "No price for that day: enter the amount."}</span>`;
  } else if (needsAmount[cat]) {
    extra.innerHTML = `<input class="amt" inputmode="decimal" placeholder="${needsAmount[cat]}" size="12">` +
      (cat === "gift-in" ? ` <input class="acq" type="date" title="Donor's acquisition date">` : "");
  }
  $(".save", tr).hidden = !cat;
});
$("#review-table").addEventListener("click", async (e) => {
  if (!e.target.classList.contains("save")) return;
  const tr = e.target.closest("tr");
  const label = { Key: tr.dataset.key, Category: $(".cat", tr).value };
  const amt = $(".amt", tr)?.value.replace(/[$,\s]/g, "");
  if (amt) label.FiatCents = Math.round(Number(amt) * 100);
  const acq = $(".acq", tr)?.value;
  if (acq) label.AcquiredAt = acq + "T00:00:00Z";
  const cp = $(".cp", tr)?.value;
  if (cp) label.Counterpart = cp;
  try { await post("/api/labels", label); refresh(); } catch (err) { toast(err.message); }
});
// Unlink an automatic match, or allow a pair to be matched again.
document.addEventListener("click", async (e) => {
  const t = e.target.closest("[data-autounlink],[data-allow]");
  if (!t) return;
  const allow = !!t.dataset.allow;
  try {
    await post("/api/unlink", { From: t.dataset.autounlink || t.dataset.allow, To: t.dataset.autounlink2 || t.dataset.allow2, Allow: allow });
    toast(allow ? "This pair can be matched again" : "Unlinked: both records are back in Review");
    await refresh();
    if ($("#graph-detail")?.open && selected) selectEvent(selected, false);
  } catch (err) { toast(err.message); }
});
$("#links-table").addEventListener("click", async (e) => {
  const a = e.target.dataset.unlink, b = e.target.dataset.unlink2;
  if (!a) return;
  await api(`/api/labels/${encodeURIComponent(a)}`, { method: "DELETE" }).catch(() => {});
  await api(`/api/labels/${encodeURIComponent(b)}`, { method: "DELETE" }).catch(() => {});
  refresh();
});

// Taxes
function renderProtected() {
  const prot = (report.graph || []).filter((g) => g.protected);
  $("#protected-table tbody").innerHTML = prot.map((g) => `<tr><td>${day(g.t)}</td><td>${esc(g.account)}</td><td class="num">${btc(g.sats)}</td>
    <td><button class="link" data-unprotect="${esc(g.key)}">Stop protecting</button></td></tr>`).join("") || `<tr><td colspan="4" class="muted">None.</td></tr>`;
}
$("#protected-table").addEventListener("click", async (e) => {
  const k = e.target.dataset.unprotect;
  if (k) { await post("/api/protect", { key: k, on: false }); refresh(); }
});


// Form 8949 lines for one year, checked against imported 1099-DAs. The
// year is the one picked in Tax season (season.js).
let formsYear = 0;
function fillForms(f) {
  const boxes = Object.keys(f.boxes).sort();
  $("#forms-downloads").innerHTML = f.rows.length
    ? `Download: <a href="/api/export/8949.csv?year=${f.year}">all lines</a>` + boxes.map((b) => ` · <a href="/api/export/8949.csv?year=${f.year}&box=${b}">box ${b}</a> (${f.boxes[b]})`).join("")
      + (f.forms.length || f.year >= 2025 ? ` · <a href="/api/export/1099da-reconciliation.csv?year=${f.year}">1099-DA reconciliation</a>` : "")
    : "";
  $("#da-list").innerHTML = f.forms.length
    ? "Lines imported for " + f.year + ": " + f.forms.map((x) => `${esc(x.account)} <span class="muted">(${esc(x.filename)})</span> <button class="link small da-del" data-id="${x.id}">Remove</button>`).join(" · ")
    : "";
  $("#forms-summary").innerHTML = f.issues
    ? `<p class="warn">${f.issues} line${f.issues === 1 ? "" : "s"} to look at: differences from a 1099-DA, sales a 1099-DA should cover but doesn't, 1099-DA lines with no sale, or coins with no known purchase.</p>`
    : f.rows.length ? `<p class="pos">Every line is accounted for.</p>` : `<p class="muted">No sales or spends in ${f.year}.</p>`;
  const ok = (r) => r.status === "matches the 1099-DA" || r.status === "before 1099-DA reporting" || r.status === "not a broker account: no 1099-DA expected";
  $("#forms-table tbody").innerHTML = f.rows.map((r) => `<tr>
      <td><b>${esc(r.box)}</b></td><td>${esc(r.sold)}</td><td>${esc(r.account)}</td><td class="num">${btc(r.sats)}</td>
      <td class="num">${usd(r.proceeds)}</td><td class="num">${usd(r.basis)}</td><td>${esc(r.code)}</td><td class="num">${r.adjustment ? usd(r.adjustment) : ""}</td>
      <td class="num">${signed(r.gain)}</td>
      <td class="small${ok(r) && !r.note ? " muted" : " warn"}">${esc(r.status)}${r.note ? `<br>${esc(r.note)}` : ""} <a href="${recordLink(r.record)}" data-flowpick="${esc(r.record)}">open</a></td></tr>`).join("")
    + f.daOnly.map((d) => `<tr><td></td><td>${day(d.row.sold)}</td><td>${esc(d.account)}</td><td class="num">${btc(d.row.sats)}</td><td class="num">${usd(d.row.proceeds)}</td>
      <td class="num">${d.row.hasBasis ? usd(d.row.basis) : "not reported"}</td><td></td><td></td><td></td><td class="small warn">Only on the 1099-DA. ${esc(d.note)}</td></tr>`).join("")
    || `<tr><td colspan="10" class="muted">None.</td></tr>`;
}
$("#da-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  try {
    const r = await api("/api/1099da", { method: "POST", body: new FormData(e.target) });
    toast(`Imported ${r.rows} Bitcoin line${r.rows === 1 ? "" : "s"}` + (r.warnings?.length ? `, ${r.warnings.length} skipped` : ""));
    e.target.reset();
    renderSeason();
  } catch (err) { toast(err.message); }
});
$("#da-list").addEventListener("click", async (e) => {
  const id = e.target.closest(".da-del")?.dataset.id;
  if (!id) return;
  try { await api(`/api/1099da/${id}`, { method: "DELETE" }); renderSeason(); } catch (err) { toast(err.message); }
});

// The January 2025 split of pooled basis across accounts.
function renderAllocation() {
  const a = report.allocation || { rows: [] };
  const card = $("#alloc-card");
  card.hidden = !a.rows.length;
  if (!a.rows.length) return;
  const accts = [...new Set([...(report.accounts || []).filter((x) => x.kind !== "pool").map((x) => x.name), ...a.rows.map((r) => r.account)])].sort();
  $("#alloc-status").innerHTML = a.confirmed
    ? (a.changed ? `<p class="warn">Changed since you confirmed it on ${day(a.confirmed)}: confirm again to keep a record of the new split.</p>`
                 : `<p class="pos">Confirmed on ${day(a.confirmed)}. This split is in your accountant package.</p>`)
    : `<p class="warn">Not confirmed yet.</p>`;
  const tot = {};
  for (const r of a.rows) { tot[r.account] ||= [0, 0]; tot[r.account][0] += r.sats; tot[r.account][1] += r.basis; }
  $("#alloc-status").innerHTML += `<p>${Object.entries(tot).sort().map(([n, [sats, basis]]) => `<b>${esc(n)}</b> ${amt(sats)}, basis ${usd(basis)}`).join(" · ")}</p>`;
  $("#alloc-table tbody").innerHTML = a.rows.map((r) => `<tr>
    <td>${day(r.acquired)}</td><td>${esc(r.boughtOn || "")}</td><td class="num">${btc(r.sats)}</td><td class="num">${usd(r.basis)}</td>
    <td><select class="alloc-acct" data-origin="${esc(r.originId)}">${accts.map((n) => `<option${n === r.account ? " selected" : ""}>${esc(n)}</option>`).join("")}</select>
      ${r.assigned ? '<span class="small muted">set by you</span>' : '<span class="small muted">default</span>'}
      <a href="${recordLink(r.originId)}" data-flowpick="${esc(r.originId)}" class="small">open</a></td></tr>`).join("");
}
$("#alloc-table").addEventListener("change", async (e) => {
  const sel = e.target.closest(".alloc-acct");
  if (!sel) return;
  try { await post("/api/allocation", { OriginID: sel.dataset.origin, Account: sel.value }); toast("Split updated"); refresh(); } catch (err) { toast(err.message); }
});
$("#alloc-confirm").addEventListener("click", async () => {
  try { await post("/api/allocation/confirm", {}); toast("Split confirmed and recorded"); refresh(); } catch (err) { toast(err.message); }
});

function renderTaxes() {
  renderProtected();
  renderAllocation();
  renderSeason();
  const sales = {};
  // A sale can use several lots; count each sale once.
  const seenSale = new Set();
  for (const d of report.disposals || []) {
    if (seenSale.has(d.OpID)) continue;
    seenSale.add(d.OpID);
    const yr = Number(day(d.Disposed).slice(0, 4));
    sales[yr] = (sales[yr] || 0) + 1;
  }
  $("#years-table tbody").innerHTML = (report.years || []).map((y) => `<tr><td>${y.year}</td>
    <td class="num">${signed(y.shortProceeds - y.shortBasis)}</td><td class="num">${signed(y.longProceeds - y.longBasis)}</td>
    <td class="num">${usd(y.income)}</td>
    <td class="num">${y.carryInShort || y.carryInLong ? usd(y.carryInShort + y.carryInLong) : '<span class="muted">–</span>'}</td>
    <td class="num">${y.lossDeducted ? usd(y.lossDeducted) : '<span class="muted">–</span>'}</td>
    <td class="num">${y.carryOutShort || y.carryOutLong ? `<span title="${usd(y.carryOutShort)} short-term, ${usd(y.carryOutLong)} long-term">${usd(y.carryOutShort + y.carryOutLong)}</span>` : '<span class="muted">–</span>'}</td>
    <td class="num">${y.estimatedTax < 0 ? `<span class="pos" title="Losses lower this year's tax">saves ${usd(-y.estimatedTax)}</span>` : usd(y.estimatedTax)}${y.missingBasisSats ? ` <span class="warn" title="${amt(y.missingBasisSats)} sold with no known basis">!</span>` : ""}</td>    <td title="${esc([...(y.digitalAsset?.reasons || []), ...(y.digitalAsset?.ask || [])].join("; ") || "No sales, spends, income or fees paid in BTC")}">${esc(y.digitalAsset?.answer || "No")}${y.digitalAsset?.ask?.length ? ' <span class="warn">?</span>' : ""}</td>

    <td>${sales[y.year] ? `<a href="/api/export/8949.csv?year=${y.year}">Form 8949 CSV</a> <span class="small muted">${sales[y.year]} sales</span>` : `<span class="small muted" title="Income is reported elsewhere.">No sales to report</span>`}</td></tr>`).join("") || `<tr><td colspan="6" class="muted">No disposals yet.</td></tr>`;
  $("#disposals-table tbody").innerHTML = (report.disposals || []).map((d) => `<tr>
    <td>${day(d.Disposed)}</td><td>${d.Missing ? '<span class="warn">unknown</span>' : day(d.Acquired)}</td><td>${esc(d.Account)}</td>
    <td class="num">${btc(d.Sats)}</td><td class="num">${usd(d.Proceeds)}</td><td class="num">${usd(d.Basis)}</td>
    <td class="num">${signed(d.Proceeds - d.Basis)}</td><td>${d.LongTerm ? "Long" : "Short"}</td></tr>`).join("") || `<tr><td colspan="8" class="muted">None.</td></tr>`;
}

// Flow has three views, each answering one question:
//   Accounts: what happened in each account (this file, below)
//   Custody:  how coins moved between accounts over time (timelinegraph.js)
//   Coins:    which coin to spend next, and what can be linked (coins.js)
const KIND_NAMES = { buy: "Buy", sell: "Sale", income: "Income", transfer: "Transfer", consolidation: "Consolidation", unmatched: "Needs a label", spend: "Spent",
  sold: "Sold to a person", gift: "Gift sent", donation: "Donation", lost: "Lost or stolen", untracked: "Untracked wallet", "self-transfer": "Self transfer",
  "gift-in": "Gift received", purchase: "Bought from a person", refund: "Refund" };
const flowViews = {};
let flowMode = (() => { try { const m = localStorage.getItem("flowMode"); return ["accounts", "custody"].includes(m) ? m : "accounts"; } catch { return "accounts"; } })();
// scheduleDraw redraws Flow once on the next frame, however many times it's
// asked before then: a pan or pinch sends far more moves than a phone can
// redraw the timeline for, and drawing on each one froze the page.
let drawQueued = 0;
function scheduleDraw() {
  if (!drawQueued) drawQueued = requestAnimationFrame(() => { drawQueued = 0; drawGraph(); });
}
function drawGraph() {
  if (!report || !$("#graph").classList.contains("on")) return;
  const box = { accounts: "#flow", custody: "#tg" };
  for (const [m, sel] of Object.entries(box)) $(sel).style.display = m === flowMode ? "" : "none";
  $$("#graph [data-view]").forEach((el) => (el.style.display = el.dataset.view.split(" ").includes(flowMode) ? "" : "none"));
  $("#graph-wrap").style.overflow = "hidden";
  if (flowMode !== "accounts" && selected && !flowViews[flowMode]?.keepsSelection) selectEvent(null);
  (flowMode === "accounts" ? drawTimeline : flowViews[flowMode] || drawTimeline)();
}
$$("#flow-mode input").forEach((r) => {
  r.checked = r.value === flowMode;
  r.addEventListener("change", () => {
    flowMode = r.value;
    try { localStorage.setItem("flowMode", flowMode); } catch {}
    drawGraph();
  });
});

// Accounts: one lane per account, time left to right. Each lane draws the
// account's balance as a line, with a plain icon on it for every event.
// Moves between accounts show as an "out" icon on one lane and an "in"
// icon on the other, without lines across lanes.
const LEDGER_KEY = [["buy", "●", "Buy"], ["income", "●", "Income or received"], ["in", "▲", "Moved in from your account"], ["out", "▼", "Moved out to your account"],
  ["sell", "▼", "Sold or spent"], ["gift", "▼", "Gift, donation, sent outside"], ["unmatched", "◆", "Needs a label"], ["consol", "■", "Coins merged"]];
$("#legend").innerHTML = LEDGER_KEY.map(([k, g, l]) => `<span><span class="lg ${k}">${g}</span> ${l}</span>`).join("")
  + `<span><svg width="22" height="10"><path d="M1,8 L7,8 L7,4 L15,4 L15,2 L21,2" class="bal-line"/></svg> Balance</span>`;
// On a phone the key folds away behind its button.
$("#flow-key").addEventListener("click", (e) => {
  const on = $("#graph").classList.toggle("show-key");
  e.currentTarget.setAttribute("aria-expanded", on);
  drawGraph();
});
let selected = null;
const view = { d0: 0, d1: 1, full: 1, key: "" }; // visible slice of the compressed timeline
let axis = null; // {times, pos}: event times and their positions on the timeline

function lanesByBand() {
  const kindOf = Object.fromEntries((report.accounts || []).map((a) => [a.name, a.kind]));
  const names = [...new Set([...(report.accounts || []).filter((a) => a.kind !== "pool").map((a) => a.name),
    ...(report.graph || []).flatMap((e) => [e.account, e.to].filter(Boolean))])];
  let saved = [];
  try { saved = JSON.parse(localStorage.getItem("laneOrder") || "[]"); } catch {}
  const rank = (n) => { const i = saved.indexOf(n); return i < 0 ? 1e9 : i; };
  const bands = [["Exchanges", "exchange"], ["Wallets", "wallet"], ["Lightning", "lightning"], ["Other", null]].map(([label, k]) => ({
    label, lanes: names.filter((n) => (k ? kindOf[n] === k : !["exchange", "wallet", "lightning"].includes(kindOf[n]))).sort((a, b) => rank(a) - rank(b) || a.localeCompare(b)),
  }));
  return bands.filter((b) => b.lanes.length);
}
// Drag a lane's name up or down to reorder the lanes (within its band).
(() => {
  let lift = null;
  $("#flow").addEventListener("pointerdown", (e) => {
    const name = e.target.closest(".lane-name");
    if (!name) return;
    e.preventDefault(); e.stopPropagation();
    lift = { acct: name.querySelector("title")?.textContent || name.textContent, el: name, over: null };
    name.classList.add("lifting");
  });
  window.addEventListener("pointermove", (e) => {
    if (!lift) return;
    const rect = $("#flow").getBoundingClientRect(), y = e.clientY - rect.top;
    let best = null;
    for (const t of $$("#flow .lane-name")) { const ty = Number(t.getAttribute("y")); if (best == null || Math.abs(ty - y) < Math.abs(Number(best.getAttribute("y")) - y)) best = t; }
    lift.over = best && best !== lift.el ? (best.querySelector("title")?.textContent || best.textContent) : null;
    $$("#flow .lane-drop").forEach((l) => l.remove());
    if (best && lift.over) best.insertAdjacentHTML("beforebegin", `<line class="lane-drop" x1="4" x2="${LEFT - 4}" y1="${Number(best.getAttribute("y")) - 22}" y2="${Number(best.getAttribute("y")) - 22}"/>`);
  });
  window.addEventListener("pointerup", () => {
    if (!lift) return;
    const { acct, over } = lift;
    lift = null;
    $$("#flow .lane-drop").forEach((l) => l.remove());
    if (!over || over === acct) { drawGraph(); return; }
    const order = lanesByBand().flatMap((b) => b.lanes).filter((n) => n !== acct);
    order.splice(order.indexOf(over), 0, acct);
    try { localStorage.setItem("laneOrder", JSON.stringify(order)); } catch {}
    drawGraph();
  });
})();

// Every distinct moment gets room and quiet stretches are compressed (log
// of the gap), so events minutes apart don't pile up and empty years don't
// waste the width.
function buildAxis(ev) {
  const times = [...new Set(ev.map((e) => e.t))].sort((a, b) => a - b);
  const pos = [0];
  for (let i = 1; i < times.length; i++) pos.push(pos[i - 1] + 1 + 0.9 * Math.log2(1 + (times[i] - times[i - 1]) / 86400));
  return { times, pos };
}
function posOf(t) {
  const { times, pos } = axis;
  if (t <= times[0]) return pos[0] - (times[0] - t) / 86400 / 30;
  if (t >= times.at(-1)) return pos.at(-1) + (t - times.at(-1)) / 86400 / 30;
  let lo = 0, hi = times.length - 1;
  while (hi - lo > 1) { const m = (lo + hi) >> 1; if (times[m] <= t) lo = m; else hi = m; }
  return pos[lo] + ((t - times[lo]) / (times[hi] - times[lo] || 1)) * (pos[hi] - pos[lo]);
}
function timeOf(p) {
  const { times, pos } = axis;
  if (p <= pos[0]) return times[0];
  if (p >= pos.at(-1)) return times.at(-1);
  let lo = 0, hi = pos.length - 1;
  while (hi - lo > 1) { const m = (lo + hi) >> 1; if (pos[m] <= p) lo = m; else hi = m; }
  return times[lo] + ((p - pos[lo]) / (pos[hi] - pos[lo] || 1)) * (times[hi] - times[lo]);
}

let LEFT = 150; // the account names' column
function geometry() {
  const wrap = $("#graph-wrap"), narrow = matchMedia("(max-width: 760px)").matches;
  LEFT = narrow ? 104 : 150;
  return { W: Math.max(narrow ? 300 : 400, wrap.clientWidth), H: Math.max(260, wrap.clientHeight) };
}
const xOf = (p, W) => LEFT + 20 + ((p - view.d0) / (view.d1 - view.d0)) * (W - LEFT - 40);
const pOfX = (x, W) => view.d0 + ((x - LEFT - 20) / (W - LEFT - 40)) * (view.d1 - view.d0);

function ticks(t0, t1) {
  const span = t1 - t0, out = [];
  const d = new Date(t0 * 1000);
  if (span > 3 * 365 * 86400) {
    for (let y = d.getUTCFullYear() + 1; Date.UTC(y, 0, 1) / 1000 <= t1; y++) out.push([Date.UTC(y, 0, 1) / 1000, String(y), true]);
  } else if (span > 60 * 86400) {
    for (let m = new Date(Date.UTC(d.getUTCFullYear(), d.getUTCMonth() + 1, 1)); m / 1000 <= t1; m.setUTCMonth(m.getUTCMonth() + 1)) {
      const jan = m.getUTCMonth() === 0;
      out.push([m / 1000, jan ? String(m.getUTCFullYear()) : m.toLocaleString("en-US", { month: "short", timeZone: "UTC" }), jan]);
    }
  } else {
    const step = span > 14 * 86400 ? 7 : 1;
    for (let x = new Date(Date.UTC(d.getUTCFullYear(), d.getUTCMonth(), d.getUTCDate() + 1)); x / 1000 <= t1; x.setUTCDate(x.getUTCDate() + step)) {
      out.push([x / 1000, x.toISOString().slice(5, 10), x.getUTCDate() === 1]);
    }
  }
  return out;
}

// Each account's balance after every event that touches it, in time order.
// The same running total the custody view uses; unlabeled outflows count as
// gone from the account, as they are on chain.
function accountBalances(ev) {
  const sorted = ev.slice().sort((a, b) => a.t - b.t || (a.key < b.key ? -1 : 1));
  const bal = {}, series = {};
  const add = (a, v, t, e, dir) => { if (!a) return; bal[a] = (bal[a] || 0) + v; (series[a] ||= []).push({ t, bal: bal[a], e, dir }); };
  for (const e of sorted) {
    if (e.kind === "transfer" && e.to) { add(e.account, -(e.sats + (e.feeSats || 0)), e.t, e, "out"); add(e.to, e.sats, e.t, e, "in"); }
    else if (e.kind === "consolidation") add(e.account, -(e.feeSats || 0), e.t, e, "consol");
    else if (e.kind === "buy" || e.kind === "income" || e.dir === "in") add(e.account, e.sats, e.t, e, "in");
    else add(e.account, -(e.sats + (e.kind === "sell" ? 0 : e.feeSats || 0)), e.t, e, "out");
  }
  return series;
}
function ledgerIcon(p) {
  const e = p.e;
  if (e.kind === "transfer") return p.dir === "in" ? ["in", "▲"] : ["out", "▼"];
  if (e.kind === "consolidation") return ["consol", "■"];
  if (e.kind === "unmatched") return ["unmatched", "◆"];
  if (e.kind === "buy") return ["buy", "●"];
  if (e.kind === "income" || e.dir === "in") return ["income", "●"];
  if (["sell", "spend", "sold"].includes(e.kind)) return ["sell", "▼"];
  return ["gift", "▼"];
}

function drawTimeline() {
  const svg = $("#flow");
  if (!report || !$("#graph").classList.contains("on")) return;
  const ev = report.graph || [];
  if (!ev.length) { svg.setAttribute("width", 600); svg.setAttribute("height", 60); svg.innerHTML = `<text x="16" y="34">${esc(t("Nothing to draw yet."))}</text>`; return; }
  const sig = ev.length + ":" + ev[0].t + ":" + ev.at(-1).t;
  if (view.key !== sig) { // new data: rebuild the timeline and show everything
    axis = buildAxis(ev);
    view.full = axis.pos.at(-1) || 1;
    view.d0 = -view.full * 0.01; view.d1 = view.full * 1.01; view.key = sig;
    accountBalances.cache = accountBalances(ev);
  }
  const series = accountBalances.cache;
  const { W, H } = geometry();
  const bands = lanesByBand();
  const nLanes = bands.reduce((n, b) => n + b.lanes.length, 0);
  const narrow = matchMedia("(max-width: 760px)").matches, head = narrow ? 16 : 22;
  const laneH = Math.max(narrow ? 36 : 40, Math.min(narrow ? 60 : 110, (H - 40 - bands.length * (head + 12)) / nLanes));
  const laneTop = {};
  let yy = narrow ? 2 : 6, out = "", labels = "";
  for (const b of bands) {
    const top = yy;
    yy += head;
    for (const l of b.lanes) { laneTop[l] = yy; yy += laneH; }
    out += `<rect class="band" x="0" y="${top}" width="${W}" height="${yy - top}" rx="8"/>`;
    labels += `<text class="band-label" x="10" y="${top + (narrow ? 12 : 15)}">${esc(t(b.label))}</text>`;
    yy += narrow ? 8 : 12;
  }
  const nameY = (top) => narrow ? top + 14 : top + laneH / 2;
  const bottom = yy;
  const visible = (p) => p >= view.d0 - (view.d1 - view.d0) * 0.02 && p <= view.d1 + (view.d1 - view.d0) * 0.02;
  const tt0 = timeOf(Math.max(view.d0, 0)), tt1 = timeOf(Math.min(view.d1, view.full));
  let lastLabel = -1e9;
  for (const [t, label, major] of ticks(tt0, tt1)) {
    const xx = xOf(posOf(t), W);
    if (xx < LEFT + 10 || xx > W - 10) continue;
    out += `<line class="lane${major ? "" : " minor"}" x1="${xx}" x2="${xx}" y1="6" y2="${bottom}" stroke-dasharray="2 4"/>`;
    if (xx - lastLabel > (major ? 34 : 44)) { out += `<text x="${xx + 3}" y="${bottom + 14}"${major ? ' class="major"' : ""}>${label}</text>`; lastLabel = xx; }
  }
  const nowBal = {};
  for (const [acct, top] of Object.entries(laneTop)) {
    const pts = series[acct] || [];
    const maxB = Math.max(...pts.map((p) => p.bal), 1);
    const yOf = (b) => top + laneH - 8 - (Math.max(b, 0) / maxB) * (laneH - 22);
    out += `<line class="lane" x1="${LEFT}" x2="${W - 10}" y1="${top + laneH - 8}" y2="${top + laneH - 8}"/>`;
    // Step line of the balance, clipped to the visible span.
    let d = "", prevY = yOf(0), started = false;
    for (const p of pts) {
      const x = Math.min(Math.max(xOf(posOf(p.t), W), LEFT), W - 10), y = yOf(p.bal);
      d += started ? `L${x},${prevY}L${x},${y}` : `M${LEFT},${prevY}L${x},${prevY}L${x},${y}`;
      started = true; prevY = y;
    }
    if (started) {
      d += `L${W - 10},${prevY}`;
      out += `<path class="bal-area" d="${d}L${W - 10},${top + laneH - 8}L${LEFT},${top + laneH - 8}Z"/><path class="bal-line" d="${d}"/>`;
    }
    // The line stops at zero, so ring the point where it first went below.
    const dip = pts.find((p) => p.bal < 0);
    if (dip && visible(posOf(dip.t))) out += `<circle class="dip" cx="${xOf(posOf(dip.t), W)}" cy="${yOf(0)}" r="9"><title>${esc(t(`Below zero from ${day(dip.t)}: ${amt(dip.bal)}`))}</title></circle>`;
    for (const p of pts) {
      const pp = posOf(p.t);
      if (!visible(pp)) continue;
      const [cls, glyph] = ledgerIcon(p);
      const e = p.e;
      const what = e.kind === "transfer" ? (p.dir === "in" ? `Moved in from ${e.account}` : `Moved out to ${e.to}`) : KIND_NAMES[e.label || e.kind] || e.kind;
      out += `<text class="lg-icon ${cls}" data-key="${esc(e.key)}" x="${xOf(pp, W)}" y="${yOf(p.bal) + 4}" text-anchor="middle"><title>${esc(t(`${what}: ${amt(e.sats)}\n${day(e.t)}\nBalance after: ${amt(p.bal)}`))}</title>${glyph}</text>`;
    }
    nowBal[acct] = pts.length ? pts.at(-1).bal : 0;
  }
  // Lane names and the current balance stay put while the timeline pans.
  const nx = narrow ? 8 : 16, nmax = narrow ? 14 : 19;
  labels += Object.entries(laneTop).map(([l, top]) => `<text class="lane-name" x="${nx}" y="${nameY(top)}"><title>${esc(l)}</title>${esc(l.length > nmax ? l.slice(0, nmax - 1) + "…" : l)}</text>`
    + `<text class="lane-bal${(nowBal[l] || 0) < 0 ? " neg" : ""}" x="${nx}" y="${nameY(top) + 14}">${amt(nowBal[l] || 0)}</text>`).join("");
  out += `<rect class="gutter" x="0" y="0" width="${LEFT}" height="${bottom + 20}"/>` + labels;
  svg.setAttribute("width", W);
  svg.setAttribute("height", H);
  svg.setAttribute("viewBox", `0 0 ${W} ${H}`);
  svg.innerHTML = out;
  const span = timeOf(Math.min(view.d1, view.full)) - timeOf(Math.max(view.d0, 0));
  $("#zoom-level").textContent = span > 2 * 365 * 86400 ? `${(span / 365 / 86400).toFixed(1)} yrs` : span > 60 * 86400 ? `${Math.round(span / 30 / 86400)} mos` : `${Math.max(1, Math.round(span / 86400))} days`;
  if (selected && ev.some((e) => e.key === selected)) highlight(selected);
}

// Zoom keeps the point under the pointer still.
function zoomAt(px, factor) {
  const { W } = geometry();
  const p = pOfX(px, W);
  let span = (view.d1 - view.d0) * factor;
  span = Math.max(0.5, Math.min(view.full * 1.05, span));
  const f = (p - view.d0) / (view.d1 - view.d0);
  view.d0 = p - f * span; view.d1 = view.d0 + span;
  clampView();
  scheduleDraw();
}
function clampView() {
  const span = view.d1 - view.d0, pad = view.full * 0.02;
  if (view.d0 < -pad) { view.d0 = -pad; view.d1 = view.d0 + span; }
  if (view.d1 > view.full + pad) { view.d1 = view.full + pad; view.d0 = view.d1 - span; }
}
let anim = 0;
function animateTo(d0, d1) {
  const s0 = view.d0, s1 = view.d1, start = performance.now();
  cancelAnimationFrame(anim);
  const step = (now) => {
    const k = Math.min(1, (now - start) / 350), e = 1 - Math.pow(1 - k, 3);
    view.d0 = s0 + (d0 - s0) * e; view.d1 = s1 + (d1 - s1) * e;
    drawGraph();
    if (k < 1) anim = requestAnimationFrame(step);
  };
  anim = requestAnimationFrame(step);
}
const svgEl = $("#flow");
svgEl.addEventListener("wheel", (e) => {
  e.preventDefault();
  const rect = svgEl.getBoundingClientRect();
  if (e.shiftKey || Math.abs(e.deltaX) > Math.abs(e.deltaY)) { // horizontal scroll pans
    const { W } = geometry(), d = ((e.deltaX || e.deltaY) / (W - LEFT)) * (view.d1 - view.d0);
    view.d0 += d; view.d1 += d; clampView(); scheduleDraw();
    return;
  }
  zoomAt(e.clientX - rect.left, Math.exp(e.deltaY * 0.0015));
}, { passive: false });
let drag = null;
svgEl.addEventListener("pointerdown", (e) => { if (e.target.closest(".lane-name")) return; drag = { x: e.clientX, d0: view.d0, d1: view.d1, moved: false }; });
window.addEventListener("pointermove", (e) => {
  if (!drag) return;
  const dx = e.clientX - drag.x;
  if (Math.abs(dx) > 3) { drag.moved = true; svgEl.classList.add("panning"); }
  if (!drag.moved) return;
  const { W } = geometry(), d = (-dx / (W - LEFT - 40)) * (drag.d1 - drag.d0);
  view.d0 = drag.d0 + d; view.d1 = drag.d1 + d; clampView(); scheduleDraw();
});
window.addEventListener("pointerup", () => { setTimeout(() => (drag = null), 0); svgEl.classList.remove("panning"); });
// Pinch zooms Flow around the fingers, and moving them pans.
(() => {
  let s = null;
  const frac = (cx) => { const { W } = geometry(); return (cx - svgEl.getBoundingClientRect().left - LEFT - 20) / (W - LEFT - 40); };
  pinchZoom(svgEl, (cx) => {
    drag = null;
    svgEl.classList.remove("panning");
    s = { d0: view.d0, d1: view.d1 };
    s.at = s.d0 + frac(cx) * (s.d1 - s.d0);
  }, (k, cx) => {
    const span = Math.max(0.5, Math.min(view.full * 1.05, (s.d1 - s.d0) * k));
    view.d0 = s.at - frac(cx) * span; view.d1 = view.d0 + span;
    clampView();
    scheduleDraw();
  });
})();
$("#zoom-in").addEventListener("click", () => zoomAt(geometry().W / 2 + LEFT / 2, 0.5));
$("#zoom-out").addEventListener("click", () => zoomAt(geometry().W / 2 + LEFT / 2, 2));
$("#zoom-fit").addEventListener("click", () => animateTo(-view.full * 0.01, view.full * 1.01));

// The likely route coins took between two accounts: a chain of transfers,
// each later than the last, from where they were acquired to where they
// were sold. Used only to highlight the path; basis comes from the engine.
function route(from, t0, to, t1) {
  const ts = (report.graph || []).filter((e) => e.kind === "transfer" && e.to && e.t >= t0 && e.t <= t1);
  const walk = (acct, t, seen) => {
    if (acct === to) return [];
    if (seen.size > 6) return null;
    for (const e of ts) {
      if (e.account !== acct || e.t < t || seen.has(e.to)) continue;
      const rest = walk(e.to, e.t, new Set([...seen, e.to]));
      if (rest) return [e, ...rest];
    }
    return null;
  };
  return walk(from, t0, new Set([from])) || [];
}
const tsec = (iso) => Math.floor(new Date(iso).getTime() / 1000);
const routeText = (from, hops) => hops.length ? `<div class="small muted">Route: ${[from, ...hops.map((h) => h.to)].map(esc).join(" → ")}</div>` : "";
const whenFmt = {};
const when = (t) => {
  const z = tzName();
  whenFmt[z] ||= new Intl.DateTimeFormat("en-CA", { timeZone: z, year: "numeric", month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit", hourCycle: "h23", timeZoneName: "short" });
  return whenFmt[z].format(new Date(t * 1000)).replace(",", "");
};
const pickKey = (k) => `<button class="link" data-pick="${esc(k)}">`;
let relatedKeys = new Set();

// The transaction view: what happened, plus where the coins came from
// (sales, spends, gifts) or where they went (buys, income, gifts received).
// buildDetail gathers what is known about one record: its facts, what it
// traces to, and the two sides of its diagram.
function buildDetail(key) {
  const e = (report.graph || []).find((g) => g.key === key);
  if (!e) return null;
  const disposals = report.disposals || [], removals = report.removals || [], open = report.openLots || [];
  const related = new Set([key]);
  const byKey = Object.fromEntries((report.graph || []).map((g) => [g.key, g]));
  const via = (hops) => { hops.forEach((h) => related.add(h.key)); return hops; };
  const rows = [["When", when(e.t)], ["Account", esc(e.account) + (e.to ? ` → ${esc(e.to)}` : "")], ["Amount", `${amt(e.sats)}`]];
  if (e.cents) rows.push([e.kind === "sell" || e.kind === "sold" ? "Proceeds" : e.kind === "buy" ? "Cost" : "Value", usd(e.cents) + (e.sats ? ` <span class="muted small">(${usd(Math.round(e.cents * 1e8 / e.sats))}/BTC)</span>` : "")]);
  else if (e.value) rows.push(["Market value", usd(e.value) + ` <span class="muted small">at that day's price</span>`]);
  if (e.valueSource) rows.push(["Value from", esc(e.valueSource)]);
  if (e.feeCents) rows.push(["Fee (USD)", usd(e.feeCents)]);
  if (e.feeSats) rows.push(["Network fee", `${amt(e.feeSats)}`]);
  if (e.inputs) rows.push(["Shape", `${e.inputs} input${e.inputs === 1 ? "" : "s"} → ${e.outputs} output${e.outputs === 1 ? "" : "s"}`]);
  if (e.ref) rows.push(["Reference", `<span class="mono">${esc(e.ref)}</span> <span class="small muted">how exports name this record</span>`]);
  if (e.method) rows.push(["Matched by", esc(e.method) + (e.toKey && e.method !== "manual" && e.method !== "same transaction"
    ? ` <button class="link small" data-autounlink="${esc(e.key)}" data-autounlink2="${esc(e.toKey)}">Unlink these</button>` : "")]);
  if (e.txid) rows.push(["Transaction", `<a class="mono small" href="#tx/${esc(e.txid)}">${esc(e.txid)}</a>`]);
  if (e.address) rows.push(["Address", `<a class="mono small" href="#addr/${encodeURIComponent(e.address)}">${esc(e.address)}</a>`]);
  if (e.note) rows.push(["Note", esc(e.note)]);

  let trace = "";
  const sameTx = e.txid ? (report.graph || []).filter((g) => g.txid === e.txid && g.key !== key) : [];
  if (sameTx.length) {
    trace += `<b>Same transaction</b>` + sameTx.map((g) => { related.add(g.key);
      return `<div>${pickKey(g.key)}${esc(KIND_NAMES[g.kind] || g.kind)}</button> ${amt(g.sats)} ${esc(g.account)}${g.to ? ` → ${esc(g.to)}` : ""}</div>`; }).join("");
  }
  const fromLots = disposals.filter((d) => d.OpID === key);
  const fromGifts = removals.filter((d) => d.OpID === key);
  if (fromLots.length || fromGifts.length) {
    trace += `<b>Came from</b>` + [...fromLots, ...fromGifts].map((d) => {
      related.add(d.OriginID);
      if (d.Missing) return `<div class="warn">${amt(d.Sats)} with no known purchase</div>`;
      const gain = d.Proceeds != null ? `, gain ${signed(d.Proceeds - d.Basis)} (${d.LongTerm ? "long" : "short"} term)` : "";
      const o = byKey[d.OriginID], hops = o ? via(route(o.account, o.t, e.account, e.t)) : [];
      return `<div>${amt(d.Sats)} ${pickKey(d.OriginID)}acquired ${day(d.Acquired)}</button>${o ? ` on ${esc(o.account)}` : ""}, basis ${usd(d.Basis)}${gain}${routeText(o?.account, hops)}</div>`;
    }).join("");
  }
  const sold = disposals.filter((d) => d.OriginID === key), gone = removals.filter((d) => d.OriginID === key), held = open.filter((l) => l.OriginID === key);
  if (sold.length || gone.length || held.length) {
    trace += `<b>Where these coins went</b>`
      + sold.map((d) => { related.add(d.OpID); const hops = via(route(e.account, e.t, d.Account, tsec(d.Disposed)));
        return `<div>${amt(d.Sats)} ${pickKey(d.OpID)}sold ${day(d.Disposed)}</button> from ${esc(d.Account)}, gain ${signed(d.Proceeds - d.Basis)}${routeText(e.account, hops)}</div>`; }).join("")
      + gone.map((d) => { related.add(d.OpID); const hops = via(route(e.account, e.t, d.Account, tsec(d.Removed)));
        return `<div>${amt(d.Sats)} ${pickKey(d.OpID)}left ${day(d.Removed)}</button> from ${esc(d.Account)} (gift, donation or loss)${routeText(e.account, hops)}</div>`; }).join("")
      + held.map((l) => { const hops = l.Account ? via(route(e.account, e.t, l.Account, Infinity)) : [];
        return `<div>${amt(l.Sats)} still held in ${l.Account ? esc(l.Account) : "the pre-2025 pool"}, basis ${usd(l.Basis)}${routeText(e.account, hops)}</div>`; }).join("");
  }
  if (e.kind === "transfer") trace += `<b>Moved between your accounts</b><div>${esc(e.account)} → ${esc(e.to)}. Not taxable; cost basis and purchase dates move with the coins${e.feeSats ? ". The network fee is added to basis or treated as a small sale, per Settings" : ""}.</div>`;
  if (e.kind === "consolidation") trace += `<b>Coins merged inside ${esc(e.account)}</b><div>Not taxable. Only the network fee left the wallet, handled per Settings.</div>`;
  if (e.kind === "unmatched") trace += `<b>Not traced yet</b><div>This left or entered a tracked account and couldn't be matched. Label it below or in <a href="#review">Review</a>.</div>`;
  if (!trace) trace = `<span class="muted">Nothing further to trace.</span>`;
  const sides = txSides(e, { fromLots, fromGifts, sold, gone, held, byKey });
  return { e, rows, trace, related, sides, byKey, fromLots, fromGifts, sold, gone, held, sameTx };
}
const pageLink = (e) => `#tx/rec:${encodeURIComponent(e.key)}`;

function selectEvent(key, zoom) {
  const box = $("#graph-detail");
  const d = key ? buildDetail(key) : null;
  if (!d) {
    const was = box.open;
    selected = null;
    relatedKeys = new Set();
    if (was) box.close();
    highlight(null);
    return;
  }
  selected = key;
  box.innerHTML = `<div class="detail-inner">${renderRecord(d, { compact: true })}</div>`;
  if (!box.open) box.showModal();
  relatedKeys = d.related;
  highlight(key);
  if (zoom && onFlow()) zoomToKeys(d.related, d.byKey);
}
const onFlow = () => $("#graph").classList.contains("on");
function zoomToKeys(keys, byKey) {
  const ps = [...keys].map((k) => byKey[k]).filter(Boolean).map((g) => posOf(g.t));
  let lo = Math.min(...ps), hi = Math.max(...ps);
  const minSpan = Math.max(8, view.full * 0.01);
  if (hi - lo < minSpan) { const m = (lo + hi) / 2; lo = m - minSpan / 2; hi = m + minSpan / 2; }
  const pad = (hi - lo) * 0.12;
  animateTo(lo - pad, hi + pad);
}
// "Show related on the graph": the popup steps aside and the chain stays
// highlighted until the graph is clicked or Escape pressed.
let keepPick = false;
function showOnGraph() {
  const key = selected, d = key && buildDetail(key);
  if (!d) return;
  if (!onFlow()) {
    // From Overview: go to Flow, with the chain highlighted there.
    location.hash = "graph";
    setTimeout(() => { selected = key; relatedKeys = d.related; highlight(key); zoomToKeys(d.related, d.byKey); }, 60);
    return;
  }
  keepPick = true;
  $("#graph-detail").close();
  zoomToKeys(d.related, d.byKey);
}
// ← and → in the popup step to the previous or next record in its lane.
document.addEventListener("keydown", (ev) => {
  if (!["ArrowLeft", "ArrowRight"].includes(ev.key) || !$("#graph-detail").open || ev.target.closest?.("input, select, textarea") || !selected) return;
  const cur = (report.graph || []).find((g) => g.key === selected);
  if (!cur) return;
  const lane = (report.graph || []).filter((g) => g.account === cur.account).sort((a, b) => a.t - b.t || (a.key < b.key ? -1 : 1));
  const next = lane[lane.indexOf(cur) + (ev.key === "ArrowRight" ? 1 : -1)];
  ev.preventDefault();
  if (next) selectEvent(next.key, false);
});
// The transaction diagram under the details, in the spirit of mempool.space:
// where the coins came from on the left, where they went on the right, band
// widths by amount. Only records related to the selected one appear.
let lastFlow = null;
function txSides(e, c) {
  const L = [], R = [];
  const add = (side, n) => { const f = side.find((x) => x.id === n.id); if (f) f.sats += n.sats; else side.push(n); };
  const legs = e.txid ? (report.graph || []).filter((g) => g.txid === e.txid) : [e];
  const fee = legs.reduce((m, g) => Math.max(m, g.feeSats || 0), 0);
  // Left: lots a sale came from; else the wallets that funded the transaction.
  if (c.fromLots.length || c.fromGifts.length) {
    for (const d of [...c.fromLots, ...c.fromGifts]) {
      const o = c.byKey[d.OriginID];
      add(L, d.Missing ? { id: "missing", label: "Unknown origin", sub: "no purchase on record", sats: d.Sats, kind: "unmatched" }
        : { id: d.OriginID, key: o ? d.OriginID : null, label: `${KIND_NAMES[o?.kind] || "Acquired"} ${day(d.Acquired)}`, sub: o ? o.account : "", sats: d.Sats, kind: o?.kind || "buy" });
    }
  } else if (e.kind === "buy" || e.kind === "income" || ["purchase", "gift-in", "refund"].includes(e.kind)) {
    add(L, { id: "src", label: e.kind === "buy" ? `Paid ${e.cents ? usd(e.cents) : ""}` : KIND_NAMES[e.kind], sub: e.account, sats: e.sats, kind: e.kind });
  } else if (e.kind === "unmatched" && !e.to && !legs.some((g) => g.kind === "transfer")) {
    add(L, { id: "src", label: "From outside", sub: "needs a label", sats: e.sats, kind: "unmatched" });
  } else {
    const from = legs.filter((g) => g.key.includes(":from:"));
    const out = legs.filter((g) => !g.key.includes(":from:"));
    const total = out.reduce((s, g) => s + g.sats, 0) + fee;
    from.forEach((g) => add(L, { id: "a:" + g.account, key: g.key, label: g.account, sub: "funded part of it", sats: g.sats, kind: "acct" }));
    const sender = (out.find((g) => g.kind === "transfer") || out[0] || e).account;
    add(L, { id: "a:" + sender, label: sender, sub: from.length ? "sent it" : e.inputs ? `${e.inputs} input${e.inputs === 1 ? "" : "s"}` : "", sats: Math.max(total - from.reduce((s, g) => s + g.sats, 0), 0), kind: "acct" });
  }
  // Right: where they went.
  if (c.sold.length || c.gone.length || c.held.length) {
    c.sold.forEach((d) => add(R, { id: d.OpID, key: d.OpID, label: `Sold ${day(d.Disposed)}`, sub: d.Account, sats: d.Sats, kind: c.byKey[d.OpID]?.kind || "sell" }));
    c.gone.forEach((d) => add(R, { id: d.OpID, key: d.OpID, label: `Left ${day(d.Removed)}`, sub: d.Account, sats: d.Sats, kind: "other" }));
    c.held.forEach((l) => add(R, { id: "held:" + l.Account, label: `Still held`, sub: l.Account || "pre-2025 pool", sats: l.Sats, kind: "acct" }));
  } else if (e.kind === "sell" || e.kind === "sold") {
    add(R, { id: "proceeds", label: `Proceeds ${e.cents ? usd(e.cents) : ""}`, sub: e.account, sats: e.sats, kind: "sell" });
  } else if (legs.length > 1 || e.kind === "transfer" || e.kind === "consolidation") {
    legs.filter((g) => !g.key.includes(":from:")).forEach((g) => add(R, g.kind === "transfer"
      ? { id: g.key, key: g.key, label: g.to, sub: "transfer", sats: g.sats, kind: "acct" }
      : g.kind === "consolidation" ? { id: g.key, key: g.key, label: g.account, sub: "merged, stays in the wallet", sats: g.sats, kind: "consolidation" }
      : { id: g.key, key: g.key, label: "Outside", sub: KIND_NAMES[g.kind] || g.kind, sats: g.sats, kind: g.kind }));
  } else {
    add(R, { id: "dst", label: e.kind === "unmatched" ? "Outside" : KIND_NAMES[e.kind] || e.kind, sub: e.kind === "unmatched" ? "needs a label" : e.account, sats: e.sats, kind: e.kind });
  }
  if (fee > 0 && !(c.fromLots.length && e.kind !== "transfer")) add(R, { id: "fee", label: "Network fee", sub: "", sats: fee, kind: "fee" });
  // Keep it readable: the biggest nine, then one "more" node.
  const cap = (side) => {
    if (side.length <= 10) return side;
    side.sort((a, b) => b.sats - a.sats);
    const rest = side.slice(9);
    return [...side.slice(0, 9), { id: "more", label: `${rest.length} more`, sub: "smaller amounts", sats: rest.reduce((s, n) => s + n.sats, 0), kind: "other" }];
  };
  return { L: cap(L), R: cap(R) };
}

function drawTxFlow(e, { L, R }, svg = $("#txflow")) {
  if (!svg) return;
  const W = Math.max(300, svg.parentElement.clientWidth), labelW = Math.min(200, W * 0.34), narrow = W < 480;
  const n = Math.max(L.length, R.length, 1), gap = 6;
  const H = Math.max(140, n * 34 + 40), avail = H - 40 - gap * (n - 1);
  const tl = L.reduce((s, x) => s + x.sats, 0), tr = R.reduce((s, x) => s + x.sats, 0), total = Math.max(tl, tr, 1);
  const k = avail / total, hgt = (s) => Math.max(4, s * k);
  const xL = labelW, xR = W - labelW - 12, cx = W / 2 - 7, nodeW = 12, top = 26;
  const place = (side) => { let y = top + (avail + gap * (n - 1) - (side.reduce((s, x) => s + hgt(x.sats), 0) + gap * (side.length - 1))) / 2;
    return side.map((x) => { const r = { ...x, y, h: hgt(x.sats) }; y += r.h + gap; return r; }); };
  const lp = place(L), rp = place(R);
  const cTop = top, cH = Math.max(lp.reduce((s, x) => s + x.h, 0), rp.reduce((s, x) => s + x.h, 0), 20);
  const cy0 = cTop + (avail + gap * (n - 1) - cH) / 2;
  let g = `<text class="col" x="${xL}" y="14" text-anchor="end">From</text><text class="col" x="${xR + nodeW}" y="14">To</text>`;
  let yc = cy0;
  for (const x of lp) {
    const y2 = yc; yc += x.h;
    g += `<path class="band f-${esc(x.kind)}" d="M${xL + nodeW},${x.y} C${(xL + cx) / 2},${x.y} ${(xL + cx) / 2},${y2} ${cx},${y2} L${cx},${y2 + x.h} C${(xL + cx) / 2},${y2 + x.h} ${(xL + cx) / 2},${x.y + x.h} ${xL + nodeW},${x.y + x.h}Z"/>`;
  }
  yc = cy0;
  for (const x of rp) {
    const y1 = yc; yc += x.h;
    g += `<path class="band f-${esc(x.kind)}" d="M${cx + 14},${y1} C${(cx + xR) / 2},${y1} ${(cx + xR) / 2},${x.y} ${xR},${x.y} L${xR},${x.y + x.h} C${(cx + xR) / 2},${x.y + x.h} ${(cx + xR) / 2},${y1 + x.h} ${cx + 14},${y1 + x.h}Z"/>`;
  }
  const node = (x, nx, anchor, tx) => `<g class="node${x.key ? " link-node" : ""}"${x.key ? ` data-flowkey="${esc(x.key)}"` : ""}><title>${esc(x.label)} ${amt(x.sats)}${x.sub ? ", " + esc(x.sub) : ""}</title>
    <rect class="f-${esc(x.kind)}" x="${nx}" y="${x.y}" width="${nodeW}" height="${x.h}" rx="2"/>
    <text x="${tx}" y="${x.y + x.h / 2 - 1}" text-anchor="${anchor}">${esc(x.label)}</text>
    <text class="sub" x="${tx}" y="${x.y + x.h / 2 + 12}" text-anchor="${anchor}">${amt(x.sats)}${x.sub && !narrow ? " · " + esc(x.sub) : ""}</text></g>`;
  g += lp.map((x) => node(x, xL, "end", xL - 6)).join("") + rp.map((x) => node(x, xR, "start", xR + nodeW + 6)).join("");
  g += `<rect class="center" x="${cx}" y="${cy0}" width="14" height="${cH}" rx="3"><title>${esc(KIND_NAMES[e.kind] || e.kind)} ${amt(e.sats)}</title></rect>
    <text x="${cx + 7}" y="${cy0 + cH + 16}" text-anchor="middle" class="sub">${esc(KIND_NAMES[e.kind] || e.kind)} · ${day(e.t)}</text>`;
  svg.setAttribute("width", W); svg.setAttribute("height", H + 10); svg.setAttribute("viewBox", `0 0 ${W} ${H + 10}`);
  svg.innerHTML = g;
}
$("#graph-detail").addEventListener("click", (e) => {
  const k = e.target.closest("[data-flowkey]")?.dataset.flowkey;
  if (k && (report.graph || []).some((g) => g.key === k)) selectEvent(k, true);
});
window.addEventListener("resize", () => { if ($("#lookup").classList.contains("on")) drawLookupFlows(); });

// What can be done to the selected record from here: label it (or change
// or clear its label), and protect a purchase so it is sold last.
const ACQUIRE_KINDS = ["buy", "income", "purchase", "gift-in", "refund", "untracked"];
function actionsFor(e) {
  const parts = [];
  const labelable = e.dir && (e.kind === "unmatched" || e.label);
  if (labelable) {
    const cats = e.dir === "out" ? OUT_CATS : IN_CATS;
    parts.push(`<span>${e.label ? `Labeled <b>${esc(KIND_NAMES[e.label] || e.label)}</b>.` : "What was it?"}</span>
      <select class="lb-cat"><option value="">${e.label ? "Change to…" : "Choose…"}</option>${cats.filter(([v]) => v !== "self").map(([v, l]) => `<option value="${v}">${esc(l)}</option>`).join("")}</select>
      <input class="lb-amt" inputmode="decimal" size="11" hidden placeholder="${e.value ? usd(e.value) : "USD"}">
      <button class="lb-save primary" hidden>Save</button>${e.label ? `<button class="lb-unlabel link">Remove label</button>` : ""}`);
    // A label that carries a value (income, spend, purchase…): the value is
    // always shown and can be changed on its own, without relabeling.
    const valueName = e.label && (MARKET_VALUE[e.label] || needsAmount[e.label]);
    if (valueName) {
      parts.push(`<span>${esc(valueName)}:</span>
        <input class="lb-val" inputmode="decimal" size="11" placeholder="${e.cents ? usd(e.cents) : e.value ? usd(e.value) : "USD"}">
        <button class="lb-value primary" data-cat="${esc(e.label)}">Save value</button>`);
    }
  }
  if (e.kind === "income" || e.label === "income") {
    // What kind of income, so it lands on the right form.
    const types = report.incomeTypes || {};
    parts.push(`<span>Type:</span><select class="lb-itype">${Object.keys(types).map((t) => `<option value="${t}"${t === e.incomeType ? " selected" : ""}>${esc(t)}</option>`).join("")}</select>
      <span class="small muted">${esc(types[e.incomeType] || "")}</span>`);
  }
  if (["gift", "donation", "gift-in"].includes(e.label)) {
    parts.push(`<span>${e.label === "gift-in" ? "From" : e.label === "donation" ? "Charity" : "To"}:</span>
      <input class="lb-recipient" size="18" value="${esc(e.recipient || "")}" placeholder="Name"><button class="lb-recipient-save">Save</button>`);
  }
  if (e.kind === "income" && e.key.startsWith("ex:")) {
    // Imported income: its value (the basis of these coins) can be corrected.
    parts.push(`<span>Value received${e.label ? " (you set it)" : ""}:</span>
      <input class="lb-val" inputmode="decimal" size="11" placeholder="${e.cents ? usd(e.cents) : "USD"}">
      <button class="lb-value primary" data-cat="income">Save value</button>${e.label ? `<button class="lb-unlabel link">Use the import's value</button>` : ""}`);
  }
  if (ACQUIRE_KINDS.includes(e.kind) || (e.label && ACQUIRE_KINDS.includes(e.label))) {
    parts.push(e.protected
      ? `<span class="badge prot">Protected: sold last</span><button class="lb-protect link" data-on="0">Stop protecting</button>`
      : `<button class="lb-protect" data-on="1" title="Your method will use other coins first and only sell these when nothing else is left">Protect these coins (sell last)</button>`);
  }
  return parts.length ? `<div class="label-box" data-key="${esc(e.key)}">${parts.join('<span class="spacer"></span>')}</div>` : "";
}
document.addEventListener("change", async (e) => {
  if (e.target.classList.contains("lb-itype")) {
    const key = e.target.closest(".label-box").dataset.key;
    try { await post(`/api/labels/${encodeURIComponent(key)}/details`, { IncomeType: e.target.value }); toast("Income type saved"); await refresh(); if ((report.graph || []).some((g) => g.key === key)) selectEvent(key, false); } catch (err) { toast(err.message); }
    return;
  }
  if (!e.target.classList.contains("lb-cat")) return;
  const box = e.target.closest(".label-box"), cat = e.target.value, amt = $(".lb-amt", box);
  amt.hidden = !(MARKET_VALUE[cat] || needsAmount[cat]);
  amt.title = MARKET_VALUE[cat] || needsAmount[cat] || "";
  if (needsAmount[cat]) amt.placeholder = needsAmount[cat];
  $(".lb-save", box).hidden = !cat;
});
document.addEventListener("click", async (e) => {
  const t = e.target, box = t.closest(".label-box");
  if (!box || !(t.classList.contains("lb-save") || t.classList.contains("lb-value") || t.classList.contains("lb-unlabel") || t.classList.contains("lb-protect") || t.classList.contains("lb-recipient-save"))) return;
  const key = box.dataset.key;
  try {
    if (t.classList.contains("lb-recipient-save")) {
      await post(`/api/labels/${encodeURIComponent(key)}/details`, { Recipient: $(".lb-recipient", box).value });
      toast("Saved");
    } else if (t.classList.contains("lb-value")) {
      const amt = Number($(".lb-val", box).value.replace(/[$,\s]/g, ""));
      if (!(amt > 0)) { toast("Enter the value in USD"); return; }
      await post("/api/labels", { Key: key, Category: t.dataset.cat, FiatCents: Math.round(amt * 100) });
      toast("Value saved");
    } else if (t.classList.contains("lb-save")) {
      const label = { Key: key, Category: $(".lb-cat", box).value };
      const amt = $(".lb-amt", box).value.replace(/[$,\s]/g, "");
      if (amt) label.FiatCents = Math.round(Number(amt) * 100);
      await post("/api/labels", label);
      toast("Label saved");
    } else if (t.classList.contains("lb-unlabel")) {
      await api(`/api/labels/${encodeURIComponent(key)}`, { method: "DELETE" });
      toast("Label removed");
    } else {
      await post("/api/protect", { key, on: t.dataset.on === "1" });
      toast(t.dataset.on === "1" ? "Protected: these coins will be sold last" : "No longer protected");
    }
    await refresh();
    if ($("#lookup").classList.contains("on")) renderLookup();
    else if ((report.graph || []).some((g) => g.key === key)) selectEvent(key, false);
  } catch (err) { toast(err.message); }
});

// The transaction page: the on-chain transaction (from your wallets' scans
// or your Fulcrum), each of your records for it with its trace, labels and
// protection, and the coins diagram.
// Files kept as proof for a record (receipts, letters), on its panel.
const attachBox = (key) => `<div class="attach-box small" data-key="${esc(key)}"></div>`;
async function fillAttachments(root) {
  for (const box of $$(".attach-box", root)) {
    let list = [];
    try { list = await api(`/api/attachments?key=${encodeURIComponent(box.dataset.key)}`); } catch { continue; }
    box.innerHTML = `<ul class="att-list">${list.map((a) => `<li><a href="/api/attachments/${a.id}">${esc(a.filename)}</a>${a.note ? ` <span class="muted">(${esc(a.note)})</span>` : ""}
      <button class="link small att-del" data-id="${a.id}" aria-label="Remove ${esc(a.filename)}">✕</button></li>`).join("") || '<li class="muted">None attached yet.</li>'}</ul>
      ${report?.settings?.demo ? `<p class="small muted">Attaching files is off in the demo.</p>` : `<label class="dropzone inline att-add">Drop a receipt or letter here, or <span class="link">choose a file</span><input type="file" hidden></label>
      <input class="att-note" placeholder="Note for the next file (optional)">`}`;
  }
}
async function attachFile(box, file) {
  const fd = new FormData();
  fd.append("key", box.dataset.key);
  fd.append("note", $(".att-note", box)?.value || "");
  fd.append("file", file);
  try { await api("/api/attachments", { method: "POST", body: fd }); toast("File attached"); fillAttachments(box.parentElement); } catch (err) { toast(err.message); }
}
document.addEventListener("change", (e) => {
  if (e.target.closest(".att-add") && e.target.files?.length) attachFile(e.target.closest(".attach-box"), e.target.files[0]);
});
document.addEventListener("dragover", (e) => {
  const z = e.target.closest(".attach-box .dropzone");
  if (!z) return;
  e.preventDefault();
  z.classList.add("over");
});
document.addEventListener("dragleave", (e) => e.target.closest?.(".attach-box .dropzone")?.classList.remove("over"));
document.addEventListener("drop", (e) => {
  const z = e.target.closest(".attach-box .dropzone");
  if (!z) return;
  e.preventDefault();
  z.classList.remove("over");
  if (e.dataTransfer.files.length) attachFile(z.closest(".attach-box"), e.dataTransfer.files[0]);
});
document.addEventListener("click", async (e) => {
  const id = e.target.closest(".att-del")?.dataset.id;
  if (!id) return;
  const box = e.target.closest(".attach-box");
  try { await api(`/api/attachments/${id}`, { method: "DELETE" }); fillAttachments(box.parentElement); } catch (err) { toast(err.message); }
});

async function renderLookup() {
  if (!lookupRoute || !report) return;
  const [kind, arg] = lookupRoute, page = $("#lookup");
  page.innerHTML = `<p class="muted">Looking that up…</p>`;
  try {
    if (kind === "addr") { page.innerHTML = await addressPage(arg); return; }
    page.innerHTML = await txPage(arg);
    drawLookupFlows();
    fillAttachments(page);
  } catch (err) { page.innerHTML = `<h2>Not found</h2><p class="warn">${esc(err.message)}</p>`; }
}
const shortTx = (t) => `${t.slice(0, 10)}…${t.slice(-6)}`;
const drawLookupFlows = () => $$("#lookup svg.txflow").forEach((svg) => { const d = buildDetail(svg.dataset.key); if (d) drawTxFlow(d.e, d.sides, svg); });
// A record's page, reached by its key or by a txid (which shows the
// transaction's main record, with its other records linked below it).
async function txPage(arg) {
  const graph = report.graph || [];
  let rec, txid = "";
  if (arg.startsWith("rec:")) {
    rec = graph.find((g) => g.key === arg.slice(4));
    if (!rec) throw new Error("That record isn't in your data any more.");
    txid = rec.txid || "";
  } else {
    txid = arg;
    const legs = graph.filter((g) => g.txid === txid);
    const main = legs.filter((g) => !/:fee$|:from:/.test(g.key));
    rec = (main.length ? main : legs).sort((x, y) => y.sats - x.sats)[0];
  }
  const [tx, source] = await Promise.all([
    txid ? api(`/api/tx/${txid}`) : null,
    rec?.key.startsWith("ex:") ? api(`/api/records/${encodeURIComponent(rec.key)}/source`).catch(() => null) : null,
  ]);
  if (rec) return renderRecord(buildDetail(rec.key), { tx, source });
  return `<article class="rec-view page"><header class="rec-head"><div class="rec-title"><span class="kind">Transaction</span>
      <h2 class="mono">${esc(shortId(txid))}</h2></div></header>
    <p class="muted">None of your records refer to this transaction.</p>${sec("On-chain transaction", chainSection(tx), "rec-chain")}</article>`;
}
async function addressPage(addr) {
  const a = await api(`/api/address/${encodeURIComponent(addr)}`);
  const recs = (report.graph || []).filter((g) => g.address === addr);
  return `<h2>Address <span class="mono">${esc(addr)}</span></h2>
    <div class="card">${a.wallet ? `<p>This is an address in your wallet <span class="mine">${esc(a.wallet)}</span>.</p>` : `<p class="muted">Not one of your wallets' addresses.</p>`}
      ${a.note ? `<p class="small warn">${esc(a.note)}</p>` : a.fulcrum ? `<p class="small muted">Checked your records and your Fulcrum server.</p>` : ""}</div>
    <h3>Transactions</h3><div class="scroll"><table><thead><tr><th>Transaction</th><th>Block</th><th></th></tr></thead><tbody>
      ${(a.txs || []).map((t) => `<tr><td><a class="mono small" href="#tx/${esc(t.txid)}">${esc(t.txid)}</a></td><td>${t.height > 0 ? t.height.toLocaleString("en-US") : "unconfirmed"}</td><td class="small muted">${t.local ? "in your wallets" : "from Fulcrum"}</td></tr>`).join("")
        || `<tr><td colspan="3" class="muted">None found.</td></tr>`}</tbody></table></div>
    ${recs.length ? `<h3>Your records with this address</h3>${recs.map((r) => `<div><a href="${pageLink(r)}">${esc(KIND_NAMES[r.kind] || r.kind)} ${amt(r.sats)}, ${esc(r.account)}, ${day(r.t)}</a></div>`).join("")}` : ""}
    ${(a.exchange || []).length ? `<h3>Exchange records with this address</h3>${a.exchange.map((x) => `<div>${esc(x.account)} ${esc(x.kind)} ${amt(x.sats)} on ${day(x.t)}</div>`).join("")}` : ""}`;
}
// A link to a record opens its own page: the transaction, or the record
// when it has no txid. "Show in Flow" (data-flowshow) opens it on Flow.
function recordLink(key) {
  const rec = String(key).split(">")[0];
  const g = (report?.graph || []).find((x) => x.key === rec);
  if (g) return pageLink(g);
  const t = rec.match(/^tx:([0-9a-f]{64})/);
  return t ? `#tx/${t[1]}` : `#tx/rec:${encodeURIComponent(rec)}`;
}
document.addEventListener("click", (e) => {
  const show = e.target.closest("[data-flowshow]")?.dataset.flowshow;
  if (show) { e.preventDefault(); location.hash = "graph"; setTimeout(() => selectEvent(show, true), 50); return; }
  const k = e.target.closest("[data-flowpick]")?.dataset.flowpick;
  if (!k) return;
  e.preventDefault();
  const rec = String(k).split(">")[0];
  if ($("#overview").classList.contains("on") && (report?.graph || []).some((g) => g.key === rec)) selectEvent(rec, false);
  else location.hash = recordLink(k);
});
$("#lookup").addEventListener("click", (e) => {
  const k = e.target.closest("[data-pick]")?.dataset.pick || e.target.closest("[data-flowkey]")?.dataset.flowkey;
  if (k) location.hash = pageLink((report.graph || []).find((g) => g.key === k) || { key: k });
});

function highlight(key) {
  const nodes = $$("#flow [data-key]");
  nodes.forEach((n) => n.classList.remove("hl", "dim", "sel"));
  if (!key) return;
  const has = (n) => (n.dataset.keys ? n.dataset.keys.split(" ") : [n.dataset.key]).some((k) => relatedKeys.has(k));
  nodes.forEach((n) => n.classList.toggle("dim", !has(n)));
  nodes.filter(has).forEach((n) => n.classList.add((n.dataset.keys || n.dataset.key).split(" ").includes(key) ? "sel" : "hl"));
}
$("#flow").addEventListener("click", (e) => {
  if (drag?.moved) return;
  const key = e.target.closest("[data-key]")?.dataset.key;
  if (key) selectEvent(key, true); else selectEvent(null);
});
$("#graph-detail").addEventListener("click", (e) => {
  if (e.target.id === "detail-close") { selectEvent(null); return; }
  const k = e.target.closest("[data-pick]")?.dataset.pick;
  if (k) {
    if (!(report.graph || []).some((g) => g.key === k)) { toast("That record isn't on the chart."); return; }
    selectEvent(k, true);
  }
});
document.addEventListener("keydown", (e) => { if (e.key === "Escape" && selected) selectEvent(null); });
window.addEventListener("resize", scheduleDraw);

// Settings
async function loadSettings() {
  const pi = await api("/api/prices");
  $("#price-source").innerHTML = pi.sources.map((x) => `<option value="${esc(x.id)}">${esc(x.name)}</option>`).join("");
  const span = (x) => (x.days ? `${x.days} days, ${x.first} to ${x.last}` : "none");
  $("#price-line").textContent = `Built in: ${span(pi.bundled)}. Imported or updated: ${span(pi.stored)}.`;
  priceHosts = Object.fromEntries(pi.sources.map((x) => [x.id, x.host]));
  const s = await api("/api/settings");
  const f = $("#settings-form");
  for (const [k, v] of Object.entries(s)) if (f.elements[k]) f.elements[k].value = v;
  $("#cert-line").textContent = s.fulcrumCert ? `Pinned server certificate SHA-256: ${s.fulcrumCert}` : "";
  const lf = $("#ln-form");
  lf.elements.clnUrl.value = s.clnUrl || ""; lf.elements.clnName.value = s.clnName || "";
  lf.elements.clnRune.placeholder = s.clnRuneSet === "true" ? "saved (paste a new one to replace it)" : "paste a read-only rune";
  $("#ln-cert-line").textContent = s.clnCert ? `Pinned node certificate SHA-256: ${s.clnCert}` : "";
  // Addresses the app platform (Umbrel, StartOS) gave for the node's own
  // services, until one is saved here.
  $$("#settings .node-note").forEach((n) => n.remove());
  for (const k of (s.fromNode || "").split(",").filter((k) => ["fulcrumHost", "clnUrl", "aiUrl"].includes(k))) {
    $(`#settings [name="${k}"]`)?.closest("label")?.insertAdjacentHTML("beforeend", `<span class="small muted node-note">Found on your node</span>`);
  }
  const hs = handlers = await api("/api/handlers");
  $("#handler-select").innerHTML = `<option value="">Detect automatically</option>` + hs.map((h) => `<option value="${esc(h.id)}">${esc(h.name)}</option>`).join("");
  $("#platforms-ok").textContent = hs.map((h) => h.name).join(", ");
}
$("#settings-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  try {
    await post("/api/settings", Object.fromEntries(new FormData(e.target)));
    toast("Saved");
    checkFulcrum(true);
    loadSettings();
    refresh();
  } catch (err) { toast(err.message); }
});
let priceHosts = {};
$("#fetch-prices-btn").addEventListener("click", async () => {
  const src = $("#price-source").value;
  if (!confirm(t(`This contacts ${priceHosts[src] || src} to download BTC/USD daily prices. Nothing about you or your wallets is sent. Continue?`))) return;
  try {
    await post("/api/settings", { priceSource: src });
    const r = await post("/api/prices/fetch", {});
    toast(`Updated ${r.days} days from ${r.source}`);
    loadSettings();
    refresh();
  } catch (e) { toast("Price update failed: " + e.message); }
});
// Backup: one file with all data, encrypted by default. Restore checks the
// file's version and integrity, asks before replacing anything, and the
// server keeps a copy of the current database first.
$("#backup-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const f = e.target, encrypt = f.encrypt.checked;
  if (encrypt && f.pass.value.length < 8) { toast("Choose a passphrase of at least 8 characters."); return; }
  if (encrypt && f.pass.value !== f.pass2.value) { toast("The passphrases don't match."); return; }
  if (!encrypt && !confirm(t("Without encryption, anyone with the file can see your wallets' public keys, your history and your Lightning rune. Download it unencrypted anyway?"))) return;
  const btn = $("button[type=submit]", f);
  btn.disabled = true; btn.textContent = "Preparing…";
  try {
    const r = await fetch("/api/backup", { method: "POST", headers: { "Content-Type": "application/json" },
      body: JSON.stringify(encrypt ? { passphrase: f.pass.value } : { unencrypted: true }) });
    if (!r.ok) throw new Error((await r.json().catch(() => ({}))).error || r.statusText);
    const blob = await r.blob();
    const name = (r.headers.get("content-disposition") || "").match(/filename="([^"]+)"/)?.[1] || "proof-of-basis-backup.json";
    const a = document.createElement("a");
    a.href = URL.createObjectURL(blob); a.download = name; document.body.append(a); a.click();
    setTimeout(() => { URL.revokeObjectURL(a.href); a.remove(); }, 1000);
    f.pass.value = f.pass2.value = "";
    toast(`Backup saved as ${name}`);
  } catch (err) { toast("Backup failed: " + err.message); }
  btn.disabled = false; btn.textContent = "Download backup";
});
$("#restore-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const f = e.target;
  const typed = prompt(t("Restoring replaces ALL data in this app with the backup's: wallets, imports, labels, settings and Lightning history. A copy of the current data is saved first.\n\nType REPLACE to continue."));
  if (typed !== "REPLACE") { if (typed !== null) toast("Nothing was changed."); return; }
  const fd = new FormData(f);
  fd.append("confirm", "REPLACE");
  try {
    const r = await api("/api/restore", { method: "POST", body: fd });
    f.reset();
    toast(`Restored: ${r.rows.accounts || 0} accounts, ${r.rows.events || 0} exchange rows, ${r.rows.labels || 0} labels.`);
    await loadSettings();
    await refresh();
    checkFulcrum(true);
  } catch (err) { toast("Restore failed: " + err.message); }
});

// Recheck everything: rescan every wallet in the background, then redraw.
async function pollRecheck() {
  let st;
  try { st = await api("/api/recheck"); } catch { return; }
  const box = $("#recheck-status");
  box.hidden = !st.running && !st.finished;
  $("#recheck-bar").max = Math.max(st.total, 1);
  $("#recheck-bar").value = st.done;
  $("#recheck-btn").disabled = st.running;
  $("#recheck-text").textContent = st.running
    ? `Rescanning ${st.current || "…"} (${st.done} of ${st.total} wallets done)`
    : st.finished ? `Done: ${st.total - st.errors.length} of ${st.total} wallet${st.total === 1 ? "" : "s"} rescanned; matching and taxes redone.${st.errors.length ? " Couldn't rescan: " + st.errors.join("; ") : ""}` : "";
  if (st.running) setTimeout(pollRecheck, 1000);
  else if (pollRecheck.was) { pollRecheck.was = false; refresh(); toast(st.errors.length ? "Recheck finished with problems" : "Recheck finished"); }
  if (st.running) pollRecheck.was = true;
}
$("#recheck-btn").addEventListener("click", async () => {
  try { await post("/api/recheck", {}); pollRecheck.was = true; pollRecheck(); } catch (e) { toast(e.message); }
});
pollRecheck();

$("#test-btn").addEventListener("click", async () => {
  try {
    await post("/api/settings", Object.fromEntries(new FormData($("#settings-form"))));
    const r = await post("/api/fulcrum/test", {});
    toast(`Connected. Chain tip is block ${r.height}.`);
    loadSettings();
  } catch (e) { toast("Could not connect: " + e.message); }
});

$("#wipe-btn").addEventListener("click", async () => {
  const typed = prompt(t("This deletes every wallet, import, label, price and setting, including your Fulcrum address. It cannot be undone.\n\nType WIPE to continue."));
  if (typed !== "WIPE") { if (typed !== null) toast("Nothing deleted."); return; }
  try {
    await post("/api/wipe", { confirm: "WIPE" });
    toast("Everything was deleted.");
    $("#preview").textContent = "";
    await loadSettings();
    await refresh();
    location.hash = "overview";
  } catch (e) { toast("Wipe failed: " + e.message); }
});

show(location.hash.slice(1) || "overview");
loadSettings();
refresh();
checkFulcrum();

// Display settings apply at once and stay in this browser.
(() => {
  const f = $("#display-form");
  if (!f) return;
  // Language: the browser's ("System") unless one is picked; a change
  // reloads the page in it (i18n.js translates as the page draws).
  const L = window.POB_I18N;
  f.lang.innerHTML = `<option value="">System (${esc(L.LANGS[L.system])})</option>` +
    Object.entries(L.LANGS).map(([c, name]) => `<option value="${c}" data-no-i18n>${esc(name)}</option>`).join("");
  f.lang.value = L.chosen;
  f.cents.value = prefs.cents ? "yes" : "no";
  f.unit.value = prefs.unit;
  f.colors.value = prefs.colors;
  const sample = () => { $("#display-sample").textContent = `Looks like: ${usd(123456789)} · ${amt(12345678)}`; };
  sample(); setUnitLabels();
  f.addEventListener("submit", (e) => e.preventDefault());
  f.addEventListener("change", () => {
    if (f.lang.value !== L.chosen) {
      try { if (f.lang.value) localStorage.setItem("lang", f.lang.value); else localStorage.removeItem("lang"); } catch {}
      location.reload();
      return;
    }
    prefs.cents = f.cents.value === "yes";
    prefs.unit = f.unit.value;
    prefs.colors = f.colors.value;
    try { localStorage.setItem("displayPrefs", JSON.stringify(prefs)); } catch {}
    sample(); setUnitLabels();
    // Colors come from CSS; charts drawn with them redraw as on a theme change.
    if (prefs.colors === "cb") document.documentElement.dataset.colors = "cb"; else delete document.documentElement.dataset.colors;
    applyTheme(document.documentElement.dataset.theme, false);
    if (report) refresh();
  });
})();

// Security: change the password, sign out here or everywhere.
$("#security-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const f = e.target;
  if (f.next.value !== f.next2.value) { toast("The new passwords don't match."); return; }
  try {
    await post("/api/auth/password", { current: f.current.value, new: f.next.value });
    f.reset();
    toast("Password changed. Other devices have been signed out.");
  } catch (err) { toast(err.message); }
});
$("#logout-btn").addEventListener("click", async () => {
  try { await post("/api/auth/logout", {}); } catch {}
  location.href = "/login";
});
$("#logout-all-btn").addEventListener("click", async () => {
  if (!confirm(t("Sign out on every device, including this one?"))) return;
  try { await post("/api/auth/logout-all", {}); } catch {}
  location.href = "/login";
});



// The details dialog closes with Escape (the browser's own), its ✕, or a
// click outside it; the graph's highlight goes with it.
$("#graph-detail").addEventListener("close", () => { if (selected && !keepPick) selectEvent(null); keepPick = false; });
$("#graph-detail").addEventListener("click", (e) => { if (e.target === e.currentTarget) selectEvent(null); });
