// One record, laid out the same way on its own page and in Flow's popup:
// a headline in plain words, the numbers that matter, where the coins came
// from or went, how it is treated for tax, its proof and its details. The
// popup is the compact size of the same layout.

const longDayFmt = {};
const longDay = (t) => {
  const z = tzName();
  longDayFmt[z] ||= new Intl.DateTimeFormat("en-US", { timeZone: z, year: "numeric", month: "short", day: "numeric" });
  return longDayFmt[z].format(new Date(t * 1000));
};

// What happened, as a sentence.
function recordHeadline(e) {
  const a = amt(e.sats), at = esc(e.account), to = esc(e.to || "");
  switch (e.kind) {
    case "buy": return `Bought ${a} on ${at}`;
    case "sell": return `Sold ${a} on ${at}`;
    case "income": return `Received ${a} as income in ${at}`;
    case "transfer": return `Moved ${a} from ${at} to ${to}`;
    case "consolidation": return `Merged coins inside ${at}`;
    case "spend": return `Spent ${a} from ${at}`;
    case "sold": return `Sold ${a} to a person from ${at}`;
    case "gift": return `Gave ${a} from ${at}`;
    case "donation": return `Donated ${a} from ${at}`;
    case "lost": return `Lost ${a} from ${at}`;
    case "gift-in": return `Received ${a} as a gift in ${at}`;
    case "purchase": return `Bought ${a} from a person into ${at}`;
    case "refund": return `Got ${a} back as a refund in ${at}`;
    case "untracked": return `Brought ${a} from an old wallet into ${at}`;
    case "self-transfer": return `Moved ${a} ${e.dir === "in" ? "into" : "out of"} ${at}, between your own wallets`;
    case "unmatched": return e.dir === "in" ? `Received ${a} in ${at}` : `Sent ${a} from ${at}`;
  }
  return `${esc(KIND_NAMES[e.kind] || e.kind)} ${a} · ${at}`;
}

const ACQ_VERB = { buy: "Bought", income: "Earned", "gift-in": "Gift received", purchase: "Bought from a person", refund: "Refunded", untracked: "Old wallet" };
const stat = (k, v, c = "") => `<div class="stat"><div class="k">${k}</div><div class="v">${v}</div>${c ? `<div class="c">${c}</div>` : ""}</div>`;
const sum = (xs, f) => xs.reduce((s, x) => s + f(x), 0);
// A gain with its change on the basis: "$7,413.45 +444%".
const gainPct = (gain, basis) => {
  if (!(basis > 0)) return signed(gain);
  const p = gain / basis * 100, n = Math.abs(p) >= 100 ? Math.round(p).toLocaleString("en-US") : p.toFixed(1);
  return `${signed(gain)} <span class="pct ${gain < 0 ? "neg" : gain > 0 ? "pos" : ""}">${p > 0 ? "+" : p < 0 ? "−" : ""}${n.replace("-", "")}%</span>`;
};

// The numbers that matter for this kind of record.
function recordStats(d) {
  const { e, fromLots, fromGifts, sold, gone, held } = d;
  const out = [], source = e.valueSource ? esc(e.valueSource) : "";
  const known = fromLots.filter((l) => !l.Missing);
  if (known.some((l) => l.Proceeds != null)) {
    // Net of fees, as the gain is figured; the gross is in the caption.
    const proceeds = sum(fromLots, (l) => l.Proceeds || 0), gross = e.cents && e.cents !== proceeds ? `${usd(e.cents)} less fees` : "";
    out.push(stat(e.kind === "spend" ? "Value" : "Proceeds", usd(proceeds), [gross, source || (e.kind === "sell" ? "exchange" : "")].filter(Boolean).join(" · ")));
    out.push(stat("Basis", usd(sum(known, (l) => l.Basis)), `${known.length} lot${known.length === 1 ? "" : "s"}`
      + (known.length < fromLots.length ? ` <span class="warn">· ${amt(sum(fromLots.filter((l) => l.Missing), (l) => l.Sats))} with no purchase</span>` : "")));
    const st = sum(known.filter((l) => !l.LongTerm), (l) => l.Proceeds - l.Basis), lt = sum(known.filter((l) => l.LongTerm), (l) => l.Proceeds - l.Basis);
    const longs = known.filter((l) => l.LongTerm).length;
    out.push(stat("Gain", gainPct(st + lt, sum(known, (l) => l.Basis)), longs === known.length ? "long term" : longs === 0 ? "short term" : `${usd(st)} short · ${usd(lt)} long`));
  } else if (fromGifts.length || fromLots.length) {
    const v = e.cents || e.value;
    if (v) out.push(stat("Value", usd(v), source || "that day's price"));
    out.push(stat("Basis", usd(sum([...fromLots, ...fromGifts].filter((l) => !l.Missing), (l) => l.Basis)), "goes with the coins"));
    out.push(stat("Gain", "None", "not a sale"));
  } else if (ACQUIRE_KINDS.includes(e.kind)) {
    const v = e.cents || e.value;
    out.push(stat(e.kind === "buy" ? "Cost" : "Value", v ? usd(v) : "—", source || (e.cents ? "" : e.value ? "that day's price" : "")));
    if (e.kind === "income") out.push(stat("Taxed as", "Income", esc(e.incomeType || "")));
    const heldSats = sum(held, (l) => l.Sats);
    if (sold.length || gone.length || held.length) out.push(stat("Still held", amt(heldSats), heldSats === e.sats ? "all" : heldSats ? "" : "none"));
    if (sold.length) out.push(stat("Gain so far", gainPct(sum(sold, (s) => s.Proceeds - s.Basis), sum(sold, (s) => s.Basis)), `on ${sold.length} sale${sold.length === 1 ? "" : "s"}`));
  } else {
    out.push(stat("Amount", amt(e.sats)));
    if (e.value || e.cents) out.push(stat("Value", usd(e.cents || e.value), source || "that day's price"));
    if (["transfer", "consolidation", "self-transfer"].includes(e.kind)) out.push(stat("Taxable", "No"));
    if (e.kind === "unmatched") out.push(stat("Taxable", "Depends"));
  }
  if (e.feeSats || e.feeCents) out.push(stat("Fee", e.feeSats ? amt(e.feeSats) : usd(e.feeCents), e.feeSats && e.feeCents ? usd(e.feeCents) : e.feeSats ? "network" : ""));
  return `<div class="rec-stats">${out.join("")}</div>`;
}

const chips = (accts) => accts.length > 1 ? `<div class="route">${accts.map((a, i) => `${i ? '<span class="muted">→</span>' : ""}<span class="chip">${esc(a)}</span>`).join("")}</div>` : "";

// Where the coins came from: one row per lot.
function lotRows(d, limit) {
  const { e, fromLots, fromGifts, byKey } = d;
  let lots = [...fromLots, ...fromGifts];
  if (!lots.length) return "";
  const more = limit && lots.length > limit ? lots.length - limit : 0;
  if (more) lots = [...lots].sort((a, b) => b.Sats - a.Sats).slice(0, limit);
  const row = (l) => {
    if (l.Missing) return `<li class="lot"><span class="warn">No known purchase</span><span class="num">${amt(l.Sats)}</span><span class="num">—</span><span class="num">—</span><span></span></li>`;
    const o = byKey[l.OriginID], hops = o ? route(o.account, o.t, e.account, e.t) : [];
    const gain = l.Proceeds != null ? signed(l.Proceeds - l.Basis) : '<span class="muted">—</span>';
    return `<li class="lot"><span>${pickKey(l.OriginID)}${esc(ACQ_VERB[o?.kind] || "Acquired")} ${day(l.Acquired)}</button>${o ? ` <span class="muted">on ${esc(o.account)}</span>` : ""}</span>
      <span class="num">${amt(l.Sats)}</span><span class="num">${usd(l.Basis)}</span><span class="num">${gain}</span><span class="small muted">${l.Proceeds != null ? (l.LongTerm ? "long" : "short") : ""}</span>
      ${chips(o ? [o.account, ...hops.map((h) => h.to)] : [])}</li>`;
  };
  return `<ul class="lots"><li class="lot head"><span>Acquired</span><span class="num">Amount</span><span class="num">Basis</span><span class="num">Gain</span><span>Term</span></li>${lots.map(row).join("")}</ul>`
    + (more ? `<p class="small"><a href="${pageLink(e)}">+ ${more} more</a></p>` : "");
}

// Where an acquisition's coins went: sold, left, or still held.
function wentRows(d) {
  const { e, sold, gone, held } = d;
  if (!sold.length && !gone.length && !held.length) return "";
  const row = (what, acct, sats, gain, hops) => `<li class="lot went"><span>${what} <span class="muted">${acct}</span></span><span class="num">${amt(sats)}</span><span class="num">${gain}</span>${chips(hops.length ? [e.account, ...hops.map((h) => h.to)] : [])}</li>`;
  return `<ul class="lots"><li class="lot went head"><span>What happened</span><span class="num">Amount</span><span class="num">Gain</span></li>`
    + sold.map((s) => row(`${pickKey(s.OpID)}Sold ${day(s.Disposed)}</button>`, `from ${esc(s.Account)}`, s.Sats, signed(s.Proceeds - s.Basis), route(e.account, e.t, s.Account, tsec(s.Disposed)))).join("")
    + gone.map((g) => row(`${pickKey(g.OpID)}Left ${day(g.Removed)}</button>`, `from ${esc(g.Account)}, a gift, donation or loss`, g.Sats, '<span class="muted">none</span>', route(e.account, e.t, g.Account, tsec(g.Removed)))).join("")
    + held.map((l) => row("Still held", `in ${l.Account ? esc(l.Account) : "the pre-2025 pool"}`, l.Sats, '<span class="muted">—</span>', l.Account ? route(e.account, e.t, l.Account, Infinity) : [])).join("")
    + `</ul>`;
}

function recordNote(e) {
  if (e.kind === "transfer") return `Not taxable. ${info(`Basis and purchase dates move with the coins.${e.feeSats ? " The network fee is added to basis or treated as a small sale, per Settings." : ""}`)}`;
  if (e.kind === "consolidation") return `Not taxable. ${info("Coins merged inside one wallet. Only the network fee left it, handled per Settings.")}`;
  if (e.kind === "unmatched") return "Unmatched. Label it.";
  return "";
}

const shortId = (s) => s.length > 22 ? `${s.slice(0, 10)}…${s.slice(-8)}` : s;
const copyBtn = (v) => `<button class="link small copy" data-copy="${esc(v)}" aria-label="Copy">Copy</button>`;

function recordDetails(e) {
  const rows = [["When", when(e.t)], ["Account", esc(e.account) + (e.to ? ` → ${esc(e.to)}` : "")], ["Amount", amt(e.sats)]];
  if (e.ref) rows.push(["Reference", `<span class="mono" title="How exports name this record">${esc(e.ref)}</span> ${copyBtn(e.ref)}`]);
  if (e.method) rows.push(["Matched by", esc(e.method) + (e.toKey && e.method !== "manual" && e.method !== "same transaction"
    ? ` <button class="link small" data-autounlink="${esc(e.key)}" data-autounlink2="${esc(e.toKey)}">Unlink these</button>` : "")]);
  if (e.inputs) rows.push(["Shape", `${e.inputs} input${e.inputs === 1 ? "" : "s"} → ${e.outputs} output${e.outputs === 1 ? "" : "s"}`]);
  if (e.txid) rows.push(["Transaction", `<span class="mono small" title="${esc(e.txid)}">${esc(shortId(e.txid))}</span> ${copyBtn(e.txid)}`]);
  if (e.address) rows.push(["Address", `<a class="mono small" href="#addr/${encodeURIComponent(e.address)}" title="${esc(e.address)}">${esc(shortId(e.address))}</a> ${copyBtn(e.address)}`]);
  if (e.note) rows.push(["Note", esc(e.note)]);
  return `<dl>${rows.map(([k, v]) => `<dt>${k}</dt><dd>${v}</dd>`).join("")}</dl>`;
}

function sameTxCards(d) {
  if (!d.sameTx.length) return "";
  return `<div class="rec-mini-list">${d.sameTx.map((g) => `<button class="rec-mini" data-pick="${esc(g.key)}"><span class="kind ${esc(g.kind)}">${esc(KIND_NAMES[g.kind] || g.kind)}</span>
    <span>${amt(g.sats)}</span><span class="muted small">${esc(g.account)}${g.to ? ` → ${esc(g.to)}` : ""}</span></button>`).join("")}</div>`;
}

// The on-chain transaction behind a record, from your wallets or Fulcrum.
function chainSection(tx) {
  const owner = (who) => who ? `<span class="mine">${esc(who)}</span>` : `<span class="muted">outside</span>`;
  const facts = [["Block", tx.height > 0 ? tx.height.toLocaleString("en-US") : "not confirmed"], ["Confirmed", tx.t ? when(tx.t) : "unconfirmed or unknown"],
    ["Network fee", tx.feeSats ? amt(tx.feeSats) : "unknown"],
    ["Source", tx.source === "wallets" ? "Your wallets" : tx.source === "fulcrum" ? "Fulcrum" : "Imports"]];
  let h = `<dl class="facts">${facts.map(([k, v]) => `<dt>${k}</dt><dd>${v}</dd>`).join("")}</dl>${tx.note ? `<p class="small warn">${esc(tx.note)}</p>` : ""}`;
  if (tx.inputs.length || tx.outputs.length) {
    h += `<div class="io"><div><h4>${tx.inputs.length} input${tx.inputs.length === 1 ? "" : "s"}</h4><div class="scroll"><table><tbody>${tx.inputs.map((i) => `<tr>
        <td class="addr">${i.address ? `<a href="#addr/${encodeURIComponent(i.address)}">${esc(i.address)}</a>` : ""}<div class="small"><a class="mono" href="#tx/${esc(i.prevTxid)}">${esc(shortTx(i.prevTxid))}:${i.prevVout}</a></div></td>
        <td class="num">${i.sats ? btc(i.sats) : "?"}<div class="small">${owner(i.wallet)}</div></td></tr>`).join("")}</tbody></table></div></div>
      <div><h4>${tx.outputs.length} output${tx.outputs.length === 1 ? "" : "s"}</h4><div class="scroll"><table><tbody>${tx.outputs.map((o) => `<tr>
        <td class="addr">${o.address ? `<a href="#addr/${encodeURIComponent(o.address)}">${esc(o.address)}</a>` : "(non-standard)"}<div class="small muted">#${o.vout}</div></td>
        <td class="num">${btc(o.sats)}<div class="small">${owner(o.wallet)}</div></td></tr>`).join("")}</tbody></table></div></div></div>`;
  }
  if (tx.exchange.length) h += `<p class="small muted">Exchange records with this txid: ${tx.exchange.map((x) => `${esc(x.account)} ${esc(x.kind)} ${amt(x.sats)} on ${day(x.t)}`).join("; ")}.</p>`;
  return h;
}

// An exchange record as the exchange's file had it.
function sourceSection(src) {
  const cells = Object.entries(src.row || {}).filter(([, v]) => v !== "");
  return `<p class="small muted">${esc(src.filename)}${src.line ? `, row ${src.line}` : ""} · imported ${day(src.importedAt)}</p>
    ${cells.length ? `<dl class="facts src-row">${cells.map(([k, v]) => `<dt>${esc(k)}</dt><dd>${esc(v)}</dd>`).join("")}</dl>` : `<p class="muted small">Not kept for this import.</p>`}`;
}

const sec = (title, body, cls = "") => body ? `<section class="rec-sec ${cls}"><h3>${title}</h3>${body}</section>` : "";

// The whole record. opts: compact (Flow's popup), tx (its on-chain
// transaction), source (the exchange's row).
function renderRecord(d, opts = {}) {
  const { e } = d;
  const treat = actionsFor(e) || `<p class="small muted">Nothing to set.</p>`;
  const flowTitle = d.fromLots.length || d.fromGifts.length ? "Came from" : d.sold.length || d.gone.length || d.held.length ? "Went to" : "Movement";
  const head = (actions) => `<header class="rec-head"><div class="rec-title"><span class="kind ${esc(e.kind)}">${esc(KIND_NAMES[e.kind] || e.kind)}</span>
      <h2>${recordHeadline(e)} <span class="muted">· ${longDay(e.t)}</span></h2></div>${actions}</header>`;
  if (opts.compact) {
    const lots = lotRows(d, 3), went = lots ? "" : wentRows(d), note = recordNote(e);
    return `<article class="rec-view compact" data-key="${esc(e.key)}">
      ${head(`<button class="link" id="detail-close" aria-label="Close">✕</button>`)}
      ${recordStats(d)}
      <div class="rec-treat">${treat}</div>
      ${lots ? `<h3 class="rec-sub">Came from</h3>${lots}` : went ? `<h3 class="rec-sub">Went to</h3>${went}` : note ? `<p class="small muted">${note}</p>` : ""}
      <footer class="rec-foot"><a class="button" href="${pageLink(e)}">Open full page</a><button class="link" data-rec-act="graph">Show on graph</button>
        <span class="small muted hint" title="Step through ${esc(e.account)}">← →</span></footer></article>`;
  }
  const canLabel = /class="lb-cat"/.test(treat);
  const actions = `<div class="rec-actions">${canLabel ? `<button data-rec-act="label">${e.label ? "Change label" : "Label it"}</button>` : ""}
    <button data-rec-act="proof">Attach proof</button><a class="button" href="#graph" data-flowshow="${esc(e.key)}">Show in Flow</a></div>`;
  const note = recordNote(e);
  const flow = `<svg class="txflow txf" data-key="${esc(e.key)}" role="img" aria-label="Where these coins came from and went"></svg>`
    + (d.fromLots.length || d.fromGifts.length ? lotRows(d) : wentRows(d)) + (note ? `<p class="small muted">${note}</p>` : "");
  const both = (d.fromLots.length || d.fromGifts.length) && wentRows(d);
  let bottom = "";
  if (opts.tx) bottom += sec("On chain", chainSection(opts.tx), "rec-chain");
  if (opts.source) bottom += sec("Exchange row", sourceSection(opts.source), "rec-source");
  if (d.sameTx.length) bottom += sec("Same transaction", sameTxCards(d), "rec-others");
  return `<article class="rec-view page" data-key="${esc(e.key)}">
    ${head(actions)}
    ${recordStats(d)}
    <div class="rec-body">
      <div class="rec-main">${sec(flowTitle, flow, "rec-flow")}${both ? sec("Went to", both, "rec-went") : ""}</div>
      <aside class="rec-side">${sec("Tax treatment", treat, "rec-tax")}${sec("Proof", attachBox(e.key), "rec-proof")}${sec("Details", recordDetails(e), "rec-details")}</aside>
    </div>
    ${bottom}</article>`;
}

// Header actions on a record's page.
document.addEventListener("click", (ev) => {
  const act = ev.target.closest("[data-rec-act]")?.dataset.recAct;
  if (!act) return;
  const view = ev.target.closest(".rec-view");
  if (act === "label") { const s = $(".lb-cat", view); s?.scrollIntoView({ block: "center" }); s?.focus(); }
  if (act === "proof") $(".att-add input", view)?.click();
  if (act === "graph") showOnGraph();
});
document.addEventListener("click", async (ev) => {
  const v = ev.target.closest("[data-copy]")?.dataset.copy;
  if (!v) return;
  try { await navigator.clipboard.writeText(v); toast("Copied"); } catch { toast("Couldn't copy here; select it instead"); }
});
