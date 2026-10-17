"use strict";
// Network demo for the Flow page, drawn with vis-network (bundled locally
// in vendor/, no network access). Accounts are big nodes; each buy, income,
// sale, spend and on-chain transaction is a node linked to the accounts it
// touched; dashed links follow coins from the buy they came from to the sale
// or spend that used them. Hovering a node lights up its neighbours.

let netState = null; // { sig, network, nodes, edges, base }
const prevDraw = drawGraph;
drawGraph = function () {
  const on = flowMode === "network";
  $("#net").style.display = on ? "" : "none";
  $$(".network-only").forEach((el) => (el.style.display = on ? "" : "none"));
  if (!on) { prevDraw(); return; }
  $("#flow").style.display = "none";
  $("#sankey").style.display = "none";
  $("#graph-wrap").style.overflow = "hidden";
  $$(".timeline-only, .sankey-only").forEach((el) => (el.style.display = "none"));
  drawNetwork();
};
$("#net-period").addEventListener("change", () => { netState = null; drawNetwork(); });
$("#net-lots").addEventListener("change", () => { netState = null; drawNetwork(); });
$("#net-fit").addEventListener("click", () => netState?.network.fit({ animation: { duration: 400 } }));

function cssVar(name) { return getComputedStyle(document.documentElement).getPropertyValue(name).trim() || "#888"; }

function networkData(year, showLots) {
  const colors = { exchange: "#8b5cf6", wallet: cssVar("--accent"), lightning: "#eab308", other: cssVar("--move"),
    buy: cssVar("--buy"), income: cssVar("--income"), out: cssVar("--sell"), unmatched: cssVar("--warn"), tx: cssVar("--muted"), gift: cssVar("--move") };
  const text = cssVar("--text");
  const kindOf = Object.fromEntries((report.accounts || []).map((a) => [a.name, a.kind]));
  const inYear = (t) => !year || new Date(t * 1000).getUTCFullYear() === year;
  const nodes = [], edges = [], have = new Set(), maxSats = Math.max(...(report.graph || []).map((e) => e.sats), 1);
  const size = (s) => 6 + 18 * Math.sqrt(s / maxSats);
  const acct = (name) => {
    const id = "acct:" + name;
    if (!have.has(id)) {
      have.add(id);
      const k = kindOf[name] || "other";
      nodes.push({ id, label: name, shape: k === "exchange" ? "box" : k === "lightning" ? "diamond" : "hexagon", size: 40, group: "acct", mass: 6,
        color: { background: colors[k] || colors.other, border: colors[k] || colors.other }, font: { color: k === "exchange" ? "#fff" : text, size: 26, bold: true, strokeWidth: 4, strokeColor: cssVar("--panel") },
        title: `${name} (${k})` });
    }
    return id;
  };
  const evColor = (e) => e.kind === "buy" ? colors.buy : (e.kind === "income" || e.dir === "in" && e.kind !== "unmatched") ? colors.income
    : e.kind === "unmatched" ? colors.unmatched : ["sell", "spend", "sold"].includes(e.kind) ? colors.out : colors.gift;
  const txNodes = new Map();
  for (const e of report.graph || []) {
    if (!inYear(e.t)) continue;
    const tip = `${KIND_NAMES[e.kind] || e.kind}: ${btc(e.sats)} BTC\n${e.account}${e.to ? " → " + e.to : ""}\n${day(e.t)}`;
    if (e.kind === "transfer" && e.to) {
      // One node per on-chain transaction, so one-to-many and many-to-one show as a hub.
      const id = "tx:" + (e.txid || e.key);
      if (!txNodes.has(id)) {
        txNodes.set(id, true);
        nodes.push({ id, key: e.key, shape: "dot", size: 5, color: { background: colors.tx, border: colors.tx }, title: `Transaction ${e.txid ? e.txid.slice(0, 16) + "…" : ""}\n${day(e.t)}` });
      }
      edges.push({ from: acct(e.account), to: id, arrows: "to", width: 1 + 4 * Math.sqrt(e.sats / maxSats), title: tip, color: { color: colors.tx, opacity: 0.6 } });
      edges.push({ from: id, to: acct(e.to), arrows: "to", width: 1 + 4 * Math.sqrt(e.sats / maxSats), title: tip, color: { color: colors.tx, opacity: 0.6 } });
      continue;
    }
    if (e.kind === "consolidation") continue;
    const id = "ev:" + e.key, c = evColor(e);
    nodes.push({ id, key: e.key, shape: "dot", size: size(e.sats), color: { background: c, border: c, highlight: { background: c, border: text } }, title: tip });
    const incoming = e.kind === "buy" || e.kind === "income" || e.dir === "in";
    edges.push(incoming ? { from: id, to: acct(e.account), arrows: "to", color: { color: c, opacity: 0.5 } } : { from: acct(e.account), to: id, arrows: "to", color: { color: c, opacity: 0.5 } });
  }
  if (showLots) {
    // Follow coins: from the acquisition to the sale, spend or gift that used them.
    const pairs = new Map();
    for (const d of [...(report.disposals || []), ...(report.removals || [])]) {
      if (!d.OriginID) continue;
      const k = d.OriginID + "\u0000" + d.OpID;
      pairs.set(k, (pairs.get(k) || 0) + d.Sats);
    }
    const present = new Set(nodes.map((n) => n.id));
    for (const [k, sats] of pairs) {
      const [from, to] = k.split("\u0000");
      if (!present.has("ev:" + from) || !present.has("ev:" + to)) continue;
      edges.push({ from: "ev:" + from, to: "ev:" + to, dashes: true, arrows: "to", width: 1 + 3 * Math.sqrt(sats / maxSats), color: { color: colors.out, opacity: 0.35 },
        title: `${btc(sats)} BTC from this acquisition used here` });
    }
  }
  return { nodes, edges };
}

function drawNetwork() {
  if (!report || typeof vis === "undefined") return;
  const year = Number($("#net-period").value) || 0;
  const years = [...new Set((report.graph || []).map((e) => new Date(e.t * 1000).getUTCFullYear()))].sort((a, b) => b - a);
  const sel = $("#net-period");
  if (sel.options.length !== years.length + 1) {
    sel.innerHTML = `<option value="">All time</option>` + years.map((y) => `<option value="${y}">${y}</option>`).join("");
    sel.value = year ? String(year) : "";
  }
  const sig = `${(report.graph || []).length}:${year}:${$("#net-lots").checked}`;
  if (netState && netState.sig === sig) { netState.network.redraw(); return; }
  if (netState) netState.network.destroy();
  const { nodes, edges } = networkData(year, $("#net-lots").checked);
  const nodeSet = new vis.DataSet(nodes), edgeSet = new vis.DataSet(edges.map((e, i) => ({ id: i, ...e })));
  const big = nodes.length > 600;
  const network = new vis.Network($("#net"), { nodes: nodeSet, edges: edgeSet }, {
    autoResize: true,
    interaction: { hover: true, tooltipDelay: 120, hideEdgesOnDrag: big, hideEdgesOnZoom: big, navigationButtons: false, keyboard: false },
    physics: { solver: "forceAtlas2Based", forceAtlas2Based: { gravitationalConstant: -40, springLength: 60, avoidOverlap: 0.3 },
      stabilization: { iterations: big ? 250 : 400, updateInterval: 50 } },
    layout: { improvedLayout: !big },
    nodes: { borderWidth: 1, font: { color: cssVar("--text"), size: 11 } },
    edges: { smooth: false, selectionWidth: 2, arrows: { to: { scaleFactor: 0.4 } } },
  });
  $("#net-status").textContent = `Laying out ${nodes.length} nodes…`;
  network.once("stabilizationIterationsDone", () => {
    network.setOptions({ physics: false }); // freeze the layout so it doesn't drift
    network.fit();
    $("#net-status").textContent = `${nodes.length} nodes, ${edges.length} links. Drag nodes to rearrange; scroll to zoom.`;
  });
  const base = new Map(nodes.map((n) => [n.id, n.color]));
  const baseEdge = new Map(edgeSet.get().map((e) => [e.id, e.color]));
  // Hover: light up the node, its neighbours and the links between them; fade the rest.
  const fade = (on, id) => {
    if (!on) {
      nodeSet.update(nodes.map((n) => ({ id: n.id, color: base.get(n.id), font: n.font || {} })));
      edgeSet.update([...baseEdge].map(([eid, c]) => ({ id: eid, color: c })));
      return;
    }
    const near = new Set([id, ...network.getConnectedNodes(id)]);
    const nearEdges = new Set(network.getConnectedEdges(id));
    nodeSet.update(nodes.map((n) => near.has(n.id) ? { id: n.id, color: base.get(n.id) } : { id: n.id, color: { background: "rgba(128,128,128,0.15)", border: "rgba(128,128,128,0.2)" } }));
    edgeSet.update([...baseEdge].map(([eid, c]) => nearEdges.has(eid) ? { id: eid, color: { ...c, opacity: 1 } } : { id: eid, color: { color: "rgba(128,128,128,0.08)" } }));
  };
  network.on("hoverNode", (p) => fade(true, p.node));
  network.on("blurNode", () => fade(false));
  network.on("click", (p) => {
    const id = p.nodes[0];
    const n = id && nodeSet.get(id);
    if (n?.key) selectEvent(n.key, false); else selectEvent(null);
  });
  netState = { sig, network, nodes: nodeSet, edges: edgeSet };
}
drawGraph();
