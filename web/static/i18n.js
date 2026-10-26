"use strict";
// The page in the chosen language (Settings → Display), else the browser's.
// English is the source text: each language is a table from English to its
// own words (lang/<code>.js), plus patterns for text with numbers or names in
// it. Text is translated as it appears, so the code that draws the pages
// stays in English. Runs before the page paints, like theme-init.js.
(() => {
  const LANGS = { en: "English", es: "Español", fr: "Français", de: "Deutsch", pl: "Polski", ru: "Русский" };
  const fromBrowser = () => {
    for (const l of navigator.languages || [navigator.language || "en"]) {
      const c = String(l).slice(0, 2).toLowerCase();
      if (LANGS[c]) return c;
    }
    return "en";
  };
  let chosen = "";
  try { chosen = localStorage.getItem("lang") || ""; } catch {}
  if (!LANGS[chosen]) chosen = "";
  const system = fromBrowser();
  const lang = chosen || system;
  document.documentElement.lang = lang;

  const I = (window.POB_I18N = { LANGS, lang, chosen, system, dict: {}, patterns: [], misses: new Set() });

  // tr(): the translation of one piece of text, or null to leave it.
  // Text over several lines (chart tooltips) goes line by line. Results are
  // remembered: charts redraw the same text on every zoom.
  const memo = new Map();
  const tr = (s) => {
    let v = memo.get(s);
    if (v === undefined) {
      if (memo.size > 20000) memo.clear();
      v = translate(s);
      memo.set(s, v);
    }
    return v;
  };
  const translate = (s) => {
    if (s.includes("\n")) {
      const lines = s.split("\n").map((l) => tr(l) ?? l);
      const v = lines.join("\n");
      return v === s ? null : v;
    }
    const k = s.trim();
    if (!k || !/\p{L}/u.test(k)) return null;
    let v = I.dict[k];
    if (v == null) {
      for (const [re, rep] of I.patterns) {
        if (re.test(k)) { v = k.replace(re, rep); break; }
      }
    }
    // A list joined with "; ", or several sentences, goes part by part.
    for (const sep of ["; ", /(?<=[.?!:]) (?=[A-Z0-9])/]) {
      if (v != null) break;
      const parts = k.split(sep);
      if (parts.length < 2) continue;
      const joined = parts.map((p) => tr(p) ?? p).join(typeof sep === "string" ? sep : " ");
      if (joined !== k) v = joined;
    }
    if (v == null || v === k) {
      if (I.misses.size < 2000) I.misses.add(k);
      return null;
    }
    return s.replace(k, v);
  };
  // t(): for text that never reaches the page, such as confirm() prompts.
  window.t = (s) => (lang === "en" ? s : tr(s) ?? s);
  if (lang === "en") return;

  // Not translated: the assistant's answers and your questions to it, and
  // anything marked as data (addresses, transaction ids, names you typed).
  // [data-i18n-self]: only the element itself; what's drawn inside it (the
  // Flow chart, redrawn on every zoom) calls t() as it's drawn.
  const SKIP = "script,style,textarea,code,pre,.mono,.ai-text,.ai-bubble,[data-no-i18n],[data-i18n-self] *";
  const ATTRS = ["title", "placeholder", "aria-label", "alt"];
  const set = new WeakMap(); // node → text we put there, so we don't redo it

  const text = (n) => {
    if (set.get(n) === n.data) return;
    const v = tr(n.data);
    if (v != null) { set.set(n, v); n.data = v; }
  };
  const attrs = (el) => {
    for (const a of ATTRS) {
      const cur = el.getAttribute(a);
      if (!cur || set.get(el)?.[a] === cur) continue;
      const v = tr(cur);
      if (v != null) {
        const mine = set.get(el) || {};
        mine[a] = v;
        set.set(el, mine);
        el.setAttribute(a, v);
      }
    }
  };
  const skipped = (el) => el.closest(SKIP) != null;
  const walk = (root) => {
    if (root.nodeType === 3) { if (root.parentElement && !skipped(root.parentElement)) text(root); return; }
    if (root.nodeType !== 1 || skipped(root)) return;
    attrs(root);
    const w = document.createTreeWalker(root, NodeFilter.SHOW_ELEMENT | NodeFilter.SHOW_TEXT, {
      acceptNode: (n) => (n.nodeType === 1 && n.matches(SKIP) ? NodeFilter.FILTER_REJECT : NodeFilter.FILTER_ACCEPT),
    });
    while (w.nextNode()) {
      const n = w.currentNode;
      if (n.nodeType === 3) text(n); else attrs(n);
    }
  };

  new MutationObserver((records) => {
    for (const r of records) {
      if (r.type === "childList") r.addedNodes.forEach(walk);
      else if (r.type === "characterData") walk(r.target);
      else if (r.type === "attributes" && !skipped(r.target)) attrs(r.target);
    }
  }).observe(document.documentElement, { childList: true, subtree: true, characterData: true, attributes: true, attributeFilter: ATTRS });

  // The <title> is parsed before this runs.
  document.addEventListener("DOMContentLoaded", () => walk(document.head));

  // The table for this language; it runs before the rest of the page loads.
  document.write(`<script src="lang/${lang}.js"><\/script>`);
})();
