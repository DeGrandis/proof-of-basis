// Review on a phone: one item at a time. The item fills the card, its
// label choices sit below as buttons, and saving moves on to the next.
// Skip and Back step through without labeling; Undo takes back the last
// label. Desktop keeps the table.

const deck = { key: null, skipped: new Set(), last: null, busy: false };

// Short names for the buttons; the table keeps the long ones.
const DECK_LABELS = {
  out: { "self-transfer": ["To my own wallet", "one not tracked here"], self: ["Matches another item", "pick it next"], spend: ["Paid for something"],
    sold: ["Sold to a person"], gift: ["Gift"], donation: ["Donation"], lost: ["Lost or stolen"] },
  in: { "self-transfer": ["From my own wallet", "one not tracked here"], self: ["Matches another item", "pick it next"], income: ["Income"],
    "gift-in": ["Gift received"], purchase: ["Bought from a person"], refund: ["Refund"], untracked: ["From an old wallet", "enter its basis"] },
};

const deckItems = () => [...(report?.unmatched || [])].sort((a, b) => new Date(a.Time) - new Date(b.Time) || (a.Key < b.Key ? -1 : 1));
const shortMid = (s) => s.length > 20 ? `${s.slice(0, 10)}…${s.slice(-6)}` : s;

function renderDeck() {
  const box = $("#review-deck");
  if (!box) return;
  const items = deckItems();
  const undo = deck.last ? `<div class="deck-undo small">Labeled <b>${esc(deck.last.name)}</b> <button type="button" class="link" data-deck="undo">Undo</button></div>` : "";
  if (!items.length) { box.innerHTML = undo + `<p class="muted">Nothing to label.</p>`; return; }
  let i = items.findIndex((x) => x.Key === deck.key);
  if (i < 0) i = Math.max(0, items.findIndex((x) => !deck.skipped.has(x.Key)));
  const it = items[i];
  deck.key = it.Key;
  const dir = it.Dir === "out" ? "out" : "in", net = NET_ICON[it.Network] ? it.Network : "unknown";
  const facts = [];
  if (it.Address) facts.push([dir === "out" ? "To" : "From", `<span class="mono" title="${esc(it.Address)}">${esc(shortMid(it.Address))}</span>`]);
  if (it.Txid) facts.push(["Transaction", `<span class="mono" title="${esc(it.Txid)}">${esc(shortMid(it.Txid))}</span>`]);
  if (it.Source !== "chain" && it.Kind) facts.push(["Exchange says", esc(it.Kind)]);
  if (it.Note) facts.push(["Note", esc(it.Note)]);
  const same = it.Txid ? (report.graph || []).filter((g) => g.txid === it.Txid && g.key !== it.Key) : [];
  if (same.length) facts.push(["Same transaction", same.map((g) => `${esc(KIND_NAMES[g.kind] || g.kind)} ${amt(g.sats)}${g.to ? ` → ${esc(g.to)}` : ` · ${esc(g.account)}`}`).join("<br>")]);
  const rec = (report.graph || []).find((g) => g.key === it.Key);
  const choices = Object.entries(DECK_LABELS[dir]).map(([cat, [name, sub]]) =>
    `<button type="button" class="deck-choice" data-cat="${cat}">${esc(name)}${sub ? `<span class="small muted">${esc(sub)}</span>` : ""}</button>`).join("");
  box.innerHTML = `<div class="deck-top"><b>${i + 1}</b> <span class="muted">of ${items.length}</span><progress max="${items.length}" value="${i}"></progress></div>${undo}
    <article class="deck-card" data-key="${esc(it.Key)}" data-dir="${dir}" data-value="${it.Value || 0}">
      <div class="deck-meta">${dirTag(it.Dir)}<span class="tag ${net}">${NET_ICON[net][1]}${NET_ICON[net][0]}</span><span class="deck-when muted small">${when(tsec(it.Time))}</span></div>
      <div class="deck-acct">${esc(it.Account)}</div>
      <div class="deck-amt">${dir === "out" ? "−" : "+"}${amt(it.Sats)}</div>
      <div class="small muted">${it.Value ? `≈ ${usd(it.Value)} that day` : "No price for that day"}${it.FeeSats ? ` · fee ${amt(it.FeeSats)}` : ""}</div>
      ${facts.length ? `<dl class="deck-facts">${facts.map(([k, v]) => `<dt>${k}</dt><dd>${v}</dd>`).join("")}</dl>` : ""}
      <div class="deck-links small">${rec ? `<a href="${pageLink(rec)}">Open</a>` : ""}<button type="button" class="link ai-ask" data-ask="${esc(it.Key)}" data-ask-q="What is this item, and how should I label it?">Ask the assistant</button></div>
    </article>
    <div class="deck-q">What was it?</div>
    <div class="deck-choices">${choices}</div>
    <div class="deck-follow" hidden></div>
    <div class="deck-nav"><button type="button" data-deck="back"${i === 0 ? " disabled" : ""}>← Back</button><button type="button" data-deck="skip"${items.length < 2 ? " disabled" : ""}>Skip →</button></div>`;
}

// The follow-up a label needs before it can be saved, or null.
function deckFollow(it, cat) {
  const v = Number(it.dataset.value);
  if (cat === "self") {
    // Items going the other way, closest in amount and time first.
    const me = deckItems().find((x) => x.Key === it.dataset.key);
    const opp = deckItems().filter((x) => x.Dir !== me.Dir)
      .sort((a, b) => Math.abs(a.Sats - me.Sats) / me.Sats + Math.abs(new Date(a.Time) - new Date(me.Time)) / 864e8
        - (Math.abs(b.Sats - me.Sats) / me.Sats + Math.abs(new Date(b.Time) - new Date(me.Time)) / 864e8));
    if (!opp.length) return `<p class="small warn">Nothing going the other way to match it with.</p>`;
    return `<div class="deck-q">Which item is the other side?</div><div class="deck-pick">${opp.slice(0, 6).map((x) =>
      `<button type="button" class="deck-choice" data-cp="${esc(x.Key)}">${esc(x.Account)} ${amt(x.Sats)}<span class="small muted">${day(x.Time)}${x.Sats === me.Sats ? " · same amount" : ""}</span></button>`).join("")}</div>`;
  }
  if (MARKET_VALUE[cat]) {
    return `<label>${esc(MARKET_VALUE[cat])}<input class="amt" inputmode="decimal" placeholder="${v ? usd(v) : "USD"}"></label>
      <p class="small muted">${v ? `Leave it empty to use ${usd(v)}, that day's value.` : "No price for that day: enter the amount."}</p>
      <button type="button" class="primary" data-deck="save">Save</button>`;
  }
  if (needsAmount[cat]) {
    return `<label>${esc(needsAmount[cat])}<input class="amt" inputmode="decimal" placeholder="USD"></label>
      ${cat === "gift-in" ? `<label>Donor's purchase date<input class="acq" type="date"></label>` : ""}
      <button type="button" class="primary" data-deck="save">Save</button>`;
  }
  return null;
}

async function deckSave(label, name) {
  if (deck.busy) return;
  deck.busy = true;
  const items = deckItems(), i = items.findIndex((x) => x.Key === label.Key);
  // Next comes the item after this one, skipping ones set aside.
  const rest = [...items.slice(i + 1), ...items.slice(0, i)].filter((x) => x.Key !== label.Counterpart);
  deck.key = (rest.find((x) => !deck.skipped.has(x.Key)) || rest[0])?.Key || null;
  try {
    await post("/api/labels", label);
    deck.last = { key: label.Key, name };
    await refresh();
  } catch (err) { toast(err.message); deck.key = label.Key; renderDeck(); }
  deck.busy = false;
}

$("#review-deck").addEventListener("click", async (e) => {
  const card = $(".deck-card", e.currentTarget);
  const act = e.target.closest("[data-deck]")?.dataset.deck;
  const items = deckItems(), i = items.findIndex((x) => x.Key === deck.key);
  if (act === "skip" || act === "back") {
    if (act === "skip") deck.skipped.add(deck.key);
    deck.key = items[(i + (act === "skip" ? 1 : -1) + items.length) % items.length]?.Key;
    renderDeck();
    return;
  }
  if (act === "undo") {
    const k = deck.last.key;
    try { await api(`/api/labels/${encodeURIComponent(k)}`, { method: "DELETE" }); deck.last = null; deck.key = k; await refresh(); } catch (err) { toast(err.message); }
    return;
  }
  const cp = e.target.closest("[data-cp]")?.dataset.cp;
  if (cp) { deckSave({ Key: card.dataset.key, Category: "self", Counterpart: cp }, "Self transfer"); return; }
  const choice = e.target.closest(".deck-choice[data-cat]");
  if (choice) {
    const cat = choice.dataset.cat, name = choice.firstChild.textContent;
    $$(".deck-choice", e.currentTarget).forEach((b) => b.classList.toggle("on", b === choice));
    const follow = deckFollow(card, cat), panel = $(".deck-follow", e.currentTarget);
    if (follow == null) { panel.hidden = true; deckSave({ Key: card.dataset.key, Category: cat }, name); return; }
    panel.innerHTML = follow;
    panel.hidden = false;
    panel.dataset.cat = cat;
    panel.dataset.name = name;
    panel.scrollIntoView({ block: "nearest", behavior: "smooth" });
    $(".amt", panel)?.focus({ preventScroll: true });
    return;
  }
  if (act === "save") {
    const panel = $(".deck-follow", e.currentTarget), label = { Key: card.dataset.key, Category: panel.dataset.cat };
    const a = $(".amt", panel)?.value.replace(/[$,\s]/g, "");
    if (a) label.FiatCents = Math.round(Number(a) * 100);
    else if (needsAmount[label.Category]) { toast("Enter the amount"); return; }
    const acq = $(".acq", panel)?.value;
    if (acq) label.AcquiredAt = acq + "T00:00:00Z";
    deckSave(label, panel.dataset.name);
  }
});
if (report) renderDeck();
