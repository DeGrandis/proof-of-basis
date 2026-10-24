"use strict";
// Custody: how coins moved between your accounts over time. A network
// pinned to time. No tax figures or labels here, only where coins were. X is the date, strictly
// left to right; Y is free (physics moves nodes up and down only) so lines
// untangle. Each account is a ribbon of nodes along time, sized by what it
// held at that moment; buys are dots in their exchange's color; every kind
// of move has its own color and shape. Hovering anything lights up its
// whole chain: back to the buys it came from and on to the sale or spend.

let tgState = null;
flowViews.custody = () => drawTimelineGraph();
flowViews.custody.keepsSelection = true; // its clicks open the shared details panel
const tgRebuild = () => { tgState?.network.destroy(); tgState = null; drawTimelineGraph(); };
$("#tg-period").addEventListener("change", tgRebuild);
$("#tg-transfers").addEventListener("change", tgRebuild);
$("#tg-fit").addEventListener("click", () => tgState?.network.fit({ animation: { duration: 400 } }));

// Colors come from the theme (greys for accounts and moves, ink for buys,
// the accent for sales and for the hovered chain), read when drawing so a
// theme change only needs a rebuild.
let TG_PALETTE = [], TG = {};
function tgColors() {
  TG_PALETTE = [3, 2, 4, 3, 5, 2, 4].map((i) => cssVar(`--series-${i}`)); // greys only: ink is for buys
  TG = { withdraw: cssVar("--series-3"), self: cssVar("--series-3"), deposit: cssVar("--series-3"), outside: cssVar("--series-2"), sale: cssVar("--chart-down"),
    income: cssVar("--series-3"), merge: cssVar("--series-4"), unmatched: cssVar("--neg"), buy: cssVar("--chart-up"), hot: cssVar("--chart-hot"), panel: cssVar("--bg") };
}

function tgLegend() {
  const item = (svg, label) => `<span class="tg-key">${svg} ${label}</span>`;
  const line = (c, dash) => `<svg width="22" height="10"><line x1="1" y1="5" x2="15" y2="5" stroke="${c}" stroke-width="3"${dash ? ' stroke-dasharray="3 2"' : ""}/><path d="M15,1 L21,5 L15,9 Z" fill="${c}"/></svg>`;
  const dot = (c, shape) => shape === "square" ? `<svg width="12" height="12"><rect x="1" y="1" width="10" height="10" fill="${c}"/></svg>`
    : shape === "tri" ? `<svg width="12" height="12"><path d="M1,2 L11,2 L6,11 Z" fill="${c}"/></svg>` : `<svg width="12" height="12"><circle cx="6" cy="6" r="5" fill="${c}"/></svg>`;
  $("#tg-legend").innerHTML = [
    item(`<svg width="26" height="12"><rect x="1" y="3" width="24" height="6" rx="3" fill="${TG.merge}"/></svg>`, "Account over time (thicker = more BTC)"),
    item(dot(TG.buy), "Buy"), item(dot(TG.income), "Received from outside"),
    item(line(TG.withdraw), "Exchange → wallet"), item(line(TG.self), "Between your wallets"), item(line(TG.deposit), "Wallet → exchange"),
    item(dot(TG.merge, "square"), "Coins merged in a wallet"), item(line(TG.outside, true) + dot(TG.outside, "tri"), "Sent outside"),
    item(dot(TG.sale, "tri"), "Sold or spent"), item(line(TG.hot), "Hovered chain"),
  ].join("");
}

function timelineGraphData(year, transfersOnly) {
  const evs = (report.graph || []).filter((e) => !year || new Date(e.t * 1000).getUTCFullYear() === year)
    .slice().sort((a, b) => a.t - b.t || (a.key < b.key ? -1 : 1));
  if (!evs.length) return null;
  const kindOf = Object.fromEntries((report.accounts || []).map((a) => [a.name, a.kind]));
  const t0 = evs[0].t, t1 = evs.at(-1).t;
  const pxPerDay = Math.max(2, Math.min(40, 9000 / Math.max((t1 - t0) / 86400, 1)));
  const X = (t) => ((t - t0) / 86400) * pxPerDay;
  // Accounts in a fixed order: exchanges, wallets, Lightning, others.
  const rank = { exchange: 0, wallet: 1, lightning: 2 };
  const names = [...new Set(evs.flatMap((e) => [e.account, e.to].filter(Boolean)))].filter((n) => n !== "untracked")
    .sort((a, b) => (rank[kindOf[a]] ?? 3) - (rank[kindOf[b]] ?? 3) || a.localeCompare(b));
  const color = Object.fromEntries(names.map((n, i) => [n, TG_PALETTE[i % TG_PALETTE.length]]));
  // Vertical order: exchanges on top, then each wallet placed near the
  // accounts it trades coins with most (two passes of weighted averages),
  // which keeps transfer lines short. Account ribbons hold their height;
  // events float up and down around them.
  const vol = {};
  for (const e of evs) if (e.kind === "transfer" && e.to && e.to !== "untracked") {
    vol[e.account] ??= {}; vol[e.to] ??= {};
    vol[e.account][e.to] = (vol[e.account][e.to] || 0) + e.sats; vol[e.to][e.account] = (vol[e.to][e.account] || 0) + e.sats;
  }
  let order = names.slice();
  for (let pass = 0; pass < 3; pass++) {
    const pos = Object.fromEntries(order.map((n, i) => [n, i]));
    const score = (n) => {
      if (kindOf[n] === "exchange") return pos[n] - 1000; // exchanges stay on top, in their order
      const w = vol[n] || {}; let sw = 0, sp = 0;
      for (const [m, v] of Object.entries(w)) if (pos[m] != null) { sw += v; sp += v * pos[m]; }
      return sw ? sp / sw + 0.01 * pos[n] : pos[n];
    };
    order = order.slice().sort((a, b) => score(a) - score(b));
  }
  // Space the ribbons so the whole history fills the pane's shape when fitted.
  const wrap = $("#graph-wrap"), aspect = Math.max(1, wrap.clientWidth / Math.max(wrap.clientHeight, 1));
  const gap = Math.max(190, X(t1) / aspect / Math.max(order.length, 1));
  const laneY = Object.fromEntries(order.map((n, i) => [n, i * gap]));
  const k = gap / 190; // sizes grow with the spacing so they read at the fitted zoom

  const nodes = [], edges = [], states = new Map(), lastState = {};
  const maxSats = Math.max(...evs.map((e) => e.sats), 1);
  let maxBal = 1;
  const stateNode = (acct, t, glyph) => {
    const id = `st:${acct}:${t}`;
    if (!states.has(id)) {
      const n = { id, acct, t, x: X(t), y: laneY[acct] ?? 0, fixed: { x: true, y: true }, physics: false, shape: "dot", group: "state",
        color: { background: color[acct] || "#888", border: color[acct] || "#888" }, bal: 0 };
      states.set(id, n);
      nodes.push(n);
      const prev = lastState[acct];
      if (prev) edges.push({ id: `rb:${prev}>${id}`, from: prev, to: id, ribbon: true, acct, color: { color: color[acct], opacity: 0.55 }, smooth: false });
      lastState[acct] = id;
    }
    if (glyph) Object.assign(states.get(id), glyph);
    return id;
  };
  const evNode = (e, props) => {
    const n = { id: "ev:" + e.key, key: e.key, t: e.t, x: X(e.t), y: (laneY[e.account] ?? 0) + (props.dy || 0), fixed: { x: true, y: false }, ...props };
    delete n.dy;
    nodes.push(n);
    return n.id;
  };
  const tip = (e, what) => `${what}: ${btc(e.sats)} BTC\n${e.account}${e.to ? " → " + e.to : ""}\n${day(e.t)}`;
  const width = (s) => k * (1.5 + 5 * Math.sqrt(s / maxSats));
  for (const e of evs) {
    const isEx = (a) => kindOf[a] === "exchange";
    if (e.kind === "transfer" && e.to) {
      const from = stateNode(e.account, e.t), to = e.to === "untracked" ? null : stateNode(e.to, e.t);
      const kind = isEx(e.account) && !isEx(e.to) ? "withdraw" : !isEx(e.account) && isEx(e.to) ? "deposit" : "self";
      if (to) edges.push({ id: "tr:" + e.key, key: e.key, from, to, arrows: "to", width: width(e.sats), color: { color: TG[kind] }, title: tip(e, kind === "withdraw" ? "Exchange → wallet" : kind === "deposit" ? "Wallet → exchange" : "Between your wallets"), smooth: { type: "curvedCW", roundness: 0.25 } });
      else {
        const sink = evNode(e, { shape: "triangleDown", size: 7 * k, color: { background: TG.self, border: TG.self }, title: tip(e, "To a wallet not added here"), dy: gap * 0.4 });
        edges.push({ id: "tr:" + e.key, key: e.key, from, to: sink, arrows: "to", dashes: true, width: width(e.sats), color: { color: TG.self } });
      }
      continue;
    }
    if (e.kind === "consolidation") {
      const id = stateNode(e.account, e.t, { shape: "square", merge: e.key, label: e.inputs ? `${e.inputs}→1` : "merge", font: { size: 10, color: cssVar("--text") } });
      states.get(id).mergeTip = tip(e, "Coins merged");
      continue;
    }
    if (transfersOnly) continue; // only moves between your accounts
    const st = stateNode(e.account, e.t);
    if (e.kind === "buy") {
      const id = evNode(e, { shape: "dot", size: k * (3 + 10 * Math.sqrt(e.sats / maxSats)), color: { background: TG.buy, border: TG.panel }, borderWidth: 1, title: tip(e, "Buy"), dy: -gap * 0.3 });
      edges.push({ id: "in:" + e.key, from: id, to: st, color: { color: TG.buy, opacity: 0.5 }, width: k });
    } else if (e.kind === "income" || e.dir === "in") {
      const c = TG.income;
      const id = evNode(e, { shape: "dot", size: k * (4 + 9 * Math.sqrt(e.sats / maxSats)), color: { background: c, border: c }, title: tip(e, "Received from outside"), dy: -gap * 0.3 });
      edges.push({ id: "in:" + e.key, from: id, to: st, arrows: "to", color: { color: c }, width: width(e.sats) });
    } else {
      // Leaving: sales and spends end in a red triangle; anything sent to an
      // unknown address exits down a dashed arrow to an "outside" sink.
      const sale = ["sell", "sold", "spend"].includes(e.kind);
      const c = sale ? TG.sale : TG.outside;
      const id = evNode(e, { shape: "triangleDown", size: k * (5 + 9 * Math.sqrt(e.sats / maxSats)), color: { background: c, border: c }, title: tip(e, sale ? "Sold or spent" : "Sent outside"), dy: gap * 0.35 });
      edges.push({ id: "out:" + e.key, key: e.key, from: st, to: id, arrows: "to", dashes: !sale, width: width(e.sats), color: { color: sale ? TG.sale : TG.outside } });
    }
  }
  // Size each account node by its balance right after that moment. When one
  // year is shown, earlier history sets the opening balances.
  const bal2 = {};
  if (year) for (const e of (report.graph || [])) if (new Date(e.t * 1000).getUTCFullYear() < year) {
    const add = (a, v) => { if (a) bal2[a] = (bal2[a] || 0) + v; };
    if (e.kind === "transfer" && e.to) { add(e.account, -(e.sats + (e.feeSats || 0))); add(e.to, e.sats); }
    else if (e.kind === "consolidation") add(e.account, -(e.feeSats || 0));
    else if (e.kind === "buy" || e.kind === "income" || e.dir === "in") add(e.account, e.sats);
    else add(e.account, -(e.sats + (e.kind === "sell" ? 0 : e.feeSats || 0)));
  }
  for (const e of evs) {
    const add = (a, v) => { if (a) bal2[a] = (bal2[a] || 0) + v; };
    if (e.kind === "transfer" && e.to) { add(e.account, -(e.sats + (e.feeSats || 0))); add(e.to, e.sats); }
    else if (e.kind === "consolidation") add(e.account, -(e.feeSats || 0));
    else if (e.kind === "buy" || e.kind === "income" || e.dir === "in") add(e.account, e.sats);
    else add(e.account, -(e.sats + (e.kind === "sell" ? 0 : e.feeSats || 0)));
    for (const a of [e.account, e.to]) { const n = states.get(`st:${a}:${e.t}`); if (n) n.bal = Math.max(bal2[a] || 0, 0); }
  }
  for (const n of states.values()) maxBal = Math.max(maxBal, n.bal);
  for (const n of states.values()) {
    const wallet = kindOf[n.acct] !== "exchange";
    if (n.shape !== "square") n.size = k * (wallet ? 6 + 26 * Math.sqrt(n.bal / maxBal) : 3 + 10 * Math.sqrt(n.bal / maxBal));
    else n.size = 9 * k;
    n.title = n.mergeTip || `${n.acct}\nheld ${btc(n.bal)} BTC after ${day(n.t)}`;
  }
  for (const ed of edges) if (ed.ribbon) {
    const a = states.get(ed.from), b = states.get(ed.to);
    ed.width = k * (2 + 22 * Math.sqrt(Math.min(a.bal, b.bal) / maxBal));
  }
  return { nodes, edges, X, t0, t1, laneY, color, k };
}

// The chain for a record: the buys it came from, the sale or spend it fed,
// the transfers in between, and the same transaction's other legs.
function tgChain(key) {
  const d = buildDetail(key);
  return d ? d.related : new Set([key]);
}

function drawTimelineGraph() {
  if (!report || typeof vis === "undefined") return;
  tgColors();
  tgLegend();
  const years = [...new Set((report.graph || []).map((e) => new Date(e.t * 1000).getUTCFullYear()))].sort((a, b) => b - a);
  const sel = $("#tg-period");
  if (sel.options.length !== years.length + 1) {
    const keep = sel.value;
    sel.innerHTML = `<option value="">All time</option>` + years.map((y) => `<option value="${y}">${y}</option>`).join("");
    sel.value = keep;
  }
  const year = Number(sel.value) || 0;
  const sig = `${(report.graph || []).length}:${year}:${$("#tg-transfers").checked}`;
  if (tgState && tgState.sig === sig) { tgState.network.redraw(); return; }
  if (tgState) tgState.network.destroy();
  const data = timelineGraphData(year, $("#tg-transfers").checked);
  if (!data) { $("#tg-status").textContent = "Nothing in this period."; return; }
  const nodeSet = new vis.DataSet(data.nodes), edgeSet = new vis.DataSet(data.edges);
  const network = new vis.Network($("#tg"), { nodes: nodeSet, edges: edgeSet }, {
    autoResize: true,
    interaction: { hover: true, tooltipDelay: 100, hideEdgesOnDrag: true, keyboard: false },
    physics: { solver: "barnesHut", barnesHut: { gravitationalConstant: -1800, centralGravity: 0.02, springLength: 80, springConstant: 0.02, avoidOverlap: 0.2 },
      stabilization: { iterations: 150, updateInterval: 50 } },
    layout: { improvedLayout: false },
    nodes: { borderWidth: 1, font: { color: cssVar("--text"), size: 11 } },
    edges: { selectionWidth: 1.5, arrows: { to: { scaleFactor: 0.45 } } },
  });
  // Year marks along the top, drawn under the graph.
  network.on("beforeDrawing", (ctx) => {
    const top = network.DOMtoCanvas({ x: 0, y: 0 }).y, bottom = network.DOMtoCanvas({ x: 0, y: $("#tg").clientHeight }).y;
    ctx.save();
    ctx.strokeStyle = cssVar("--line"); ctx.fillStyle = cssVar("--muted"); ctx.font = `${12 / network.getScale()}px system-ui`;
    for (let y = new Date(data.t0 * 1000).getUTCFullYear() + 1; Date.UTC(y, 0, 1) / 1000 <= data.t1; y++) {
      const x = data.X(Date.UTC(y, 0, 1) / 1000);
      ctx.setLineDash([4 / network.getScale(), 6 / network.getScale()]);
      ctx.beginPath(); ctx.moveTo(x, top); ctx.lineTo(x, bottom); ctx.stroke();
      ctx.fillText(String(y), x + 4 / network.getScale(), top + 14 / network.getScale());
    }
    ctx.restore();
  });
  // Account names stay pinned to the left edge, level with their ribbon.
  network.on("afterDrawing", (ctx) => {
    const r = window.devicePixelRatio || 1;
    ctx.save();
    ctx.setTransform(r, 0, 0, r, 0, 0);
    ctx.font = "600 12px system-ui";
    for (const [name, y] of Object.entries(data.laneY)) {
      const p = network.canvasToDOM({ x: 0, y });
      if (p.y < 8 || p.y > $("#tg").clientHeight - 4) continue;
      const w = ctx.measureText(name).width;
      ctx.fillStyle = cssVar("--bg"); ctx.globalAlpha = 0.85; ctx.fillRect(4, p.y - 18, w + 16, 16);
      ctx.globalAlpha = 1; ctx.fillStyle = data.color[name]; ctx.fillRect(6, p.y - 13, 6, 6);
      ctx.fillStyle = cssVar("--text"); ctx.fillText(name, 16, p.y - 6);
    }
    ctx.restore();
  });
  $("#tg-status").textContent = `Laying out ${data.nodes.length} nodes…`;
  let settled = false;
  const settle = () => {
    if (settled) return;
    settled = true;
    network.setOptions({ physics: false });
    network.fit();
    $("#tg-status").textContent = "";
  };
  network.once("stabilizationIterationsDone", settle);
  // With only account ribbons (Transfers only) nothing floats, so there is
  // no layout to wait for.
  if (!data.nodes.some((n) => n.physics !== false)) setTimeout(settle, 0);
  const baseN = new Map(data.nodes.map((n) => [n.id, n.color]));
  const baseE = new Map(data.edges.map((e) => [e.id, { color: e.color, width: e.width }]));
  const faded = { background: "rgba(128,128,128,0.12)", border: "rgba(128,128,128,0.15)" };
  const hot = { background: TG.hot, border: TG.hot };
  const light = (id) => {
    if (!id) {
      nodeSet.update(data.nodes.map((n) => ({ id: n.id, color: baseN.get(n.id) })));
      edgeSet.update(data.edges.map((e) => ({ id: e.id, color: baseE.get(e.id).color })));
      return;
    }
    const n = nodeSet.get(id);
    const keys = n.key ? tgChain(n.key) : n.merge ? tgChain(n.merge) : new Set();
    const onN = new Set([id]), onE = new Set();
    for (const e of data.edges) if (e.key && keys.has(e.key)) { onE.add(e.id); onN.add(e.from); onN.add(e.to); }
    for (const nd of data.nodes) if (nd.key && keys.has(nd.key)) onN.add(nd.id);
    for (const e of data.edges) if (!e.ribbon && (onN.has(e.from) && e.from.startsWith("ev:") || onN.has(e.to) && e.to.startsWith("ev:"))) { onE.add(e.id); onN.add(e.from); onN.add(e.to); }
    if (!n.key && !n.merge) for (const e of data.edges) if (e.from === id || e.to === id) { onE.add(e.id); onN.add(e.from); onN.add(e.to); }
    // Light the ribbon of each account between the first and last lit moments.
    const span = {};
    for (const nid of onN) { const s = data.nodes.find((x) => x.id === nid); if (s?.acct) { const r = span[s.acct] || [Infinity, -Infinity]; span[s.acct] = [Math.min(r[0], s.t), Math.max(r[1], s.t)]; } }
    for (const e of data.edges) if (e.ribbon && span[e.acct]) {
      const a = nodeSet.get(e.from), b = nodeSet.get(e.to);
      if (a.t >= span[e.acct][0] && b.t <= span[e.acct][1]) { onE.add(e.id); onN.add(e.from); onN.add(e.to); }
    }
    nodeSet.update(data.nodes.map((x) => ({ id: x.id, color: onN.has(x.id) ? hot : faded })));
    edgeSet.update(data.edges.map((e) => ({ id: e.id, color: onE.has(e.id) ? { color: TG.hot, opacity: 1 } : { color: "rgba(128,128,128,0.07)" } })));
  };
  network.on("hoverNode", (p) => light(p.node));
  network.on("blurNode", () => light(null));
  network.on("click", (p) => {
    const n = p.nodes[0] && nodeSet.get(p.nodes[0]);
    const key = n?.key || n?.merge || (p.edges[0] && edgeSet.get(p.edges[0])?.key);
    if (key) selectEvent(key, false); else selectEvent(null);
  });
  tgState = { sig, network };
}
drawGraph();
