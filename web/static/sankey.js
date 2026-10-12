"use strict";
// Sankey demo for the Flow page: how coins arrived, which accounts they
// sat in, and how they left, with band widths by amount. Transfers between
// your own accounts loop on the right of the account column. It reuses
// app.js globals (report, $, esc, btc, usd...) and only takes over drawing
// while "Sankey" is picked, so the timeline view is untouched.

let flowMode = (() => { try { return localStorage.getItem("flowMode") || "timeline"; } catch { return "timeline"; } })();
const timelineDraw = drawGraph;
drawGraph = function () {
  const sk = flowMode === "sankey";
  $("#flow").style.display = sk ? "none" : "";
  $("#sankey").style.display = sk ? "" : "none";
  $("#graph-wrap").style.overflow = sk ? "auto" : "";
  $$(".timeline-only").forEach((el) => (el.style.display = sk ? "none" : ""));
  $$(".sankey-only").forEach((el) => (el.style.display = sk ? "" : "none"));
  if (sk) { if (selected) selectEvent(null); drawSankey(); } else timelineDraw();
};
$$("#flow-mode input").forEach((r) => {
  r.checked = r.value === flowMode;
  r.addEventListener("change", () => {
    flowMode = r.value;
    try { localStorage.setItem("flowMode", flowMode); } catch {}
    drawGraph();
  });
});
$("#sankey-period").addEventListener("change", () => drawSankey());
$("#sankey-transfers").addEventListener("change", () => drawSankey());
let sankeyFocus = null;

const SRC = { buy: "Bought", income: "Income", "gift-in": "Gifts received", purchase: "Bought from people", refund: "Refunds",
  untracked: "From wallets not added", "self-transfer": "From wallets not added", unmatched: "Received, not labeled yet" };
const DST = { sell: "Sold", sold: "Sold", spend: "Spent", gift: "Gifted, donated or lost", donation: "Gifted, donated or lost", lost: "Gifted, donated or lost",
  untracked: "To wallets not added", "self-transfer": "To wallets not added", unmatched: "Sent out, not labeled yet" };
const DST_ORDER = ["Sold", "Spent", "Gifted, donated or lost", "To wallets not added", "Sent out, not labeled yet", "Network fees", "Still held"];

function sankeyData(year) {
  const kindOf = Object.fromEntries((report.accounts || []).map((a) => [a.name, a.kind]));
  const inYear = (t) => !year || new Date(t * 1000).getUTCFullYear() === year;
  const A = new Map(), C = new Map(), T = new Map(), acct = new Map();
  const bump = (m, k, v) => m.set(k, (m.get(k) || 0) + v);
  const node = (name) => { if (!acct.has(name)) acct.set(name, { name, kind: kindOf[name] || "other", inA: 0, outC: 0, tIn: 0, tOut: 0 }); return acct.get(name); };
  for (const e of report.graph || []) {
    if (!inYear(e.t) || !e.sats) continue;
    if (e.kind === "transfer" && e.to) {
      bump(T, e.account + "\u0000" + e.to, e.sats);
      node(e.account).tOut += e.sats; node(e.to).tIn += e.sats;
      if (e.feeSats) { bump(C, "Network fees\u0000" + e.account, e.feeSats); node(e.account).outC += e.feeSats; }
      continue;
    }
    if (e.kind === "consolidation") {
      if (e.feeSats) { bump(C, "Network fees\u0000" + e.account, e.feeSats); node(e.account).outC += e.feeSats; }
      continue;
    }
    const dirIn = e.kind === "buy" || e.kind === "income" || (e.dir === "in");
    if (dirIn) {
      const src = e.kind === "buy" ? `Bought on ${e.account}` : SRC[e.kind] || SRC[e.label] || "Other incoming";
      bump(A, src + "\u0000" + e.account, e.sats);
      node(e.account).inA += e.sats;
    } else {
      const dst = DST[e.kind] || DST[e.label] || "Other outgoing";
      bump(C, dst + "\u0000" + e.account, e.sats);
      node(e.account).outC += e.sats;
      if (e.feeSats && e.kind !== "sell") { bump(C, "Network fees\u0000" + e.account, e.feeSats); node(e.account).outC += e.feeSats; }
    }
  }
  if (!year) {
    for (const a of report.accounts || []) {
      if (a.kind === "pool" || a.sats <= 0) continue;
      bump(C, "Still held\u0000" + a.name, a.sats);
      node(a.name).outC += a.sats;
    }
  }
  return { A, C, T, acct };
}

function drawSankey() {
  const svg = $("#sankey");
  if (!report) return;
  const year = Number($("#sankey-period").value) || 0;
  const years = [...new Set((report.graph || []).map((e) => new Date(e.t * 1000).getUTCFullYear()))].sort((a, b) => b - a);
  const sel = $("#sankey-period");
  if (sel.options.length !== years.length + 1) sel.innerHTML = `<option value="">All time</option>` + years.map((y) => `<option value="${y}"${y === year ? " selected" : ""}>${y}</option>`).join("");
  const showT = $("#sankey-transfers").checked;
  const { A, C, T, acct } = sankeyData(year);
  const wrap = $("#graph-wrap"), W = Math.max(640, wrap.clientWidth), H = Math.max(360, wrap.clientHeight);
  if (!acct.size) { svg.setAttribute("width", W); svg.setAttribute("height", 60); svg.innerHTML = `<text x="16" y="34" class="sk-label">Nothing to draw for this period.</text>`; return; }

  // Columns: sources, accounts, destinations. Each node is as tall as the
  // larger of what flows into and out of it.
  const kindRank = { exchange: 0, wallet: 1, lightning: 2, other: 3 };
  const accts = [...acct.values()].sort((a, b) => (kindRank[a.kind] ?? 3) - (kindRank[b.kind] ?? 3) || a.name.localeCompare(b.name));
  const srcTotals = new Map(), dstTotals = new Map();
  for (const [k, v] of A) srcTotals.set(k.split("\u0000")[0], (srcTotals.get(k.split("\u0000")[0]) || 0) + v);
  for (const [k, v] of C) dstTotals.set(k.split("\u0000")[0], (dstTotals.get(k.split("\u0000")[0]) || 0) + v);
  const srcs = [...srcTotals.keys()].sort((a, b) => srcTotals.get(b) - srcTotals.get(a));
  const dsts = [...dstTotals.keys()].sort((a, b) => (DST_ORDER.indexOf(a) + 99) % 99 - (DST_ORDER.indexOf(b) + 99) % 99);
  const acctH = (a) => Math.max(a.inA, a.outC + (showT ? a.tOut + a.tIn : 0));
  const top = 28, gap = 10, bottom = 16;
  const tot = (arr) => arr.reduce((s, x) => s + x, 0);
  const colA = tot(srcs.map((s) => srcTotals.get(s))), colB = tot(accts.map(acctH)), colC = tot(dsts.map((d) => dstTotals.get(d)));
  const k = Math.min(...[[colA, srcs.length], [colB, accts.length], [colC, dsts.length]].filter(([v]) => v > 0)
    .map(([v, n]) => (H - top - bottom - gap * (n - 1)) / v));
  const h = (v) => Math.max(2, v * k);
  const nodeW = 14, labelW = Math.min(200, W * 0.2);
  const xA = labelW, xB = W / 2 - nodeW / 2 - 40, xC = W - labelW - nodeW, xLoop = xB + nodeW + Math.min(90, (xC - xB) * 0.25);
  const place = (items, heightOf) => { let y = top; return new Map(items.map((it) => { const r = { y, h: heightOf(it) }; y += r.h + gap; return [it.name ?? it, r]; })); };
  const pA = place(srcs, (s) => h(srcTotals.get(s))), pB = place(accts, (a) => h(acctH(a))), pC = place(dsts, (d) => h(dstTotals.get(d)));
  const colorOf = (name) => name.startsWith("Bought") ? "buy" : name === "Income" || name.startsWith("Gifts") || name.startsWith("Refunds") ? "income"
    : name === "Sold" || name === "Spent" ? "sell" : name.includes("not labeled") ? "unmatched" : name === "Still held" ? "held" : name === "Network fees" ? "fee" : "move";
  const band = (x1, y1, x2, y2, w, cls, tip, keys) => {
    const mx = (x1 + x2) / 2;
    return `<path class="sk-band ${cls}" data-acct="${esc(keys)}" d="M${x1},${y1} C${mx},${y1} ${mx},${y2} ${x2},${y2} L${x2},${y2 + w} C${mx},${y2 + w} ${mx},${y1 + w} ${x1},${y1 + w}Z"><title>${esc(tip)}</title></path>`;
  };
  const worth = (s) => report.price ? ` (${usd(Math.round(s * report.price / 1e8))} today)` : "";
  let g = `<text class="sk-col" x="${xA + nodeW}" y="16" text-anchor="end">How coins arrived</text><text class="sk-col" x="${xB + nodeW / 2}" y="16" text-anchor="middle">Your accounts</text><text class="sk-col" x="${xC}" y="16">How they left</text>`;
  // Sources -> accounts
  const outA = new Map(srcs.map((s) => [s, pA.get(s).y])), inB = new Map(accts.map((a) => [a.name, pB.get(a.name).y]));
  for (const s of srcs) for (const a of accts) {
    const v = A.get(s + "\u0000" + a.name); if (!v) continue;
    const w = h(v), y1 = outA.get(s), y2 = inB.get(a.name);
    g += band(xA + nodeW, y1, xB, y2, w, colorOf(s), `${s} → ${a.name}: ${btc(v)} BTC`, a.name);
    outA.set(s, y1 + w); inB.set(a.name, y2 + w);
  }
  // Accounts -> destinations
  const outB = new Map(accts.map((a) => [a.name, pB.get(a.name).y])), inC = new Map(dsts.map((d) => [d, pC.get(d).y]));
  for (const a of accts) for (const d of dsts) {
    const v = C.get(d + "\u0000" + a.name); if (!v) continue;
    const w = h(v), y1 = outB.get(a.name), y2 = inC.get(d);
    g += band(xB + nodeW, y1, xC, y2, w, colorOf(d), `${a.name} → ${d}: ${btc(v)} BTC${worth(v)}`, a.name);
    outB.set(a.name, y1 + w); inC.set(d, y2 + w);
  }
  // Transfers between accounts: loops on the right of the account column.
  if (showT) {
    const tIn = new Map(accts.map((a) => [a.name, outB.get(a.name) + accts.find((x) => x.name === a.name).tOut * k]));
    for (const [key, v] of [...T].sort((a, b) => b[1] - a[1])) {
      const [from, to] = key.split("\u0000"), w = h(v);
      const y1 = outB.get(from), y2 = tIn.get(to);
      outB.set(from, y1 + w); tIn.set(to, y2 + w);
      const x0 = xB + nodeW, xr = Math.min(xLoop, x0 + 30 + Math.abs(y2 - y1) * 0.3);
      g += `<path class="sk-band transfer" data-acct="${esc(from)}|${esc(to)}" d="M${x0},${y1} C${xr},${y1} ${xr},${y2} ${x0},${y2} L${x0},${y2 + w} C${xr + w},${y2 + w} ${xr + w},${y1 + w} ${x0},${y1 + w}Z"><title>${esc(`${from} → ${to}: ${btc(v)} BTC moved between your accounts`)}</title></path>`;
    }
  }
  // Nodes and labels
  for (const s of srcs) { const p = pA.get(s); g += `<rect class="sk-node ${colorOf(s)}" x="${xA}" y="${p.y}" width="${nodeW}" height="${p.h}" rx="2"/><text class="sk-label" x="${xA - 6}" y="${p.y + p.h / 2 + 4}" text-anchor="end">${esc(s)} <tspan class="sk-amt">${btc(srcTotals.get(s))}</tspan></text>`; }
  for (const a of accts) {
    const p = pB.get(a.name);
    g += `<g class="sk-acct" data-name="${esc(a.name)}"><rect class="sk-node acct ${esc(a.kind)}" x="${xB}" y="${p.y}" width="${nodeW}" height="${p.h}" rx="2"><title>${esc(a.name)}: in ${btc(a.inA + a.tIn)} BTC, out ${btc(a.outC + a.tOut)} BTC</title></rect>`
      + `<text class="sk-label strong" x="${xB - 6}" y="${p.y + p.h / 2 + 4}" text-anchor="end">${esc(a.name)}</text></g>`;
  }
  for (const d of dsts) { const p = pC.get(d); g += `<rect class="sk-node ${colorOf(d)}" x="${xC}" y="${p.y}" width="${nodeW}" height="${p.h}" rx="2"/><text class="sk-label" x="${xC + nodeW + 6}" y="${p.y + p.h / 2 + 4}">${esc(d)} <tspan class="sk-amt">${btc(dstTotals.get(d))}</tspan></text>`; }
  const usedH = Math.max(...[...pA.values(), ...pB.values(), ...pC.values()].map((p) => p.y + p.h)) + bottom;
  svg.setAttribute("width", W); svg.setAttribute("height", Math.max(H, usedH)); svg.setAttribute("viewBox", `0 0 ${W} ${Math.max(H, usedH)}`);
  svg.innerHTML = g;
  focusSankey(sankeyFocus);
}
function focusSankey(name) {
  sankeyFocus = name;
  $$("#sankey .sk-band").forEach((b) => b.classList.toggle("dim", !!name && !b.dataset.acct.split("|").includes(name)));
  $$("#sankey .sk-acct").forEach((n) => n.classList.toggle("sel", n.dataset.name === name));
}
$("#sankey").addEventListener("click", (e) => {
  const n = e.target.closest(".sk-acct")?.dataset.name;
  focusSankey(n && n !== sankeyFocus ? n : null);
});
drawGraph();
