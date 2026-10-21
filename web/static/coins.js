"use strict";
// Coins: what you hold now, for two questions.
//   Tax: which coin should I spend next? Each node is a chunk of coins from
//   one purchase in one account (a lot), linked to its account and to the
//   buy it came from, colored by the tax you'd owe if you spent it today.
//   Pick coins to estimate a sale of exactly those, or protect a purchase.
//   Privacy: what can an outside observer link together? Each node is an
//   unspent on-chain coin; coins spent together or returned as change share
//   a cluster, colored by whether their history reaches an exchange
//   withdrawal (which carries your identity).

let coinsState = null, coinsMode = "tax", picked = new Set(), pickedAcct = "";
flowViews.coins = () => drawCoins();
$$("#coins-mode input").forEach((r) => r.addEventListener("change", () => { coinsMode = r.value; coinsState?.network.destroy(); coinsState = null; drawCoins(); }));
$("#coins-fit").addEventListener("click", () => coinsState?.network.fit({ animation: { duration: 400 } }));

const COIN = { loss: "#0ea5e9", low: [22, 163, 74], mid: [234, 179, 8], high: [220, 38, 38], linked: "#f97316", clean: "#16a34a", cluster: "#94a3b8" };
const mix = (a, b, f) => a.map((v, i) => Math.round(v + (b[i] - v) * f));
const taxColor = (f) => { const c = f <= 0.5 ? mix(COIN.low, COIN.mid, f * 2) : mix(COIN.mid, COIN.high, (f - 0.5) * 2); return `rgb(${c.join(",")})`; };

function coinsLegend() {
  $("#coins-legend").innerHTML = coinsMode === "tax"
    ? `<span class="tg-key"><svg width="90" height="10"><defs><linearGradient id="cg"><stop offset="0" stop-color="rgb(22,163,74)"/><stop offset=".5" stop-color="rgb(234,179,8)"/><stop offset="1" stop-color="rgb(220,38,38)"/></linearGradient></defs><rect width="90" height="10" rx="5" fill="url(#cg)"/></svg> Tax if spent today, low to high (share of its value)</span>
       <span class="tg-key"><svg width="12" height="12"><circle cx="6" cy="6" r="5" fill="${COIN.loss}"/></svg> At a loss: spending it lowers your tax</span>
       <span class="tg-key"><svg width="12" height="12"><circle cx="6" cy="6" r="4" fill="none" stroke="#888" stroke-width="2"/></svg> Purchase it came from</span>
       <span class="tg-key"><svg width="12" height="12"><circle cx="6" cy="6" r="5" fill="#888" stroke="${cssVar("--income")}" stroke-width="2.5"/></svg> Protected (sold last)</span>`
    : `<span class="tg-key"><svg width="12" height="12"><circle cx="6" cy="6" r="5" fill="${COIN.linked}"/></svg> Linked to an exchange withdrawal (identity attached)</span>
       <span class="tg-key"><svg width="12" height="12"><circle cx="6" cy="6" r="5" fill="${COIN.clean}"/></svg> No exchange link found</span>
       <span class="tg-key"><svg width="12" height="12"><circle cx="6" cy="6" r="3" fill="${COIN.cluster}"/></svg> Cluster: coins an observer can tie together</span>`;
}

function acctNode(name, kind, text) {
  // Neutral hubs, so the coin colors carry the meaning.
  const c = kind === "exchange" ? "#6d28d9" : kind === "lightning" ? "#a16207" : "#475569";
  return { id: "acct:" + name, label: name, shape: kind === "exchange" ? "box" : kind === "lightning" ? "diamond" : "hexagon", size: 34, mass: 5,
    color: { background: c, border: c }, font: { color: text, size: 22, bold: true, strokeWidth: 4, strokeColor: cssVar("--panel") } };
}

function taxData() {
  const text = cssVar("--text"), price = report.price || 0;
  const kindOf = Object.fromEntries((report.accounts || []).map((a) => [a.name, a.kind]));
  const rate = (k) => Number(report.settings[k] || 0) / 100;
  const byKey = Object.fromEntries((report.graph || []).map((g) => [g.key, g]));
  const prot = new Set((report.graph || []).filter((g) => g.protected).map((g) => g.key));
  const now = Date.now(), yearMs = 365.25 * 86400e3;
  const lots = (report.openLots || []).filter((l) => l.Sats > 0).map((l) => {
    const value = Math.round(l.Sats * price / 1e8), gain = value - l.Basis;
    const long = now - new Date(l.Acquired).getTime() > yearMs;
    const tax = gain > 0 ? gain * (long ? rate("rateLong") : rate("rateShort")) : 0;
    return { ...l, value, gain, long, tax, frac: value > 0 ? tax / value : 0 };
  });
  const maxFrac = Math.max(...lots.map((l) => l.frac), 0.0001);
  const maxSats = Math.max(...lots.map((l) => l.Sats), 1);
  const nodes = [], edges = [], have = new Set();
  for (const l of lots) {
    const acct = l.Account || "Pooled (before 2025)";
    if (!have.has("acct:" + acct)) { have.add("acct:" + acct); nodes.push(acctNode(acct, kindOf[acct] || "other", text)); }
    const o = byKey[l.OriginID];
    const buyId = "buy:" + l.OriginID;
    if (!have.has(buyId)) {
      have.add(buyId);
      nodes.push({ id: buyId, shape: "dot", size: 5, color: { background: cssVar("--panel"), border: cssVar("--muted") }, borderWidth: 2,
        title: o ? `${KIND_NAMES[o.kind] || "Acquired"} on ${o.account}\n${btc(o.sats)} BTC, ${day(o.t)}${o.cents ? `\npaid ${usd(o.cents)}` : ""}` : `Acquired ${day(Date.parse(l.Acquired) / 1000)}` });
    }
    const c = l.gain < 0 ? COIN.loss : taxColor(l.frac / maxFrac);
    const isProt = prot.has(l.OriginID);
    nodes.push({ id: "lot:" + l.ID, lot: l, acct, shape: "dot", size: 5 + 22 * Math.sqrt(l.Sats / maxSats), color: { background: c, border: isProt ? cssVar("--income") : c },
      borderWidth: isProt ? 4 : 1,
      title: `${btc(l.Sats)} BTC in ${acct}${isProt ? " (protected)" : ""}\nbought ${day(Date.parse(l.Acquired) / 1000)} (${l.long ? "long" : "short"} term)\nbasis ${usd(l.Basis)}, worth ${usd(l.value)}\n${l.gain >= 0 ? `gain ${usd(l.gain)}, tax if spent today ≈ ${usd(Math.round(l.tax))}` : `loss ${usd(-l.gain)}`}` });
    edges.push({ from: "lot:" + l.ID, to: "acct:" + acct, color: { color: c, opacity: 0.35 }, width: 1 });
    edges.push({ from: buyId, to: "lot:" + l.ID, arrows: "to", dashes: true, color: { color: cssVar("--muted"), opacity: 0.6 }, width: 1 });
  }
  return { nodes, edges, count: lots.length };
}

async function privacyData() {
  const text = cssVar("--text");
  const { coins, clusters } = await api("/api/coins");
  const kindOf = Object.fromEntries((report.accounts || []).map((a) => [a.name, a.kind]));
  const maxSats = Math.max(...coins.map((c) => c.sats), 1);
  const size = {};
  for (const c of coins) size[c.cluster] = (size[c.cluster] || 0) + 1;
  const nodes = [], edges = [], have = new Set();
  for (const c of coins) {
    if (!have.has("acct:" + c.wallet)) { have.add("acct:" + c.wallet); nodes.push(acctNode(c.wallet, kindOf[c.wallet] || "wallet", text)); }
    const id = `utxo:${c.txid}:${c.vout}`, col = c.linked ? COIN.linked : COIN.clean;
    nodes.push({ id, coin: c, shape: "dot", size: 5 + 20 * Math.sqrt(c.sats / maxSats), color: { background: col, border: col },
      title: `${btc(c.sats)} BTC in ${c.wallet}\n${c.address}\n${c.linked ? `traces back to a withdrawal from ${c.linkedTo}` : "no exchange withdrawal in its history"}\n${size[c.cluster] > 1 ? `tied to ${size[c.cluster] - 1} other coin${size[c.cluster] > 2 ? "s" : ""} you hold` : "not tied to your other coins"}` });
    edges.push({ from: id, to: "acct:" + c.wallet, color: { color: col, opacity: 0.25 }, width: 1 });
    if (size[c.cluster] > 1) {
      const hub = "cl:" + c.cluster;
      if (!have.has(hub)) { have.add(hub); nodes.push({ id: hub, shape: "dot", size: 4, color: { background: COIN.cluster, border: COIN.cluster }, title: `Cluster of ${size[c.cluster]} coins an observer can link` }); }
      edges.push({ from: id, to: hub, color: { color: COIN.cluster, opacity: 0.7 }, width: 2 });
    }
  }
  const linked = coins.filter((c) => c.linked).length;
  return { nodes, edges, count: coins.length, summary: `${coins.length} coins in ${clusters} clusters; ${linked} linked to an exchange withdrawal.` };
}

async function drawCoins() {
  if (!report || typeof vis === "undefined") return;
  coinsLegend();
  const sig = `${coinsMode}:${(report.openLots || []).length}:${(report.graph || []).filter((g) => g.protected).length}:${report.price}`;
  if (coinsState && coinsState.sig === sig) { coinsState.network.redraw(); return; }
  coinsState?.network.destroy();
  const data = coinsMode === "tax" ? taxData() : await privacyData();
  const nodeSet = new vis.DataSet(data.nodes), edgeSet = new vis.DataSet(data.edges.map((e, i) => ({ id: i, ...e })));
  const network = new vis.Network($("#net"), { nodes: nodeSet, edges: edgeSet }, {
    autoResize: true,
    interaction: { hover: true, tooltipDelay: 100, keyboard: false, multiselect: false },
    physics: { solver: "forceAtlas2Based", forceAtlas2Based: { gravitationalConstant: -45, springLength: 50, avoidOverlap: 0.4 }, stabilization: { iterations: 300 } },
    layout: { improvedLayout: data.nodes.length < 400 },
    nodes: { font: { color: cssVar("--text"), size: 11 } },
    edges: { smooth: false, arrows: { to: { scaleFactor: 0.4 } } },
  });
  $("#coins-status").textContent = "Laying out…";
  network.once("stabilizationIterationsDone", () => {
    network.setOptions({ physics: false });
    network.fit();
    $("#coins-status").textContent = data.summary || `${data.count} lots you still hold. Click one to inspect, pick or protect it.`;
  });
  const base = new Map(data.nodes.map((n) => [n.id, n.color]));
  const faded = { background: "rgba(128,128,128,0.12)", border: "rgba(128,128,128,0.15)" };
  network.on("hoverNode", (p) => {
    const near = new Set([p.node, ...network.getConnectedNodes(p.node)]);
    // From a purchase or cluster hub, light every coin it reaches too.
    for (const id of [...near]) if (id.startsWith("buy:") || id.startsWith("cl:")) network.getConnectedNodes(id).forEach((x) => near.add(x));
    nodeSet.update(data.nodes.map((n) => ({ id: n.id, color: near.has(n.id) ? base.get(n.id) : faded })));
  });
  network.on("blurNode", () => nodeSet.update(data.nodes.map((n) => ({ id: n.id, color: base.get(n.id) }))));
  network.on("click", (p) => coinPanel(p.nodes[0] && nodeSet.get(p.nodes[0])));
  coinsState = { sig, network };
  coinPanel(null);
}

// The panel under the graph: one coin's details, protection, and the picked
// coins with an estimate of selling exactly those.
let lastCoinNode = null;
function coinPanel(n) {
  if (n !== undefined) lastCoinNode = n;
  n = lastCoinNode;
  const box = $("#coin-panel");
  let h = "";
  if (n?.lot) {
    const l = n.lot, isProt = (report.graph || []).some((g) => g.key === l.OriginID && g.protected);
    h += `<h3>${btc(l.Sats)} BTC in ${esc(n.acct)}</h3><dl class="facts">
      <dt>Bought</dt><dd>${day(Date.parse(l.Acquired) / 1000)} (${l.long ? "long" : "short"} term today)</dd>
      <dt>Cost basis</dt><dd>${usd(l.Basis)}</dd><dt>Worth today</dt><dd>${usd(l.value)}</dd>
      <dt>${l.gain >= 0 ? "Gain" : "Loss"}</dt><dd>${signed(l.gain)}</dd>
      <dt>Tax if spent today</dt><dd>${l.gain > 0 ? `≈ ${usd(Math.round(l.tax))} at your ${l.long ? "long" : "short"}-term rate` : "none; the loss offsets other gains"}</dd></dl>
      <div class="row"><button id="coin-pick" data-lot="${esc(l.ID)}" data-acct="${esc(n.acct)}">${picked.has(l.ID) ? "Unpick" : "Pick this coin"}</button>
      <button class="lb-protect-coin" data-origin="${esc(l.OriginID)}" data-on="${isProt ? 0 : 1}">${isProt ? "Stop protecting its purchase" : "Protect its purchase (sell last)"}</button>
      ${(report.graph || []).some((g) => g.key === l.OriginID) ? `<a href="${pageLink({ key: l.OriginID, txid: "" })}">Open the purchase</a>` : ""}</div>`;
  } else if (n?.coin) {
    const c = n.coin;
    h += `<h3>${btc(c.sats)} BTC in ${esc(c.wallet)}</h3><dl class="facts"><dt>Address</dt><dd class="mono small"><a href="#addr/${encodeURIComponent(c.address)}">${esc(c.address)}</a></dd>
      <dt>Created in</dt><dd><a class="mono small" href="#tx/${esc(c.txid)}">${esc(c.txid)}:${c.vout}</a></dd>
      <dt>Exchange link</dt><dd>${c.linked ? `Yes: its history reaches a withdrawal from ${esc(c.linkedTo)}` : "None found in your wallets' history"}</dd></dl>`;
  }
  if (picked.size) {
    const lots = (report.openLots || []).filter((l) => picked.has(l.ID));
    h += `<div class="label-box"><b>Picked: ${lots.length} coin${lots.length === 1 ? "" : "s"}, ${btc(lots.reduce((s, l) => s + l.Sats, 0))} BTC from ${esc(pickedAcct)}</b>
      <button id="coins-estimate" class="primary">Estimate selling exactly these</button><button id="coins-clear" class="link">Clear</button></div><div id="coins-estimate-out"></div>`;
  }
  box.innerHTML = h;
  box.hidden = !h;
}
$("#coin-panel").addEventListener("click", async (e) => {
  const t = e.target;
  if (t.id === "coin-pick") {
    const id = t.dataset.lot, acct = t.dataset.acct;
    if (picked.has(id)) picked.delete(id);
    else {
      if (pickedAcct && pickedAcct !== acct) { picked.clear(); toast("Picked coins must come from one account; started a new pick."); }
      picked.add(id); pickedAcct = acct;
    }
    if (!picked.size) pickedAcct = "";
    coinPanel();
  } else if (t.id === "coins-clear") {
    picked.clear(); pickedAcct = ""; coinPanel();
  } else if (t.id === "coins-estimate") {
    try {
      const r = await post("/api/whatif", { account: pickedAcct, lots: [...picked] });
      const g = (s) => s.proceeds - s.basis;
      $("#coins-estimate-out").innerHTML = `<p>Selling ${btc(r.sats)} BTC for ${usd(r.proceeds)}: short-term gain ${signed(g(r.short))}, long-term gain ${signed(g(r.long))},
        extra tax this year ≈ <b>${usd(Math.max(r.extraTax, 0))}</b>${r.extraTax < 0 ? ` (lowers it by ${usd(-r.extraTax)})` : ""}.</p>`;
    } catch (err) { toast(err.message); }
  } else if (t.classList.contains("lb-protect-coin")) {
    try {
      await post("/api/protect", { key: t.dataset.origin, on: t.dataset.on === "1" });
      toast(t.dataset.on === "1" ? "Protected: coins from that purchase are sold last" : "No longer protected");
      await refresh();
    } catch (err) { toast(err.message); }
  }
});
