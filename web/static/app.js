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
  if (tab === "graph") drawGraph();
}
$("#tabs").addEventListener("click", (e) => {
  const b = e.target.closest("button");
  if (b) { location.hash = b.dataset.tab; }
});
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

function renderOverview() {
  const r = report;
  const latest = (r.years || []).at(-1);
  const tiles = [
    ["Holdings", btc(r.holdingsSats) + " BTC"],
    ["Unrealized gain", r.price ? usd(r.unrealized) : "needs a price"],
    [latest ? `${latest.year} realized` : "Realized", latest ? usd(latest.shortProceeds - latest.shortBasis + latest.longProceeds - latest.longBasis) : "$0.00"],
    [latest ? `${latest.year} est. tax` : "Est. tax", latest ? usd(latest.estimatedTax) : "$0.00"],
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
$("#price-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  try {
    const r = await api("/api/prices", { method: "POST", body: new FormData(e.target) });
    toast(`Imported ${r.days} daily prices`);
    refresh();
  } catch (err) { toast(err.message); }
});

// Review: the labeling queue
const OUT_CATS = [["spend", "Paid for something"], ["sold", "Sold to a person"], ["gift", "Gift sent"], ["donation", "Donation"], ["lost", "Lost or stolen"], ["untracked", "My wallet (not tracked)"], ["self", "Matches another row…"]];
const IN_CATS = [["income", "Income / payment received"], ["gift-in", "Gift received"], ["purchase", "Bought from a person"], ["refund", "Refund"], ["untracked", "From my untracked wallet"], ["self", "Matches another row…"]];
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
      <td class="small">${detail}</td>
      <td class="row"><select class="cat"><option value="">Choose…</option>${cats.map(([v, l]) => `<option value="${v}">${esc(l)}</option>`).join("")}</select>
        <span class="extra"></span><button class="save" hidden>Save</button></td></tr>`;
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

// Flow graph: one lane per account, time left to right.
const KINDS = [["buy", "Buy"], ["sell", "Sale / spend"], ["income", "Income / acquired"], ["unmatched", "Needs a label"], ["gift", "Gift / other"]];
$("#legend").innerHTML = KINDS.map(([k, l]) => `<span><svg width="10" height="10"><circle cx="5" cy="5" r="5" class="dot ${k}"></circle></svg> ${l}</span>`).join("");

function drawGraph() {
  const svg = $("#flow");
  if (!report) return;
  const ev = report.graph || [];
  const lanes = [...new Set([...(report.accounts || []).filter((a) => a.kind !== "pool").map((a) => a.name), ...ev.flatMap((e) => [e.account, e.to].filter(Boolean))])];
  if (!ev.length) { svg.setAttribute("width", 600); svg.setAttribute("height", 60); svg.innerHTML = `<text x="16" y="34">Nothing to draw yet.</text>`; return; }
  const left = 140, laneH = 56, W = Math.max(720, $("#graph-wrap").clientWidth - 2), H = lanes.length * laneH + 40;
  const t0 = Math.min(...ev.map((e) => e.t)), t1 = Math.max(...ev.map((e) => e.t)), span = Math.max(t1 - t0, 86400);
  const x = (t) => left + 16 + ((t - t0) / span) * (W - left - 56);
  const y = (name) => 20 + lanes.indexOf(name) * laneH + laneH / 2;
  const maxSats = Math.max(...ev.map((e) => e.sats), 1);
  const r = (s) => 4 + 8 * Math.sqrt(s / maxSats);
  let out = "";
  lanes.forEach((l) => { out += `<line class="lane" x1="${left}" x2="${W - 10}" y1="${y(l)}" y2="${y(l)}"/><text x="10" y="${y(l) + 4}">${esc(l.length > 18 ? l.slice(0, 17) + "…" : l)}</text>`; });
  // years along the bottom
  for (let yr = new Date(t0 * 1000).getUTCFullYear() + 1; Date.UTC(yr, 0, 1) / 1000 <= t1; yr++) {
    const xx = x(Date.UTC(yr, 0, 1) / 1000);
    out += `<line class="lane" x1="${xx}" x2="${xx}" y1="10" y2="${H - 24}" stroke-dasharray="2 4"/><text x="${xx + 3}" y="${H - 8}">${yr}</text>`;
  }
  ev.filter((e) => e.kind === "transfer" && e.to).forEach((e) => {
    const x1 = x(e.t), y1 = y(e.account), y2 = y(e.to), mid = (y1 + y2) / 2;
    out += `<path class="arc" data-key="${esc(e.key)}" d="M${x1},${y1} C${x1 + 18},${y1 + (mid - y1) * 0.6} ${x1 + 18},${y2 - (y2 - mid) * 0.6} ${x1 + 4},${y2}" stroke-width="${1 + 6 * Math.sqrt(e.sats / maxSats)}"><title>${btc(e.sats)} BTC ${esc(e.account)} → ${esc(e.to)} on ${day(e.t)}</title></path>`;
  });
  ev.filter((e) => e.kind !== "transfer").forEach((e) => {
    out += `<circle class="dot ${esc(e.kind)}" data-key="${esc(e.key)}" cx="${x(e.t)}" cy="${y(e.account)}" r="${r(e.sats)}"><title>${esc(e.kind)} ${btc(e.sats)} BTC, ${esc(e.account)}, ${day(e.t)}</title></circle>`;
  });
  svg.setAttribute("width", W);
  svg.setAttribute("height", H);
  svg.setAttribute("viewBox", `0 0 ${W} ${H}`);
  svg.innerHTML = out;
}
$("#flow").addEventListener("click", (e) => {
  const key = e.target.dataset?.key;
  $$("#flow .hl, #flow .dim").forEach((n) => n.classList.remove("hl", "dim"));
  const box = $("#graph-detail");
  if (!key) { box.hidden = true; return; }
  const ds = (report.disposals || []).filter((d) => d.OpID === key);
  const it = (report.unmatched || []).find((u) => u.Key === key);
  if (it) { location.hash = "review"; return; }
  if (!ds.length) { box.hidden = true; return; }
  const origins = new Set(ds.map((d) => "ex:" + d.OriginID.replace(/^ex:/, "")).concat(ds.map((d) => d.OriginID)));
  $$("#flow .dot").forEach((n) => n.classList.toggle("dim", !(origins.has(n.dataset.key) || n.dataset.key === key)));
  $$("#flow .dot").filter((n) => origins.has(n.dataset.key) || n.dataset.key === key).forEach((n) => n.classList.add("hl"));
  box.hidden = false;
  box.innerHTML = `<b>Sale on ${day(ds[0].Disposed)}</b> came from:<br>` + ds.map((d) => d.Missing
    ? `<span class="warn">${btc(d.Sats)} BTC with no known purchase</span>`
    : `${btc(d.Sats)} BTC bought ${day(d.Acquired)} (basis ${usd(d.Basis)}, ${d.LongTerm ? "long" : "short"} term, gain ${usd(d.Proceeds - d.Basis)})`).join("<br>");
});
window.addEventListener("resize", () => { if ($("#graph").classList.contains("on")) drawGraph(); });

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
  const hs = await api("/api/handlers");
  $("#handler-select").innerHTML = `<option value="">Detect automatically</option>` + hs.map((h) => `<option value="${esc(h.id)}">${esc(h.name)}</option>`).join("");
}
$("#settings-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  try {
    await post("/api/settings", Object.fromEntries(new FormData(e.target)));
    toast("Saved");
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
