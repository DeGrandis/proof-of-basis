"use strict";
// Overview summary: where did it all end up? A Sankey from how coins
// arrived, through all your bitcoin, to how they left, split into taxable
// and not taxable, with USD beside BTC. It follows the cost-basis engine's
// lots, so every band is coins traced from a source to an outcome. The
// Overview's tax-year picker filters it; "All time" shows what's still held.

let summaryAll = (() => { try { return localStorage.getItem("summaryAll") === "1"; } catch { return false; } })();
$$("#summary-scope input").forEach((r) => {
  r.checked = (r.value === "all") === summaryAll;
  r.addEventListener("change", () => { summaryAll = r.value === "all"; try { localStorage.setItem("summaryAll", summaryAll ? "1" : "0"); } catch {} drawSummary(); });
});

const OUT_GROUPS = [
  ["Taxable", ["Sold", "Spent", "Sold to people", "Network fees (as small sales)"]],
  ["Not taxable", ["Gifted, donated or lost", "Still held", "In wallets not added here"]],
];

function summaryFlows(year) {
  const byKey = Object.fromEntries((report.graph || []).map((g) => [g.key, g]));
  const inYear = (iso) => summaryAll || new Date(iso).getUTCFullYear() === year;
  const src = (origin) => {
    const o = byKey[origin];
    if (!o) return "Unknown origin";
    if (o.kind === "buy") return o.account;
    if (o.kind === "income") return o.note === "Routing fees earned" ? "Lightning routing fees" : "Income";
    return { purchase: "Bought from people", "gift-in": "Gifts received", refund: "Refunds", untracked: "From wallets not added" }[o.kind] || "Other";
  };
  const outOf = (opID) => {
    const o = byKey[opID];
    if (!o) return opID.includes(">") || opID.endsWith(":fee") ? "Network fees (as small sales)" : "Sold";
    return { sell: "Sold", spend: "Spent", sold: "Sold to people" }[o.kind] || "Spent";
  };
  const flows = new Map(); // "src\0dst" -> {sats, basis, usd}
  const add = (s, d, sats, basis, usdv) => {
    const k = s + "\u0000" + d, f = flows.get(k) || { sats: 0, basis: 0, usd: 0 };
    f.sats += sats; f.basis += basis; f.usd += usdv; flows.set(k, f);
  };
  for (const d of report.disposals || []) if (inYear(d.Disposed)) add(d.Missing ? "Unknown origin" : src(d.OriginID), outOf(d.OpID), d.Sats, d.Basis, d.Proceeds);
  for (const r of report.removals || []) if (inYear(r.Removed)) add(src(r.OriginID), "Gifted, donated or lost", r.Sats, r.Basis, r.Basis);
  const thisYear = new Date().getUTCFullYear();
  if (summaryAll || year === thisYear) {
    for (const l of report.openLots || []) {
      if (l.Sats <= 0) continue;
      add(src(l.OriginID), l.Account === "untracked" ? "In wallets not added here" : "Still held", l.Sats, l.Basis, Math.round(l.Sats * (report.price || 0) / 1e8));
    }
  }
  return flows;
}

function drawSummary() {
  const svg = $("#summary");
  if (!report || !svg) return;
  const year = Number($("#year-select").value) || new Date().getUTCFullYear();
  $("#summary-year").textContent = year;
  const flows = summaryFlows(year);
  const W = Math.max(700, $("#summary-wrap").clientWidth); // narrower panes scroll sideways
  if (!flows.size) { svg.setAttribute("width", W); svg.setAttribute("height", 40); svg.innerHTML = `<text x="8" y="24" class="sk-label">Nothing left your hands in ${year}${summaryAll ? "" : "; try All time"}.</text>`; return; }
  const srcT = new Map(), dstT = new Map();
  for (const [k, f] of flows) {
    const [s, d] = k.split("\u0000");
    const a = srcT.get(s) || { sats: 0, basis: 0 }; a.sats += f.sats; a.basis += f.basis; srcT.set(s, a);
    const b = dstT.get(d) || { sats: 0, basis: 0, usd: 0 }; b.sats += f.sats; b.basis += f.basis; b.usd += f.usd; dstT.set(d, b);
  }
  const srcs = [...srcT.keys()].sort((a, b) => srcT.get(b).sats - srcT.get(a).sats);
  const groups = OUT_GROUPS.map(([g, list]) => [g, list.filter((d) => dstT.has(d))]).filter(([, l]) => l.length);
  const dsts = groups.flatMap(([, l]) => l);
  const total = [...srcT.values()].reduce((s, a) => s + a.sats, 0);
  // Nodes and bands are strictly proportional to BTC. Labels are laid out
  // separately, nudged apart so they never overlap, with a thin leader
  // back to the node when they had to move.
  const top = 26, gap = 6, groupGap = 30, nodeW = 14;
  const leftW = Math.max(190, Math.min(230, W * 0.26)), rightW = Math.max(250, Math.min(340, W * 0.36));
  const H = Math.max(300, Math.max(srcs.length * 30, dsts.length * 42 + groups.length * groupGap) + 80);
  const k = (H - top - 24 - gap * Math.max(srcs.length, dsts.length) - groupGap * groups.length) / total;
  const h = (s) => Math.max(1, s * k);
  const xA = leftW, xM = leftW + (W - leftW - rightW) / 2 - nodeW / 2, xC = W - rightW - nodeW;
  const place = (list, size, extra) => { let y = top; return new Map(list.map((n, i) => { y += extra?.(n, i) || 0; const r = { y, h: h(size(n)) }; y += r.h + gap; return [n, r]; })); };
  const pA = place(srcs, (s) => srcT.get(s).sats);
  const groupStart = new Set(groups.map(([, l]) => l[0]));
  const pC = place(dsts, (d) => dstT.get(d).sats, (d) => groupStart.has(d) ? groupGap : 0);
  const hM = h(total), yM = top + Math.max(0, (Math.max(...[...pA.values(), ...pC.values()].map((p) => p.y + p.h)) - top - hM) / 2);
  // Label rows: centred on their node, then pushed apart top to bottom.
  const layout = (list, pos, rowH) => {
    const rows = list.map((n) => ({ n, want: pos.get(n).y + pos.get(n).h / 2, y: 0 }));
    let y = top + rowH / 2;
    for (const r of rows) { r.y = Math.max(r.want, y); y = r.y + rowH; }
    const over = y - rowH / 2 - H + 8; // pull everything up if the stack overflows
    if (over > 0) for (const r of rows) r.y -= over;
    return new Map(rows.map((r) => [r.n, r.y]));
  };
  const lA = layout(srcs, pA, 30), lC = layout(dsts, pC, 44);
  const taxable = new Set(OUT_GROUPS[0][1]);
  const exchanges = new Set((report.accounts || []).filter((a) => a.kind === "exchange").map((a) => a.name));
  const cls = (name) => exchanges.has(name) ? "buy" : /Income|routing|Gifts|Refunds|Bought from|wallets not added$/.test(name) ? "income" : name === "Unknown origin" ? "unmatched"
    : taxable.has(name) ? "sell" : name === "Still held" ? "held" : "move";
  const band = (x1, y1, x2, y2, w, c, tip) => {
    const mx = (x1 + x2) / 2;
    return `<path class="sk-band ${c}" d="M${x1},${y1} C${mx},${y1} ${mx},${y2} ${x2},${y2} L${x2},${y2 + w} C${mx},${y2 + w} ${mx},${y1 + w} ${x1},${y1 + w}Z"><title>${esc(tip)}</title></path>`;
  };
  let g = `<text class="sk-col" x="${xA + nodeW}" y="14" text-anchor="end">How it arrived</text><text class="sk-col" x="${xC}" y="14">How it left${summaryAll ? "" : ` in ${year}`}</text>`;
  let ym = yM;
  for (const s of srcs) {
    const p = pA.get(s), w = p.h;
    g += band(xA + nodeW, p.y, xM, ym, w, cls(s), `${exchanges.has(s) ? "Bought on " + s : s}: ${btc(srcT.get(s).sats)} BTC, cost ${usd(srcT.get(s).basis)}`);
    ym += w;
  }
  ym = yM;
  for (const d of dsts) {
    const p = pC.get(d), w = p.h, t = dstT.get(d);
    g += band(xM + nodeW, ym, xC, p.y, w, cls(d), `${d}: ${btc(t.sats)} BTC`);
    ym += w;
  }
  const leader = (x1, y1, x2, y2) => Math.abs(y1 - y2) > 3 ? `<line class="sk-leader" x1="${x1}" y1="${y1}" x2="${x2}" y2="${y2}"/>` : "";
  for (const s of srcs) {
    const p = pA.get(s), t = srcT.get(s), ly = lA.get(s);
    g += `<rect class="sk-node ${cls(s)}" x="${xA}" y="${p.y}" width="${nodeW}" height="${p.h}" rx="1"/>` + leader(xA - 2, p.y + p.h / 2, xA - 8, ly)
      + `<text class="sk-label" x="${xA - 10}" y="${ly}" text-anchor="end">${esc(s)}</text><text class="sk-amt" x="${xA - 10}" y="${ly + 13}" text-anchor="end">${btc(t.sats)} BTC · cost ${usd(t.basis)}</text>`;
  }
  g += `<rect class="sk-node acct" x="${xM}" y="${yM}" width="${nodeW}" height="${hM}" rx="1"/><text class="sk-label strong" x="${xM + nodeW / 2}" y="${yM - 6}" text-anchor="middle">${summaryAll ? "All your bitcoin" : `What left in ${year}`}</text>`;
  for (const [grp, list] of groups) {
    const p0 = pC.get(list[0]);
    const tx = list.reduce((s, d) => s + (taxable.has(d) ? dstT.get(d).usd - dstT.get(d).basis : 0), 0);
    g += `<text class="sk-col" x="${xC}" y="${Math.min(p0.y, lC.get(list[0]) - 22) - 8}">${grp}${grp === "Taxable" ? ` · gain ${tx >= 0 ? usd(tx) : "-" + usd(-tx)}` : ""}</text>`;
  }
  for (const d of dsts) {
    const p = pC.get(d), t = dstT.get(d), ly = lC.get(d);
    const money = taxable.has(d) ? `${usd(t.usd)} proceeds, gain ${usd(t.usd - t.basis)}` : d === "Still held" || d === "In wallets not added here" ? `worth ${usd(t.usd)} today` : `cost ${usd(t.basis)}`;
    g += `<rect class="sk-node ${cls(d)}" x="${xC}" y="${p.y}" width="${nodeW}" height="${p.h}" rx="1"/>` + leader(xC + nodeW + 2, p.y + p.h / 2, xC + nodeW + 8, ly - 4)
      + `<text class="sk-label" x="${xC + nodeW + 10}" y="${ly - 4}">${esc(d)}</text>`
      + `<text class="sk-amt" x="${xC + nodeW + 10}" y="${ly + 9}">${btc(t.sats)} BTC</text><text class="sk-amt" x="${xC + nodeW + 10}" y="${ly + 21}">${money}</text>`;
  }
  svg.setAttribute("width", W); svg.setAttribute("height", H); svg.setAttribute("viewBox", `0 0 ${W} ${H}`);
  svg.innerHTML = g;
}
window.addEventListener("resize", () => { if ($("#overview").classList.contains("on")) drawSummary(); });
