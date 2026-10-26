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
