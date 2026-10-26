"use strict";
// The landing page: the phone cycles through the app's screens, the install
// tabs switch, code copies, and sections fade in as they scroll into view.
(() => {
  const $$ = (s, el = document) => [...el.querySelectorAll(s)];
  const still = matchMedia("(prefers-reduced-motion: reduce)").matches;

  // Top bar gets a background once the page scrolls.
  const bar = document.querySelector(".bar");
  const onScroll = () => bar.classList.toggle("scrolled", scrollY > 10);
  addEventListener("scroll", onScroll, { passive: true });
  onScroll();

  // Phone screens: one every few seconds, or the one you pick.
  const shots = $$(".shot"), tabs = $$(".screen-tabs button");
  let at = 0, timer = null;
  const show = (i) => {
    at = (i + shots.length) % shots.length;
    shots.forEach((s, j) => s.classList.toggle("on", j === at));
    tabs.forEach((t, j) => t.setAttribute("aria-selected", String(j === at)));
  };
  const play = () => { if (!still && !timer) timer = setInterval(() => show(at + 1), 3400); };
  const pause = () => { clearInterval(timer); timer = null; };
  tabs.forEach((t, i) => t.addEventListener("click", () => { show(i); pause(); play(); }));
  const stage = document.querySelector(".stage");
  stage.addEventListener("pointerenter", pause);
  stage.addEventListener("pointerleave", play);
  document.addEventListener("visibilitychange", () => (document.hidden ? pause() : play()));
  play();

  // A slight tilt toward the pointer, on screens with a mouse.
  if (!still && matchMedia("(hover: hover) and (min-width: 961px)").matches) {
    const phone = document.querySelector(".phone"), browser = document.querySelector(".browser");
    stage.addEventListener("pointermove", (e) => {
      const r = stage.getBoundingClientRect();
      const x = (e.clientX - r.left) / r.width - 0.5, y = (e.clientY - r.top) / r.height - 0.5;
      phone.style.transform = `rotateY(${x * 10}deg) rotateX(${-y * 8}deg)`;
      browser.style.transform = `rotateY(${18 + x * 6}deg) rotateX(${4 - y * 4}deg) translateZ(-120px)`;
    });
    stage.addEventListener("pointerleave", () => { phone.style.transform = ""; browser.style.transform = ""; });
  }

  // Install tabs.
  const itabs = $$(".tabs button");
  itabs.forEach((t) => t.addEventListener("click", () => {
    itabs.forEach((o) => {
      const on = o === t;
      o.setAttribute("aria-selected", String(on));
      document.getElementById(o.getAttribute("aria-controls")).hidden = !on;
    });
  }));

  // Copy buttons.
  $$(".copy").forEach((b) => b.addEventListener("click", async () => {
    const text = b.previousElementSibling.textContent;
    try {
      await navigator.clipboard.writeText(text);
      b.textContent = "Copied";
    } catch {
      b.textContent = "Select it";
    }
    setTimeout(() => (b.textContent = "Copy"), 1600);
  }));

  // The worked example's lines: each node's data-to names where its coins go
  // ("cold 0.10, spend 0.01"). With the stages side by side the lines run
  // from right edge to left edge; stacked on a phone, from bottom to top.
  // Redrawn on resize.
  const flow = document.querySelector(".flow");
  if (flow) {
    const svg = flow.querySelector(".edges"), NS = "http://www.w3.org/2000/svg";
    const el = (name, attrs, parent) => {
      const n = document.createElementNS(NS, name);
      for (const k in attrs) n.setAttribute(k, attrs[k]);
      return parent.appendChild(n);
    };
    const cols = $$(".col", flow);
    const draw = () => {
      svg.replaceChildren();
      const side = getComputedStyle(flow).gridTemplateColumns.split(" ").length > 1;
      // Layout positions, not on-screen ones: the nodes slide in with a transform.
      const at = (n) => {
        let l = 0, t = 0;
        for (let e = n; e && e !== flow; e = e.offsetParent) { l += e.offsetLeft; t += e.offsetTop; }
        const w = n.offsetWidth, h = n.offsetHeight;
        return { l, r: l + w, t, b: t + h, x: l + w / 2, y: t + h / 2 };
      };
      $$(".node[data-to]", flow).forEach((from) => {
        const a = at(from), col = cols.indexOf(from.closest(".col"));
        from.dataset.to.split(",").forEach((spec, i) => {
          const [id, amount] = spec.trim().split(/\s+/);
          const b = at(document.getElementById("n-" + id));
          const [x1, y1, x2, y2] = side ? [a.r, a.y, b.l, b.y] : [a.x, a.b, b.x, b.t];
          const d = side
            ? `M${x1},${y1} C${(x1 + x2) / 2},${y1} ${(x1 + x2) / 2},${y2} ${x2},${y2}`
            : `M${x1},${y1} C${x1},${(y1 + y2) / 2} ${x2},${(y1 + y2) / 2} ${x2},${y2}`;
          const g = el("g", { class: "edge" }, svg);
          g.style.transitionDelay = `${col * 0.5 + 0.3}s`;
          const line = el("path", { class: "line", d, pathLength: 1 }, g);
          line.style.transitionDelay = g.style.transitionDelay;
          el("path", { class: "pulse", d, pathLength: 1 }, g).style.animationDelay = `${2.2 + col * 0.45 + i * 0.3}s`;
          const label = el("g", {}, g);
          const text = el("text", { x: (x1 + x2) / 2, y: (y1 + y2) / 2, "text-anchor": "middle", "dominant-baseline": "central" }, label);
          text.textContent = amount;
          const t = text.getBBox();
          label.insertBefore(el("rect", { x: t.x - 7, y: t.y - 3, width: t.width + 14, height: t.height + 6, rx: (t.height + 6) / 2 }, label), text);
        });
      });
    };
    new ResizeObserver(draw).observe(flow);
    document.fonts?.ready.then(draw);
  }

  // Sections fade in once.
  if (still || !("IntersectionObserver" in window)) {
    $$(".reveal").forEach((s) => s.classList.add("in"));
  } else {
    const io = new IntersectionObserver((entries) => {
      for (const e of entries) if (e.isIntersecting) { e.target.classList.add("in"); io.unobserve(e.target); }
    }, { threshold: 0.12 });
    $$(".reveal").forEach((s) => io.observe(s));
  }
})();
