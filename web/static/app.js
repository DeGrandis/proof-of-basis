"use strict";
// Plain JS, no dependencies. Everything talks only to this server.

const $ = (s, el = document) => el.querySelector(s);
const $$ = (s, el = document) => [...el.querySelectorAll(s)];
const esc = (s) => String(s ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c]);
const btc = (sats) => (sats < 0 ? "-" : "") + (Math.abs(sats) / 1e8).toFixed(8);
const usd = (cents) => (cents < 0 ? "-$" : "$") + (Math.abs(cents) / 100).toLocaleString("en-US", { minimumFractionDigits: 2, maximumFractionDigits: 2 });
const signed = (cents) => `<span class="${cents < 0 ? "neg" : cents > 0 ? "pos" : ""}">${usd(cents)}</span>`;
const day = (t) => new Date(typeof t === "number" ? t * 1000 : t).toISOString().slice(0, 10);

let report = null;

async function api(path, opts = {}) {
  const r = await fetch(path, opts);
  const body = r.headers.get("content-type")?.includes("json") ? await r.json() : await r.text();
  if (!r.ok) throw new Error(body?.error || r.statusText);
  return body;
}
const post = (path, data) => api(path, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(data) });

function toast(msg) {
  const t = $("#toast");
  t.textContent = msg;
  t.hidden = false;
  clearTimeout(toast.timer);
  toast.timer = setTimeout(() => (t.hidden = true), 4000);
}

function show(tab) {
  $$(".tab").forEach((s) => s.classList.toggle("on", s.id === tab));
  $$("#tabs button").forEach((b) => b.classList.toggle("on", b.dataset.tab === tab));
  $("#tab-title").textContent = $(`#tabs button[data-tab="${tab}"] .lbl`)?.textContent || "";
  document.body.classList.toggle("flow", tab === "graph");
  if (tab === "graph") drawGraph();
}
// Sidebar: open by default (collapsed to icons on narrow screens); the
// hamburger shrinks it to an icon rail and back. The choice is remembered.
function setMenu(open, remember) {
  document.body.classList.toggle("collapsed", !open);
  $("#menu-btn").setAttribute("aria-expanded", String(open));
  $("#menu-btn").setAttribute("aria-label", open ? "Collapse menu" : "Expand menu");
  if (remember) try { localStorage.setItem("menu", open ? "open" : "closed"); } catch {}
  if ($("#graph").classList.contains("on")) setTimeout(drawGraph, 200);
}
setMenu((() => { try { const v = localStorage.getItem("menu"); if (v) return v === "open"; } catch {} return innerWidth >= 760; })());
$("#menu-btn").addEventListener("click", () => setMenu(document.body.classList.contains("collapsed"), true));
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
  $("#fulcrum-line").textContent = line;
  $("#fulcrum-status").title = "Fulcrum: " + line + ". Click to check again.";
}
$("#fulcrum-status").addEventListener("click", () => { $("#fulcrum-line").textContent = "checking…"; checkFulcrum(true); });
setInterval(() => { if (!document.hidden) checkFulcrum(); }, 60000);
window.addEventListener("hashchange", () => show(location.hash.slice(1) || "overview"));

async function refresh() {
  try {
    report = await api("/api/report");
  } catch (e) {
    toast(e.message);
    return;
  }
  // One broken section must not stop the others from drawing.
  for (const f of [renderOverview, renderWallets, renderReview, renderTaxes, renderImports]) {
    try { await f(); } catch (e) { console.error(e); toast(`Could not draw the page: ${e.message}`); }
  }
  if ($("#graph").classList.contains("on")) drawGraph();
}

let pickedYear = (() => { try { return Number(localStorage.getItem("year")) || 0; } catch { return 0; } })();
$("#year-select").addEventListener("change", (e) => {
  pickedYear = Number(e.target.value);
  try { localStorage.setItem("year", pickedYear); } catch {}
  renderOverview();
});

function renderOverview() {
  const r = report;
  const thisYear = new Date().getFullYear();
  const active = (r.graph || []).map((e) => new Date(e.t * 1000).getUTCFullYear()); // any year with activity
  const years = [...new Set([thisYear, ...(r.years || []).map((y) => y.year), ...active])].sort((a, b) => b - a);
  const yr = years.includes(pickedYear) ? pickedYear : thisYear;
  $("#year-select").innerHTML = years.map((y) => `<option value="${y}"${y === yr ? " selected" : ""}>${y}${y === thisYear ? " (this year)" : ""}</option>`).join("");
  const ys = (r.years || []).find((y) => y.year === yr);
  const tiles = [
    ["Holdings", btc(r.holdingsSats) + " BTC"],
    ["Unrealized gain", r.price ? usd(r.unrealized) : "needs a price"],
    [`${yr} realized`, ys ? usd(ys.shortProceeds - ys.shortBasis + ys.longProceeds - ys.longBasis) : "$0.00"],
    [`${yr} est. tax`, ys ? usd(ys.estimatedTax) : "$0.00"],
    ["Needs a label", String(r.unmatched?.length || 0)],
  ];
  $("#tiles").innerHTML = tiles.map(([k, v]) => `<div class="tile"><div class="k">${esc(k)}</div><div class="v">${esc(v)}</div></div>`).join("");
  $("#accounts-table tbody").innerHTML = (r.accounts || []).map((a) => `<tr>
    <td>${esc(a.name)}</td><td>${esc(a.kind)}</td><td class="num">${btc(a.sats)}</td>
    <td class="num">${a.chainSats == null ? "" : btc(a.chainSats)}${a.chainSats != null && a.chainSats !== a.sats ? ' <span class="warn" title="Traced and on-chain balances differ: something is unlabeled or a scan is stale">!</span>' : ""}</td>
    <td class="num">${usd(a.basis)}</td></tr>`).join("") || `<tr><td colspan="5" class="muted">No accounts yet.</td></tr>`;
  $("#warnings").innerHTML = (r.warnings || []).length
    ? `<h2>Warnings</h2><div class="card small">${r.warnings.map((w) => `<div class="warn">${esc(w)}</div>`).join("")}</div>` : "";
  const n = r.unmatched?.length || 0;
  $("#review-count").hidden = !n;
  $("#review-count").textContent = n;
  $("#setup-hint").hidden = !!r.settings.fulcrumHost;
}

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
      toast(`Found ${r.txs} transactions, balance ${btc(r.balance)} BTC`);
    } catch (err) { toast(err.message); }
    refresh();
  }
  if (del && confirm("Remove this wallet and its scanned transactions?")) {
    await api(`/api/accounts/${del}`, { method: "DELETE" });
    refresh();
  }
});
const walletData = () => {
  const f = new FormData($("#wallet-form"));
  return { name: f.get("name"), key: f.get("key"), script: f.get("script"), gap: Number(f.get("gap")) };
};
$("#preview-btn").addEventListener("click", async () => {
  try {
    const r = await post("/api/wallets/preview", walletData());
    $("#preview").innerHTML = `Detected ${esc(r.prefix)} (${esc(r.script)}). First receive addresses, check they match your wallet:<br>` + r.addresses.map(esc).join("<br>");
  } catch (e) { $("#preview").textContent = e.message; }
});
$("#wallet-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  try {
    const { id } = await post("/api/wallets", walletData());
    e.target.reset();
    $("#preview").textContent = "";
    toast("Wallet added. Scanning…");
    await refresh(); // show it now; the scan can take a while
    await post(`/api/wallets/${id}/scan`, {}).then((r) => toast(`Found ${r.txs} transactions`)).catch((err) => toast("Added, but the scan failed: " + err.message));
    refresh();
  } catch (err) { toast(err.message); }
});

// Imports
async function renderImports() {
  const list = await api("/api/imports").catch(() => []);
  $("#imports-table tbody").innerHTML = (list || []).map((i) => `<tr>
    <td>${esc(i.filename)}</td><td>${esc(i.account)}</td><td>${esc(i.handler)}</td><td class="num">${i.rows}</td>
    <td class="small">${(i.warnings || []).length ? `<details><summary class="warn">${i.warnings.length}</summary>${i.warnings.map((w) => `line ${w.Row}: ${esc(w.Message)}`).join("<br>")}</details>` : ""}</td>
    <td><button class="link" data-del-import="${i.id}">Remove</button></td></tr>`).join("") || `<tr><td colspan="6" class="muted">Nothing imported yet.</td></tr>`;
}
$("#imports-table").addEventListener("click", async (e) => {
  const id = e.target.dataset.delImport;
  if (id && confirm("Remove this import and all its rows?")) {
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
const needsAmount = { spend: "Value (USD)", sold: "Proceeds (USD)", income: "Value (USD)", "gift-in": "Donor's basis (USD)", purchase: "Cost (USD)", refund: "Basis (USD)", untracked: "Basis (USD)" };

function renderReview() {
  const items = report.unmatched || [];
  $("#review-table tbody").innerHTML = items.map((it) => {
    const cats = it.Dir === "out" ? OUT_CATS : IN_CATS;
    const detail = [it.Source === "chain" ? "on-chain" : esc(it.Kind), it.Address && `<span class="mono">${esc(it.Address)}</span>`,
      it.Txid && `<span class="mono" title="${esc(it.Txid)}">${esc(it.Txid.slice(0, 12))}…</span>`, it.Note && esc(it.Note)].filter(Boolean).join(" · ");
    return `<tr data-key="${esc(it.Key)}" data-dir="${esc(it.Dir)}">
      <td>${day(it.Time)}</td><td>${esc(it.Account)}</td><td>${it.Dir === "out" ? "Out" : "In"}</td>
      <td class="num">${btc(it.Sats)}${it.FeeSats ? `<div class="small muted">+${btc(it.FeeSats)} fee</div>` : ""}</td>
      <td class="row"><select class="cat"><option value="">Choose…</option>${cats.map(([v, l]) => `<option value="${v}">${esc(l)}</option>`).join("")}</select>
        <span class="extra"></span><button class="save" hidden>Save</button></td>
      <td class="small">${detail}</td></tr>`;
  }).join("") || `<tr><td colspan="6" class="muted">Nothing to label.</td></tr>`;
  $("#links-table tbody").innerHTML = (report.links || []).map((l) => `<tr><td class="small mono">${esc(l.From)}</td><td class="small mono">${esc(l.To)}</td><td>${esc(l.Method)}${l.Method === "manual" ? ` <button class="link" data-unlink="${esc(l.From)}" data-unlink2="${esc(l.To)}">undo</button>` : ""}</td></tr>`).join("")
    || `<tr><td colspan="3" class="muted">No transfers matched yet.</td></tr>`;
}
$("#review-table").addEventListener("change", (e) => {
  if (!e.target.classList.contains("cat")) return;
  const tr = e.target.closest("tr"), cat = e.target.value, extra = $(".extra", tr);
  extra.innerHTML = "";
  if (cat === "self") {
    const opp = (report.unmatched || []).filter((i) => i.Dir !== tr.dataset.dir);
    extra.innerHTML = `<select class="cp">${opp.map((i) => `<option value="${esc(i.Key)}">${day(i.Time)} ${esc(i.Account)} ${btc(i.Sats)}</option>`).join("")}</select>`;
  } else if (cat === "self-transfer") {
    extra.innerHTML = `<span class="small muted">${tr.dataset.dir === "out" ? "Keeps its basis and purchase dates while it's away." : "Brings back coins you sent out as a self transfer, with their original basis and dates."}</span>`;
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
$("#links-table").addEventListener("click", async (e) => {
  const a = e.target.dataset.unlink, b = e.target.dataset.unlink2;
  if (!a) return;
  await api(`/api/labels/${encodeURIComponent(a)}`, { method: "DELETE" }).catch(() => {});
  await api(`/api/labels/${encodeURIComponent(b)}`, { method: "DELETE" }).catch(() => {});
  refresh();
});

// Taxes
function renderTaxes() {
  const sales = {};
  for (const d of report.disposals || []) { const yr = new Date(d.Disposed).getUTCFullYear(); sales[yr] = (sales[yr] || 0) + 1; }
  $("#years-table tbody").innerHTML = (report.years || []).map((y) => `<tr><td>${y.year}</td>
    <td class="num">${signed(y.shortProceeds - y.shortBasis)}</td><td class="num">${signed(y.longProceeds - y.longBasis)}</td>
    <td class="num">${usd(y.income)}</td><td class="num">${usd(y.estimatedTax)}${y.missingBasisSats ? ` <span class="warn" title="${btc(y.missingBasisSats)} BTC sold with no known basis">!</span>` : ""}</td>
    <td>${sales[y.year] ? `<a href="/api/export/8949.csv?year=${y.year}">Form 8949 CSV</a> <span class="small muted">${sales[y.year]} sales</span>` : `<span class="small muted" title="Form 8949 lists sales and spends. This year only has income, which is reported elsewhere.">No sales to report</span>`}</td></tr>`).join("") || `<tr><td colspan="6" class="muted">No disposals yet.</td></tr>`;
  $("#disposals-table tbody").innerHTML = (report.disposals || []).map((d) => `<tr>
    <td>${day(d.Disposed)}</td><td>${d.Missing ? '<span class="warn">unknown</span>' : day(d.Acquired)}</td><td>${esc(d.Account)}</td>
    <td class="num">${btc(d.Sats)}</td><td class="num">${usd(d.Proceeds)}</td><td class="num">${usd(d.Basis)}</td>
    <td class="num">${signed(d.Proceeds - d.Basis)}</td><td>${d.LongTerm ? "Long" : "Short"}</td></tr>`).join("") || `<tr><td colspan="8" class="muted">None.</td></tr>`;
}

// Flow graph: exchanges and wallets in separate bands, one lane per
// account, time left to right, dot size by amount. It fills the page;
// the wheel zooms the timeline around the pointer, dragging pans, and
// clicking a dot opens it and zooms to it and everything it traces to.
const KINDS = [["buy", "Buy"], ["sell", "Sale / spend"], ["income", "Income / acquired"], ["unmatched", "Needs a label"], ["gift", "Gift / other"]];
const KIND_NAMES = { buy: "Buy", sell: "Sale", income: "Income", transfer: "Transfer", consolidation: "Consolidation", unmatched: "Needs a label", spend: "Spent",
  sold: "Sold to a person", gift: "Gift sent", donation: "Donation", lost: "Lost or stolen", untracked: "Untracked wallet", "self-transfer": "Self transfer",
  "gift-in": "Gift received", purchase: "Bought from a person", refund: "Refund" };
$("#legend").innerHTML = KINDS.map(([k, l]) => `<span><svg width="10" height="10"><circle cx="5" cy="5" r="5" class="dot ${k}"></circle></svg> ${l}</span>`).join("")
  + `<span><svg width="18" height="10"><line x1="1" y1="5" x2="17" y2="5" class="link" stroke-width="2"/></svg> Transfer</span>`
  + `<span><svg width="12" height="12"><rect x="2" y="2" width="8" height="8" class="hub" transform="rotate(45 6 6)"/></svg> One tx, several accounts</span>`
  + `<span><svg width="12" height="12"><rect x="1.5" y="1.5" width="9" height="9" class="consol"/></svg> Consolidation</span>`;
$("#show-transfers").addEventListener("change", () => drawGraph());
let selected = null;
const view = { d0: 0, d1: 1, full: 1, key: "" }; // visible slice of the compressed timeline
let axis = null; // {times, pos}: event times and their positions on the timeline

function lanesByBand() {
  const kindOf = Object.fromEntries((report.accounts || []).map((a) => [a.name, a.kind]));
  const names = [...new Set([...(report.accounts || []).filter((a) => a.kind !== "pool").map((a) => a.name),
    ...(report.graph || []).flatMap((e) => [e.account, e.to].filter(Boolean))])];
  const bands = [["Exchanges", "exchange"], ["Wallets", "wallet"], ["Other", null]].map(([label, k]) => ({
    label, lanes: names.filter((n) => (k ? kindOf[n] === k : kindOf[n] !== "exchange" && kindOf[n] !== "wallet")).sort(),
  }));
  return bands.filter((b) => b.lanes.length);
}

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

const LEFT = 150;
function geometry() {
  const wrap = $("#graph-wrap");
  return { W: Math.max(400, wrap.clientWidth), H: Math.max(260, wrap.clientHeight) };
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

function drawGraph() {
  const svg = $("#flow");
  if (!report || !$("#graph").classList.contains("on")) return;
  const ev = report.graph || [];
  if (!ev.length) { svg.setAttribute("width", 600); svg.setAttribute("height", 60); svg.innerHTML = `<text x="16" y="34">Nothing to draw yet.</text>`; return; }
  const sig = ev.length + ":" + ev[0].t + ":" + ev.at(-1).t;
  if (view.key !== sig) { // new data: rebuild the timeline and show everything
    axis = buildAxis(ev);
    view.full = axis.pos.at(-1) || 1;
    view.d0 = -view.full * 0.01; view.d1 = view.full * 1.01; view.key = sig;
  }
  const { W, H } = geometry();
  const bands = lanesByBand();
  const nLanes = bands.reduce((n, b) => n + b.lanes.length, 0);
  const laneH = Math.max(26, Math.min(70, (H - 40 - bands.length * 34) / nLanes));
  const laneY = {};
  let yy = 6, out = "", labels = "";
  for (const b of bands) {
    const top = yy;
    yy += 22;
    for (const l of b.lanes) { laneY[l] = yy + laneH / 2; yy += laneH; }
    out += `<rect class="band" x="0" y="${top}" width="${W}" height="${yy - top}" rx="8"/>`;
    labels += `<text class="band-label" x="10" y="${top + 15}">${esc(b.label)}</text>`;
    yy += 12;
  }
  const bottom = yy;
  const visible = (p) => p >= view.d0 - (view.d1 - view.d0) * 0.02 && p <= view.d1 + (view.d1 - view.d0) * 0.02;
  // ticks
  const tt0 = timeOf(Math.max(view.d0, 0)), tt1 = timeOf(Math.min(view.d1, view.full));
  let lastLabel = -1e9;
  for (const [t, label, major] of ticks(tt0, tt1)) {
    const xx = xOf(posOf(t), W);
    if (xx < LEFT + 10 || xx > W - 10) continue;
    out += `<line class="lane${major ? "" : " minor"}" x1="${xx}" x2="${xx}" y1="6" y2="${bottom}" stroke-dasharray="2 4"/>`;
    if (xx - lastLabel > (major ? 34 : 44)) { out += `<text x="${xx + 3}" y="${bottom + 14}"${major ? ' class="major"' : ""}>${label}</text>`; lastLabel = xx; }
  }
  for (const l of Object.keys(laneY)) out += `<line class="lane" x1="${LEFT}" x2="${W - 10}" y1="${laneY[l]}" y2="${laneY[l]}"/>`;
  const maxSats = Math.max(...ev.map((e) => e.sats), 1);
  const r = (s) => 3 + 9 * Math.sqrt(s / maxSats);

  // Transfers, grouped by transaction. One account to one account is a
  // straight connector; one to many or many to one meets at a diamond hub.
  const hubbed = new Set();
  if ($("#show-transfers").checked) {
    const groups = new Map();
    for (const e of ev) {
      if (e.kind !== "transfer" || !e.to || laneY[e.to] == null || laneY[e.account] == null) continue;
      const g = e.txid || e.key;
      if (!groups.has(g)) groups.set(g, []);
      groups.get(g).push(e);
    }
    for (const legs of groups.values()) {
      const p = posOf(legs[0].t);
      if (!visible(p)) continue;
      const xx = xOf(p, W), keys = legs.map((l) => l.key).join(" ");
      const srcs = [...new Set(legs.map((l) => l.account))], dsts = [...new Set(legs.map((l) => l.to))];
      const tip = legs.map((l) => `${btc(l.sats)} BTC ${l.account} → ${l.to}`).join("\n") + `\n${day(legs[0].t)}`;
      const width = (s) => 1 + 4 * Math.sqrt(s / maxSats);
      if (legs.length === 1) {
        const e = legs[0], y1 = laneY[e.account], y2 = laneY[e.to], rr = Math.max(3, r(e.sats) * 0.6);
        out += `<g data-key="${esc(e.key)}" data-keys="${esc(keys)}" class="tr"><title>${esc(tip)}</title><line class="link" x1="${xx}" y1="${y1}" x2="${xx}" y2="${y2}" stroke-width="${width(e.sats)}"/>`
          + `<circle class="link-end" cx="${xx}" cy="${y1}" r="${rr}"/><circle class="link-end" cx="${xx}" cy="${y2}" r="${rr}"/></g>`;
        continue;
      }
      if (legs[0].txid) hubbed.add(legs[0].txid);
      const fanOut = srcs.length === 1;
      const hy = fanOut ? laneY[srcs[0]] : dsts.length === 1 ? laneY[dsts[0]] : (Math.min(...legs.map((l) => laneY[l.account])) + Math.max(...legs.map((l) => laneY[l.to]))) / 2;
      let paths = "";
      legs.forEach((l, i) => {
        const off = (i - (legs.length - 1) / 2) * 12;
        const [ax, ay, bx, by] = fanOut ? [xx, hy, xx + off, laneY[l.to]] : [xx + off, laneY[l.account], xx, hy];
        const my = (ay + by) / 2;
        paths += `<path class="link" d="M${ax},${ay} C${ax},${my} ${bx},${my} ${bx},${by}" stroke-width="${width(l.sats)}"/>`
          + `<circle class="link-end" cx="${fanOut ? bx : ax}" cy="${fanOut ? by : ay}" r="${Math.max(3, r(l.sats) * 0.55)}"/>`;
      });
      out += `<g data-key="${esc(legs[0].key)}" data-keys="${esc(keys)}" class="tr"><title>${esc(tip)}</title>${paths}<rect class="hub" x="${xx - 5}" y="${hy - 5}" width="10" height="10" transform="rotate(45 ${xx} ${hy})"/></g>`;
    }
  }
  for (const e of ev) {
    if (e.kind === "transfer" || laneY[e.account] == null) continue;
    const p = posOf(e.t);
    if (!visible(p)) continue;
    const xx = xOf(p, W) + (e.txid && hubbed.has(e.txid) ? 12 : 0), yy2 = laneY[e.account];
    const tip = `${KIND_NAMES[e.kind] || e.kind} ${btc(e.sats)} BTC, ${e.account}, ${day(e.t)}` + (e.inputs ? `\n${e.inputs} inputs → ${e.outputs} outputs` : "");
    if (e.kind === "consolidation") {
      const s = r(e.sats) * 1.6;
      out += `<rect class="consol" data-key="${esc(e.key)}" x="${xx - s / 2}" y="${yy2 - s / 2}" width="${s}" height="${s}"><title>${esc(tip)}</title></rect>`;
    } else {
      out += `<circle class="dot ${esc(e.kind)}" data-key="${esc(e.key)}" cx="${xx}" cy="${yy2}" r="${r(e.sats)}"><title>${esc(tip)}</title></circle>`;
    }
  }
  // Lane names stay put while the timeline pans underneath them.
  labels += Object.keys(laneY).map((l) => `<text class="lane-name" x="16" y="${laneY[l] + 4}"><title>${esc(l)}</title>${esc(l.length > 19 ? l.slice(0, 18) + "…" : l)}</text>`).join("");
  out += `<rect class="gutter" x="0" y="0" width="${LEFT}" height="${bottom + 20}"/>` + labels;
  svg.setAttribute("width", W);
  svg.setAttribute("height", H);
  svg.setAttribute("viewBox", `0 0 ${W} ${H}`);
  svg.innerHTML = out;
  const span = timeOf(Math.min(view.d1, view.full)) - timeOf(Math.max(view.d0, 0));
  $("#zoom-level").textContent = span > 2 * 365 * 86400 ? `${(span / 365 / 86400).toFixed(1)} years shown` : span > 60 * 86400 ? `${Math.round(span / 30 / 86400)} months shown` : `${Math.max(1, Math.round(span / 86400))} days shown`;
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
  drawGraph();
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
    view.d0 += d; view.d1 += d; clampView(); drawGraph();
    return;
  }
  zoomAt(e.clientX - rect.left, Math.exp(e.deltaY * 0.0015));
}, { passive: false });
let drag = null;
svgEl.addEventListener("pointerdown", (e) => { drag = { x: e.clientX, d0: view.d0, d1: view.d1, moved: false }; });
window.addEventListener("pointermove", (e) => {
  if (!drag) return;
  const dx = e.clientX - drag.x;
  if (Math.abs(dx) > 3) { drag.moved = true; svgEl.classList.add("panning"); }
  if (!drag.moved) return;
  const { W } = geometry(), d = (-dx / (W - LEFT - 40)) * (drag.d1 - drag.d0);
  view.d0 = drag.d0 + d; view.d1 = drag.d1 + d; clampView(); drawGraph();
});
window.addEventListener("pointerup", () => { setTimeout(() => (drag = null), 0); svgEl.classList.remove("panning"); });
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
const when = (t) => new Date(t * 1000).toISOString().replace("T", " ").slice(0, 16) + " UTC";
const pickKey = (k) => `<button class="link" data-pick="${esc(k)}">`;
let relatedKeys = new Set();

// The transaction view: what happened, plus where the coins came from
// (sales, spends, gifts) or where they went (buys, income, gifts received).
function selectEvent(key, zoom) {
  const e = (report.graph || []).find((g) => g.key === key);
  const box = $("#graph-detail");
  if (!e) { const was = !box.hidden; box.hidden = true; selected = null; relatedKeys = new Set(); if (was) drawGraph(); else highlight(null); return; }
  selected = key;
  const disposals = report.disposals || [], removals = report.removals || [], open = report.openLots || [];
  const related = new Set([key]);
  const byKey = Object.fromEntries((report.graph || []).map((g) => [g.key, g]));
  const via = (hops) => { hops.forEach((h) => related.add(h.key)); return hops; };
  const rows = [["When", when(e.t)], ["Account", esc(e.account) + (e.to ? ` → ${esc(e.to)}` : "")], ["Amount", `${btc(e.sats)} BTC`]];
  if (e.cents) rows.push([e.kind === "sell" || e.kind === "sold" ? "Proceeds" : e.kind === "buy" ? "Cost" : "Value", usd(e.cents) + (e.sats ? ` <span class="muted small">(${usd(Math.round(e.cents * 1e8 / e.sats))}/BTC)</span>` : "")]);
  if (e.feeCents) rows.push(["Fee (USD)", usd(e.feeCents)]);
  if (e.feeSats) rows.push(["Network fee", `${btc(e.feeSats)} BTC`]);
  if (e.inputs) rows.push(["Shape", `${e.inputs} input${e.inputs === 1 ? "" : "s"} → ${e.outputs} output${e.outputs === 1 ? "" : "s"}`]);
  if (e.method) rows.push(["Matched by", esc(e.method)]);
  if (e.txid) rows.push(["Transaction", `<span class="mono small">${esc(e.txid)}</span>`]);
  if (e.address) rows.push(["Address", `<span class="mono small">${esc(e.address)}</span>`]);
  if (e.note) rows.push(["Note", esc(e.note)]);

  let trace = "";
  const sameTx = e.txid ? (report.graph || []).filter((g) => g.txid === e.txid && g.key !== key) : [];
  if (sameTx.length) {
    trace += `<b>Same transaction</b>` + sameTx.map((g) => { related.add(g.key);
      return `<div>${pickKey(g.key)}${esc(KIND_NAMES[g.kind] || g.kind)}</button> ${btc(g.sats)} BTC ${esc(g.account)}${g.to ? ` → ${esc(g.to)}` : ""}</div>`; }).join("");
  }
  const fromLots = disposals.filter((d) => d.OpID === key);
  const fromGifts = removals.filter((d) => d.OpID === key);
  if (fromLots.length || fromGifts.length) {
    trace += `<b>Came from</b>` + [...fromLots, ...fromGifts].map((d) => {
      related.add(d.OriginID);
      if (d.Missing) return `<div class="warn">${btc(d.Sats)} BTC with no known purchase</div>`;
      const gain = d.Proceeds != null ? `, gain ${signed(d.Proceeds - d.Basis)} (${d.LongTerm ? "long" : "short"} term)` : "";
      const o = byKey[d.OriginID], hops = o ? via(route(o.account, o.t, e.account, e.t)) : [];
      return `<div>${btc(d.Sats)} BTC ${pickKey(d.OriginID)}acquired ${day(d.Acquired)}</button>${o ? ` on ${esc(o.account)}` : ""}, basis ${usd(d.Basis)}${gain}${routeText(o?.account, hops)}</div>`;
    }).join("");
  }
  const sold = disposals.filter((d) => d.OriginID === key), gone = removals.filter((d) => d.OriginID === key), held = open.filter((l) => l.OriginID === key);
  if (sold.length || gone.length || held.length) {
    trace += `<b>Where these coins went</b>`
      + sold.map((d) => { related.add(d.OpID); const hops = via(route(e.account, e.t, d.Account, tsec(d.Disposed)));
        return `<div>${btc(d.Sats)} BTC ${pickKey(d.OpID)}sold ${day(d.Disposed)}</button> from ${esc(d.Account)}, gain ${signed(d.Proceeds - d.Basis)}${routeText(e.account, hops)}</div>`; }).join("")
      + gone.map((d) => { related.add(d.OpID); const hops = via(route(e.account, e.t, d.Account, tsec(d.Removed)));
        return `<div>${btc(d.Sats)} BTC ${pickKey(d.OpID)}left ${day(d.Removed)}</button> from ${esc(d.Account)} (gift, donation or loss)${routeText(e.account, hops)}</div>`; }).join("")
      + held.map((l) => { const hops = l.Account ? via(route(e.account, e.t, l.Account, Infinity)) : [];
        return `<div>${btc(l.Sats)} BTC still held in ${l.Account ? esc(l.Account) : "the pre-2025 pool"}, basis ${usd(l.Basis)}${routeText(e.account, hops)}</div>`; }).join("");
  }
  if (e.kind === "transfer") trace += `<b>Moved between your accounts</b><div>${esc(e.account)} → ${esc(e.to)}. Not taxable; cost basis and purchase dates move with the coins${e.feeSats ? ". The network fee is added to basis or treated as a small sale, per Settings" : ""}.</div>`;
  if (e.kind === "consolidation") trace += `<b>Coins merged inside ${esc(e.account)}</b><div>Not taxable. Only the network fee left the wallet, handled per Settings.</div>`;
  if (e.kind === "unmatched") trace += `<b>Not traced yet</b><div>This left or entered a tracked account and couldn't be matched. <a href="#review">Label it in Review</a>.</div>`;
  if (!trace) trace = `<span class="muted">Nothing further to trace.</span>`;

  box.innerHTML = `<h3><span class="kind ${esc(e.kind)}">${esc(KIND_NAMES[e.kind] || e.kind)}</span> ${btc(e.sats)} BTC <button class="link small" id="detail-close" aria-label="Close">✕</button></h3>
    <dl>${rows.map(([k, v]) => `<dt>${k}</dt><dd>${v}</dd>`).join("")}</dl><div class="trace">${trace}</div>`;
  box.hidden = false;
  relatedKeys = related;
  drawGraph(); // the panel may have narrowed the chart
  if (zoom) {
    const ps = [...related].map((k) => byKey[k]).filter(Boolean).map((g) => posOf(g.t));
    let lo = Math.min(...ps), hi = Math.max(...ps);
    const minSpan = Math.max(8, view.full * 0.01);
    if (hi - lo < minSpan) { const m = (lo + hi) / 2; lo = m - minSpan / 2; hi = m + minSpan / 2; }
    const pad = (hi - lo) * 0.12;
    animateTo(lo - pad, hi + pad);
  }
}
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
window.addEventListener("resize", () => drawGraph());

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
  const hs = handlers = await api("/api/handlers");
  $("#handler-select").innerHTML = `<option value="">Detect automatically</option>` + hs.map((h) => `<option value="${esc(h.id)}">${esc(h.name)}</option>`).join("");
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
  if (!confirm(`This contacts ${priceHosts[src] || src} to download BTC/USD daily prices. Nothing about you or your wallets is sent. Continue?`)) return;
  try {
    await post("/api/settings", { priceSource: src });
    const r = await post("/api/prices/fetch", {});
    toast(`Updated ${r.days} days from ${r.source}`);
    loadSettings();
    refresh();
  } catch (e) { toast("Price update failed: " + e.message); }
});
$("#test-btn").addEventListener("click", async () => {
  try {
    await post("/api/settings", Object.fromEntries(new FormData($("#settings-form"))));
    const r = await post("/api/fulcrum/test", {});
    toast(`Connected. Chain tip is block ${r.height}.`);
    loadSettings();
  } catch (e) { toast("Could not connect: " + e.message); }
});

$("#wipe-btn").addEventListener("click", async () => {
  const typed = prompt("This deletes every wallet, import, label, price and setting, including your Fulcrum address. It cannot be undone.\n\nType WIPE to continue.");
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
