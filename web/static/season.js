"use strict";
// Tax season: five steps that gather what one tax year needs, check it,
// and hand it to a tax professional. Each step's state comes from the
// readiness checks; any step can be opened and changed at any time.

const STEP_CHECKS = {
  records: (id) => ["basis", "review", "dips", "prices", "fresh", "balances"].includes(id),
  forms: (id) => id.startsWith("1099da:") || id === "reconcile",
  details: (id) => ["allocation", "income-type", "rewards", "gifts"].includes(id) || id.startsWith("ident-"),
};
const seasonOpen = {}; // year -> step -> open, as the user left it

// Where a check's "Go there" leads when the fix is on this page: the step
// to open and the part of it to show.
const CHECK_GOTO = {
  basis: ["numbers", "#forms-table"],
  reconcile: ["numbers", "#forms-table"],
  allocation: ["details", "#season-alloc"],
  "income-type": ["details", "#income-types-table"],
  rewards: ["details", "#income-types-table"],
  gifts: ["details", "#gifts-table"],
};
function checkGoto(c) {
  if (c.id.startsWith("1099da:")) {
    const acct = c.id.slice(7);
    return ["forms", `.tf-card[data-account="${CSS.escape(acct)}"], #taxform-other`];
  }
  return CHECK_GOTO[c.id];
}
function checkLink(c) {
  if (c.status === "ok") return "";
  const g = checkGoto(c);
  if (g) return ` <a class="small" href="#taxes" data-goto-step="${g[0]}" data-goto="${esc(g[1])}">Go there</a>`;
  return c.link ? ` <a class="small" href="${esc(c.link)}">Go there</a>` : "";
}

async function renderSeason() {
  if (!report) return;
  const thisYear = new Date().getFullYear();
  const years = [...new Set([...(report.years || []).map((y) => y.year)])].sort((a, b) => b - a);
  // Default to the current tax season: last year's return until the
  // extended deadline (October 15), then this year's.
  const now = new Date(), season = now.getMonth() > 9 || (now.getMonth() === 9 && now.getDate() > 15) ? thisYear : thisYear - 1;
  if (!years.includes(formsYear)) formsYear = years.includes(season) ? season : years.find((y) => y < season) || years[0] || season;
  $("#season-year").innerHTML = (years.length ? years : [formsYear])
    .map((y) => `<option value="${y}"${y === formsYear ? " selected" : ""}>${y}${y === thisYear ? " (in progress)" : ""}</option>`).join("");
  const year = formsYear;
  let r, f;
  try {
    [r, f] = await Promise.all([api(`/api/readiness?year=${year}`), api(`/api/forms?year=${year}`)]);
  } catch (e) { toast(e.message); return; }
  if (year !== formsYear) return; // the year changed while loading

  // Each step's checks and state.
  const icon = { ok: '<span class="pos" aria-label="done">✓</span>', warn: '<span class="warn" aria-label="worth a look">!</span>', bad: '<span class="neg" aria-label="needs fixing">✕</span>' };
  const order = { bad: 0, warn: 1, ok: 2 };
  const stateOf = (checks) => checks.some((c) => c.status === "bad") ? "bad" : checks.some((c) => c.status === "warn") ? "warn" : "ok";
  const states = {};
  for (const [step, test] of Object.entries(STEP_CHECKS)) {
    const checks = r.checks.filter((c) => test(c.id)).sort((a, b) => order[a.status] - order[b.status]);
    $(`.checklist[data-checks="${step}"]`).innerHTML = checks.map((c) => `<li>${icon[c.status]} <b>${esc(c.title)}</b>${c.detail ? `<br><span class="small muted">${esc(c.detail)}</span>` : ""}${checkLink(c)}</li>`).join("");
    states[step] = stateOf(checks);
  }
  if (year < 2025) states.forms = "na";
  states.numbers = r.reviewed ? "ok" : "todo";
  states.handoff = r.downloaded ? "ok" : "todo";
  const label = { ok: "Done", warn: "Worth a look", bad: "Needs fixing", todo: "To do", na: "Not needed" };
  const steps = $$("#season-steps .step");
  for (const li of steps) {
    const st = states[li.dataset.step];
    li.dataset.state = st;
    $(".step-state", li).innerHTML = `<span class="step-pill ${st}">${label[st]}</span>`;
  }
  const done = steps.filter((li) => ["ok", "na"].includes(li.dataset.state)).length;
  $("#season-progress").textContent = `${done} of ${steps.length} steps done`;
  // Open the first step that isn't done, unless the user chose otherwise.
  const chosen = seasonOpen[year] || {};
  const first = steps.find((li) => !["ok", "na"].includes(li.dataset.state));
  for (const li of steps) {
    const open = li.dataset.step in chosen ? chosen[li.dataset.step] : li === first;
    li.classList.toggle("open", open);
    $(".step-head", li).setAttribute("aria-expanded", open);
  }

  renderSeasonForms(f, year);
  fillForms(f);
  renderSeasonDetails(year);
  renderSeasonSummary(r, year);
  renderHandoff(r, year);
}

// Step 2: one card per exchange, each a drop target for its 1099-DA.
const TF_STATUS = {
  "no form": () => `<span class="warn">No 1099-DA yet.</span>`,
  "no totals": () => `<span class="warn">Type the form's total proceeds to check it.</span>`,
  match: (c) => `<span class="pos">✓ The form's total proceeds match your records.</span>`,
  differs: (c) => `<span class="neg">✕ The form shows ${usd(c.formProceeds)}: ${usd(Math.abs(c.diff))} ${c.diff < 0 ? "less" : "more"} than your records.</span>`,
  lines: () => `<span class="pos">✓ Checked sale by sale against the imported lines.</span>`,
};
let tfFocus = 0; // a form just added, to put the cursor on its total
function renderSeasonForms(f, year) {
  const exch = (report.accounts || []).filter((a) => a.kind === "exchange").map((a) => a.name);
  $("#da-account").innerHTML = exch.map((n) => `<option>${esc(n)}</option>`).join("") || `<option value="">No exchange accounts</option>`;
  if (year < 2025) {
    $("#taxform-list").innerHTML = `<p class="small muted">Exchanges started sending Form 1099-DA for 2025. Nothing to collect for ${year}.</p>`;
    $("#taxform-other").innerHTML = "";
    return;
  }
  const checks = f.checks || [];
  $("#taxform-list").innerHTML = checks.map((c) => `<div class="tf-card" data-account="${esc(c.account)}">
      <div class="tf-head"><b>${esc(c.account)}</b><span class="small muted">${c.sales} sale${c.sales === 1 ? "" : "s"}, proceeds ${usd(c.proceeds)} in your records</span></div>
      <div class="small">${TF_STATUS[c.status](c)}</div>
      ${c.forms.map((x) => `<div class="tf-form row small" data-id="${x.id}">
        <a href="/api/taxforms/${x.id}" title="Download">${esc(x.filename)}</a>
        <label>Total proceeds <input class="tf-p" inputmode="decimal" size="11" value="${x.totals.proceeds != null ? (x.totals.proceeds / 100).toFixed(2) : ""}" placeholder="$0.00"></label>
        <label>Total basis <input class="tf-b" inputmode="decimal" size="11" value="${x.totals.basis != null ? (x.totals.basis / 100).toFixed(2) : ""}" placeholder="if shown"></label>
        <button class="tf-save">Save</button><button class="link tf-del">Remove</button></div>`).join("")}
      ${report.settings.demo ? `<p class="small muted">Uploads are off in the demo.</p>` : `<label class="dropzone"><input type="file" class="tf-file" accept=".pdf,application/pdf,image/*" hidden>
        ${c.forms.length ? "Drop another page or form here" : `Drop ${esc(c.account)}'s 1099-DA here`}, or <span class="link">choose the file</span></label>`}
    </div>`).join("") || `<p class="small muted">No exchange sales in ${year}, so no 1099-DA to collect.</p>`;
  // A form from an exchange with no sales here still matters: it may show
  // sales that were never imported.
  const others = exch.filter((n) => !checks.some((c) => c.account === n));
  $("#taxform-other").innerHTML = others.length && !report.settings.demo ? `<label class="dropzone inline" data-account="">
      <input type="file" class="tf-file" accept=".pdf,application/pdf,image/*" hidden>
      Got a 1099-DA from <select class="tf-other-acct">${others.map((n) => `<option>${esc(n)}</option>`).join("")}</select>? Drop it here or <span class="link">choose the file</span>.</label>` : "";
  if (tfFocus) {
    $(`.tf-form[data-id="${tfFocus}"] .tf-p`)?.focus();
    tfFocus = 0;
  }
}
async function addTaxForm(account, file) {
  if (!account) return toast("Pick the exchange first");
  const fd = new FormData();
  fd.append("account", account);
  fd.append("year", formsYear);
  fd.append("file", file);
  try {
    const t = await api("/api/taxforms", { method: "POST", body: fd });
    toast(`Added ${t.filename}. Enter its total.`);
    tfFocus = t.id;
    seasonOpen[formsYear] = { ...(seasonOpen[formsYear] || {}), forms: true };
    renderSeason();
  } catch (err) { toast(err.message); }
}
const dropAccount = (zone) => zone.closest(".tf-card")?.dataset.account || $(".tf-other-acct", zone)?.value;
$("#season-steps").addEventListener("dragover", (e) => {
  const z = e.target.closest(".dropzone");
  if (!z) return;
  e.preventDefault();
  z.classList.add("over");
});
$("#season-steps").addEventListener("dragleave", (e) => e.target.closest(".dropzone")?.classList.remove("over"));
$("#season-steps").addEventListener("drop", (e) => {
  const z = e.target.closest(".dropzone");
  if (!z) return;
  e.preventDefault();
  z.classList.remove("over");
  const file = e.dataTransfer.files?.[0];
  if (file) addTaxForm(dropAccount(z), file);
});
$("#season-steps").addEventListener("change", (e) => {
  if (e.target.classList.contains("tf-file") && e.target.files?.length) addTaxForm(dropAccount(e.target.closest(".dropzone")), e.target.files[0]);
});
$("#season-steps").addEventListener("click", async (e) => {
  const row = e.target.closest(".tf-form");
  if (!row) return;
  try {
    if (e.target.classList.contains("tf-save")) {
      await post(`/api/taxforms/${row.dataset.id}`, { Proceeds: $(".tf-p", row).value, Basis: $(".tf-b", row).value });
      toast("Totals saved");
    } else if (e.target.classList.contains("tf-del")) {
      await api(`/api/taxforms/${row.dataset.id}`, { method: "DELETE" });
    } else return;
    renderSeason();
  } catch (err) { toast(err.message); }
});
$("#season-steps").addEventListener("keydown", (e) => {
  if (e.key === "Enter" && e.target.closest(".tf-form input")) $(".tf-save", e.target.closest(".tf-form")).click();
});

// Step 3: the year's income by type, gifts, the 2025 split, coin choices.
function renderSeasonDetails(year) {
  const types = report.incomeTypes || {};
  const y = (report.years || []).find((x) => x.year === year);
  $("#income-types-table tbody").innerHTML = Object.entries(y?.incomeByType || {}).sort((a, b) => b[1] - a[1])
    .map(([t, v]) => `<tr><td>${year}</td><td>${esc(t)}</td><td class="num">${usd(v)}</td><td class="small">${esc(types[t] || "")}</td></tr>`).join("")
    || `<tr><td colspan="4" class="muted">No income in ${year}.</td></tr>`;
  const giftName = { gift: "Gift sent", donation: "Donation", lost: "Lost or stolen", "gift-in": "Gift received" };
  $("#gifts-table tbody").innerHTML = (report.gifts || []).filter((g) => Number(day(g.t).slice(0, 4)) === year).map((g) => `<tr>
    <td>${day(g.t)}</td><td>${esc(giftName[g.category] || g.category)}</td><td>${esc(g.recipient || "")}</td><td class="num">${btc(g.sats)}</td>
    <td class="num">${usd(g.value)}</td><td class="num">${usd(g.basis)}</td><td>${esc(g.term || "")}</td><td class="num">${g.deduction ? usd(g.deduction) : ""}</td>
    <td class="small">${g.flags.map((f) => `<div class="warn">${esc(f)}</div>`).join("")} <a href="${recordLink(g.key)}" data-flowpick="${esc(g.key)}">open</a></td></tr>`).join("")
    || `<tr><td colspan="9" class="muted">None in ${year}.</td></tr>`;
  $("#season-alloc").hidden = year < 2025 || !(report.allocation?.rows || []).length;
  const ids = (report.identifications || []).filter((i) => Number(day(i.createdAt).slice(0, 4)) === year);
  $("#season-choices").innerHTML = ids.length
    ? `<h3>Coins chosen before a sale</h3><p class="small">${ids.length} recorded in ${year}: ${ids.filter((i) => i.status === "applied").length} used for their sale. They're listed under <a href="#overview">Plan a sale or spend</a>.</p>` : "";
}

// Step 4: the year at a glance, then its Form 8949 lines.
function renderSeasonSummary(r, year) {
  const y = (report.years || []).find((x) => x.year === year) || { shortProceeds: 0, shortBasis: 0, longProceeds: 0, longBasis: 0, income: 0 };
  const tile = (k, v, sub) => `<div class="tile"><div class="k">${k}</div><div class="v">${v}</div>${sub ? `<div class="sub">${sub}</div>` : ""}</div>`;
  const carryIn = (y.carryInShort || 0) + (y.carryInLong || 0), carryOut = (y.carryOutShort || 0) + (y.carryOutLong || 0);
  $("#season-summary").innerHTML = `<div class="whatif-grid">
      ${tile("Short-term", signed(y.shortProceeds - y.shortBasis), `proceeds ${usd(y.shortProceeds)}, basis ${usd(y.shortBasis)}`)}
      ${tile("Long-term", signed(y.longProceeds - y.longBasis), `proceeds ${usd(y.longProceeds)}, basis ${usd(y.longBasis)}`)}
      ${tile("Income in BTC", usd(y.income), Object.keys(y.incomeByType || {}).join(", ") || "none")}
      ${tile("Form 1040 question", esc(y.digitalAsset?.answer || "No"), esc((y.digitalAsset?.reasons || []).join("; ")) || "no sales, income or fees in BTC")}
    </div>
    ${carryIn || y.lossDeducted || carryOut ? `<p class="small">Capital loss: ${usd(carryIn)} carried in, ${usd(y.lossDeducted || 0)} deducted from other income, ${usd(carryOut)} carried to ${year + 1}.</p>` : ""}
    ${(y.digitalAsset?.ask || []).map((a) => `<p class="small warn">${esc(a)}</p>`).join("")}`;
  $("#season-reviewed").textContent = r.reviewed ? "Reviewed ✓" : "Mark as reviewed";
  $("#season-reviewed").classList.toggle("primary", !r.reviewed);
  $("#season-reviewed").dataset.on = r.reviewed ? "" : "1";
  $("#season-reviewed").title = r.reviewed ? "Click to clear the mark" : "";
  $("#season-reviewed-note").textContent = r.reviewed ? `Reviewed ${day(r.reviewed)}. Changed something? Clear and recheck.` : "Open any line to fix it.";
}
$("#season-reviewed").addEventListener("click", async (e) => {
  try { await post(`/api/season/${formsYear}/reviewed`, { Reviewed: e.target.dataset.on === "1" }); renderSeason(); } catch (err) { toast(err.message); }
});

// Step 5: the package, with on-chain IDs only if asked.
function renderHandoff(r, year) {
  const href = () => `/api/package/${year}.zip${$("#season-onchain").checked ? "?onchain=1" : ""}`;
  $("#season-pkg").href = href();
  $("#season-pkg").textContent = `Download the ${year} package`;
  const open = r.checks.filter((c) => c.status !== "ok");
  $("#season-open").innerHTML = r.ready
    ? (open.length ? `<p>Ready to hand over. ${open.length} thing${open.length === 1 ? "" : "s"} worth a look are listed in the README for your tax professional.</p>` : `<p class="pos">Ready to hand over: every check passed.</p>`)
    : `<p class="warn">Not ready yet: the steps above marked "Needs fixing" will show in the README as open items. You can still download it to share a draft.</p>`;
  $("#season-downloaded").textContent = r.downloaded ? `Last downloaded on ${day(r.downloaded)}.` : "";
}
$("#season-onchain").addEventListener("change", () => {
  $("#season-pkg").href = `/api/package/${formsYear}.zip${$("#season-onchain").checked ? "?onchain=1" : ""}`;
});
$("#season-pkg").addEventListener("click", () => setTimeout(renderSeason, 1500));

$("#season-year").addEventListener("change", (e) => { formsYear = Number(e.target.value); renderSeason(); });
$("#season-steps").addEventListener("click", (e) => {
  const head = e.target.closest(".step-head");
  if (!head) return;
  const li = head.closest(".step");
  const open = !li.classList.contains("open");
  li.classList.toggle("open", open);
  head.setAttribute("aria-expanded", open);
  seasonOpen[formsYear] = { ...(seasonOpen[formsYear] || {}), [li.dataset.step]: open };
});

// "Go there" for a fix on this page: open its step and show the part.
$("#season-steps").addEventListener("click", (e) => {
  const a = e.target.closest("[data-goto-step]");
  if (!a) return;
  e.preventDefault();
  const li = $(`.step[data-step="${a.dataset.gotoStep}"]`);
  li.classList.add("open");
  $(".step-head", li).setAttribute("aria-expanded", true);
  seasonOpen[formsYear] = { ...(seasonOpen[formsYear] || {}), [li.dataset.step]: true };
  const target = $$(a.dataset.goto, li).find((el) => !el.closest("[hidden]")) || li;
  target.scrollIntoView({ block: "start" });
  target.classList.remove("flash");
  void target.offsetWidth; // restart the highlight
  target.classList.add("flash");
});
