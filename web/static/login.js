"use strict";
// The login page: set the password on the first visit, log in after that.

const $ = (s) => document.querySelector(s);

function showError(msg) {
  const e = $("#login-error");
  e.textContent = msg;
  e.hidden = !msg;
}

async function send(path, data) {
  const r = await fetch(path, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(data) });
  const body = r.headers.get("content-type")?.includes("json") ? await r.json() : {};
  if (!r.ok) throw new Error(body.error || r.statusText);
}

function busy(form, on) {
  for (const b of form.querySelectorAll("button")) b.disabled = on;
}

(async () => {
  let st = { passwordSet: true, loggedIn: false };
  try { st = await (await fetch("/api/auth/status")).json(); } catch {}
  if (st.loggedIn) { location.replace("/"); return; }
  const form = st.passwordSet ? $("#login-form") : $("#setup-form");
  form.hidden = false;
  form.querySelector("input").focus();
})();

$("#login-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const f = e.target;
  showError("");
  busy(f, true);
  try {
    await send("/api/auth/login", { password: f.password.value });
    location.replace("/");
  } catch (err) {
    showError(err.message.charAt(0).toUpperCase() + err.message.slice(1) + ".");
    f.password.select();
  } finally {
    busy(f, false);
  }
});

$("#setup-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const f = e.target;
  showError("");
  if (f.password.value !== f.password2.value) { showError("The two passwords don't match."); return; }
  busy(f, true);
  try {
    await send("/api/auth/setup", { password: f.password.value });
    location.replace("/");
  } catch (err) {
    if (/already set/.test(err.message)) { location.reload(); return; }
    showError(err.message.charAt(0).toUpperCase() + err.message.slice(1) + ".");
  } finally {
    busy(f, false);
  }
});
