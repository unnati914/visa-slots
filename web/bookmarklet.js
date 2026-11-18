// Visa slot agent as a bookmarklet: runs inside your own logged-in visa tab, nothing to install.
// agent.html glues extension/lib.js (window.VSA) in front of this file and swaps in the server URL.
// It never logs in or solves a CAPTCHA. It only does what the page does when you pick a consulate.
(function () {
  const SERVER = "__VSA_SERVER__";
  if (window.__vsaAgent) return window.__vsaAgent.show();

  const host = location.hostname;
  const SYSTEM = /(^|\.)usvisa-info\.com$/.test(host) ? "ais" : /(^|\.)usvisascheduling\.com$/.test(host) ? "cgi" : "";
  if (!SYSTEM) {
    alert("Visa slot agent\n\nOpen your visa account first (ais.usvisa-info.com or usvisascheduling.com), go to the appointment page, then click this bookmark there.");
    return;
  }

  const VISAS = ["B1/B2", "F1/M1", "J1", "H1B", "L1", "O1", "C1/D"];
  const KEY = "vsa-agent";
  const store = {
    get() { try { return JSON.parse(localStorage.getItem(KEY)) || {}; } catch { return {}; } },
    set(v) { try { localStorage.setItem(KEY, JSON.stringify(v)); } catch {} },
  };
  const saved = store.get();
  const s = { visa: "F1/M1", cat: "Regular", cc: "IN", alertBefore: "", minutes: 10, best: {}, ...saved };
  const save = () => store.set(s);

  // ---------- usvisascheduling.com: watch the calendar responses the page loads ----------
  const listeners = new Set();
  let lastCapture = null;
  if (SYSTEM === "cgi") {
    const emit = (url, data) => {
      const dates = VSA.extractDates(data);
      if (!VSA.looksLikeSlotUrl(url) && !dates.length) return;
      lastCapture = { dates, label: selectedLabel(), at: Date.now() };
      for (const fn of [...listeners]) fn(lastCapture);
    };
    const parse = (t) => { try { return JSON.parse(t); } catch { return null; } };
    const origFetch = window.fetch;
    window.fetch = async function (...args) {
      const res = await origFetch.apply(this, args);
      try {
        const url = typeof args[0] === "string" ? args[0] : args[0] && args[0].url;
        if (VSA.looksLikeSlotUrl(url)) res.clone().text().then((t) => { const j = parse(t); if (j) emit(url, j); });
      } catch {}
      return res;
    };
    const origOpen = XMLHttpRequest.prototype.open;
    XMLHttpRequest.prototype.open = function (m, url, ...rest) { this.__vsaUrl = url; return origOpen.call(this, m, url, ...rest); };
    const origSend = XMLHttpRequest.prototype.send;
    XMLHttpRequest.prototype.send = function (...args) {
      if (VSA.looksLikeSlotUrl(this.__vsaUrl)) {
        this.addEventListener("load", () => {
          try { const j = this.responseType === "json" ? this.response : parse(this.responseText); if (j) emit(this.__vsaUrl, j); } catch {}
        });
      }
      return origSend.apply(this, args);
    };
  }

  function postSelect() {
    const known = document.querySelector("#post_select, select[name*='post' i], select[id*='post' i], select[id*='consul' i]");
    if (known) return known;
    return [...document.querySelectorAll("select")].find((el) => {
      const opts = [...el.options].filter((o) => o.value);
      return opts.length >= 1 && opts.length <= 20 && opts.every((o) => !/\d/.test(o.textContent) && o.textContent.trim().length < 40);
    });
  }
  function selectedLabel() {
    const el = postSelect();
    return el && el.selectedIndex >= 0 ? el.options[el.selectedIndex].textContent.trim() : "";
  }
  function waitForCapture(ms) {
    return new Promise((resolve) => {
      const fn = (cap) => { listeners.delete(fn); clearTimeout(t); resolve(cap); };
      const t = setTimeout(() => { listeners.delete(fn); resolve(null); }, ms);
      listeners.add(fn);
    });
  }

  async function checkCGI() {
    if (/sign-?in|login|account\/log/i.test(location.pathname) || document.querySelector("input[type=password]")) {
      return { signedOut: true, error: "Signed out. Log in again, open the scheduling page, and click the bookmark." };
    }
    const sel = postSelect();
    const posts = [];
    if (sel) {
      const opts = [...sel.options].filter((o) => o.value);
      for (let i = 0; i < opts.length && running; i++) {
        if (i > 0) await VSA.sleep(VSA.jitter(3000, 6000));
        say(`Checking ${opts[i].textContent.trim()} (${i + 1} of ${opts.length})`);
        const wait = waitForCapture(12000);
        sel.value = opts[i].value;
        sel.dispatchEvent(new Event("input", { bubbles: true }));
        sel.dispatchEvent(new Event("change", { bubbles: true }));
        const cap = await wait;
        if (cap) posts.push({ city: opts[i].textContent.trim(), dates: cap.dates });
      }
      if (posts.length) return { cc: s.cc, posts };
    }
    if (lastCapture && Date.now() - lastCapture.at < 30 * 60 * 1000) return { cc: s.cc, posts: [{ city: lastCapture.label, dates: lastCapture.dates }] };
    return { error: "No calendar seen yet. Open the appointment page and pick a consulate once, so the agent can learn the page." };
  }

  // ---------- ais.usvisa-info.com: ask for each consulate's calendar, like the page does ----------
  async function checkAIS() {
    const info = VSA.parseAisUrl(location.href);
    if (!info) return { error: "Open the Reschedule Appointment page of your account, then click the bookmark there." };
    const sel = document.querySelector("#appointments_consulate_appointment_facility_id");
    if (!sel) return { cc: info.cc, error: "Consulate dropdown not found. Open the Reschedule Appointment page." };
    const fac = [...sel.options].filter((o) => o.value).map((o) => ({ id: o.value, city: o.textContent.trim() }));
    const posts = [];
    for (let i = 0; i < fac.length && running; i++) {
      if (i > 0) await VSA.sleep(VSA.jitter(3000, 7000));
      say(`Checking ${fac[i].city} (${i + 1} of ${fac.length})`);
      let res;
      try {
        res = await fetch(VSA.aisDaysUrl(info, fac[i].id), { credentials: "same-origin", headers: { Accept: "application/json", "X-Requested-With": "XMLHttpRequest" } });
      } catch (e) {
        return { cc: info.cc, posts, error: "Network error: " + e.message };
      }
      const type = res.headers.get("content-type") || "";
      if (res.status === 401 || res.redirected || !type.includes("json")) return { cc: info.cc, posts, signedOut: true, error: "Signed out. Log in again, reopen the appointment page, and click the bookmark." };
      if (res.status === 429) return { cc: info.cc, posts, error: "The site says too many requests. Use a longer interval." };
      posts.push({ city: fac[i].city, dates: VSA.extractDates(await res.json().catch(() => null)) });
    }
    return { cc: info.cc, posts };
  }

  // ---------- sending to the dashboard ----------
  async function send(report) {
    let r;
    try {
      r = await fetch(SERVER + "/api/report", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(report) });
    } catch (e) {
      // usually the visa site's security rules (CSP) stopping uploads to other sites
      throw new Error("This visa site blocks sending dates to the dashboard. Alerts in this tab still work. To share, use the Chrome extension instead.");
    }
    const b = await r.json().catch(() => ({}));
    if (!r.ok) throw new Error("Dashboard said: " + (b.error || "HTTP " + r.status));
    return b;
  }

  // ---------- alerts ----------
  let audio = null;
  function beep() {
    try {
      audio = audio || new (window.AudioContext || window.webkitAudioContext)();
      [0, 0.25, 0.5].forEach((d) => {
        const o = audio.createOscillator(), g = audio.createGain();
        o.frequency.value = 880; g.gain.value = 0.2; o.connect(g); g.connect(audio.destination);
        o.start(audio.currentTime + d); o.stop(audio.currentTime + d + 0.15);
      });
    } catch {}
  }
  const origTitle = document.title;
  let flash = null;
  function notify(title, body) {
    beep();
    try { if (window.Notification && Notification.permission === "granted") new Notification(title, { body, requireInteraction: true }).onclick = () => window.focus(); } catch {}
    clearInterval(flash);
    let on = false;
    flash = setInterval(() => { document.title = (on = !on) ? "🟢 " + title : origTitle; }, 900);
    window.addEventListener("focus", () => { clearInterval(flash); document.title = origTitle; }, { once: true });
  }
  function alertIfBetter(cc, posts) {
    for (const p of posts) {
      const earliest = p.dates[0];
      if (!earliest) continue;
      const key = `${cc}|${p.city}|${s.visa}|${s.cat}`;
      const prev = s.best[key];
      if ((prev && earliest < prev) || (s.alertBefore && earliest < s.alertBefore && earliest !== prev)) {
        notify(`Earlier slot: ${p.city}`, `${s.visa} ${s.cat}: ${earliest}${prev ? ` (was ${prev})` : ""}. Book it in this tab now.`);
      }
      s.best[key] = earliest;
    }
    save();
  }

  // ---------- loop ----------
  let running = false, busy = false, timer = null, nextAt = 0;
  async function runCheck() {
    if (busy) return;
    busy = true;
    try {
      const r = SYSTEM === "ais" ? await checkAIS() : await checkCGI();
      const cc = (r.cc || s.cc || "").toUpperCase();
      if (r.posts && r.posts.length) {
        renderResults(cc, r.posts);
        alertIfBetter(cc, r.posts);
        try {
          const body = await send({ agent: "vsa-bookmarklet/1", via: "extension", system: SYSTEM, cc, visa: s.visa, cat: s.cat, checkedAt: Date.now(), posts: r.posts });
          say(body.rejected && body.rejected.length ? `Sent. Dashboard didn't recognise: ${body.rejected.join(", ")}` : "Sent to dashboard ✓");
        } catch (e) { say(e.message, true); }
      }
      if (r.error) say(r.error, true);
      if (r.signedOut) { notify("Visa agent paused", "You were signed out. Log in again and click the bookmark."); stop(); }
    } finally {
      busy = false;
      if (running) schedule();
    }
  }
  function schedule() {
    clearTimeout(timer);
    const mins = Math.max(5, Number(s.minutes) || 10);
    const ms = mins * 60000 * VSA.jitter(0.9, 1.1);
    nextAt = Date.now() + ms;
    timer = setTimeout(runCheck, ms);
  }
  function start() {
    readForm();
    try { if (window.Notification && Notification.permission === "default") Notification.requestPermission(); } catch {}
    beep(); // unlocks sound while we still have your click
    running = true; renderState(); runCheck();
  }
  function stop() { running = false; clearTimeout(timer); nextAt = 0; renderState(); }

  // ---------- panel ----------
  const root = document.createElement("div");
  root.style.cssText = "position:fixed;z-index:2147483647;right:16px;bottom:16px;";
  const sh = root.attachShadow({ mode: "open" });
  const css = `
    :host{all:initial}
    .p{width:300px;max-width:calc(100vw - 32px);background:#fff;color:#16181d;border:1px solid #e3e1db;border-radius:16px;
       box-shadow:0 10px 40px rgba(0,0,0,.18);font:13px/1.4 -apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,sans-serif;overflow:hidden}
    .h{display:flex;align-items:center;gap:8px;padding:10px 12px;background:#f6f5f1;border-bottom:1px solid #e9e7e1;font-weight:650}
    .h span{flex:1}.dot{width:8px;height:8px;border-radius:50%;background:#a3a7ae}.dot.on{background:#0e7c5a;box-shadow:0 0 0 3px #e3f3ec}
    .x{border:0;background:none;font-size:16px;cursor:pointer;color:#8a8f98;padding:0 2px}
    .b{padding:10px 12px;display:grid;gap:8px}.b[hidden],[hidden]{display:none!important}
    label{display:grid;gap:3px;font-size:11px;color:#4a4f58;font-weight:600}
    .row{display:grid;grid-template-columns:1fr 1fr;gap:8px}
    select,input{font:inherit;font-size:13px;padding:6px 8px;border:1px solid #d9d6cf;border-radius:8px;background:#fff;color:#16181d;width:100%;box-sizing:border-box}
    button.btn{font:inherit;font-weight:650;border:0;border-radius:10px;padding:8px 10px;cursor:pointer;background:#0e7c5a;color:#fff}
    button.btn.ghost{background:#efede8;color:#16181d}
    .acts{display:grid;grid-template-columns:1fr 1fr;gap:8px}
    .msg{font-size:12px;color:#4a4f58;min-height:16px}.msg.err{color:#a86200}
    .res{display:grid;gap:3px;max-height:150px;overflow:auto}
    .r{display:flex;justify-content:space-between;gap:8px;font-size:12px;padding:4px 0;border-top:1px solid #f0eee9}
    .r b{font-weight:650}.r .open{color:#0e7c5a;font-weight:650}.r .none{color:#8a8f98}
    .note{font-size:11px;color:#8a8f98}
    @media (prefers-color-scheme:dark){
      .p{background:#18191c;color:#eeeff1;border-color:#26282c}.h{background:#0f1012;border-color:#26282c}
      select,input{background:#0f1012;color:#eeeff1;border-color:#33363b}label,.msg{color:#b7bbc2}
      button.btn.ghost{background:#26282c;color:#eeeff1}.r{border-color:#26282c}.r .open{color:#3ddc97}}`;
  try { const sheet = new CSSStyleSheet(); sheet.replaceSync(css); sh.adoptedStyleSheets = [sheet]; }
  catch { const st = document.createElement("style"); st.textContent = css; sh.appendChild(st); }

  const el = (tag, attrs = {}, kids = []) => {
    const n = document.createElement(tag);
    for (const [k, v] of Object.entries(attrs)) k === "text" ? (n.textContent = v) : n.setAttribute(k, v);
    for (const c of [].concat(kids)) n.append(c);
    return n;
  };
  const opt = (v, t = v) => el("option", { value: v, text: t });
  const ui = {};
  ui.dot = el("i", { class: "dot" });
  ui.min = el("button", { class: "x", title: "Minimise", text: "–" });
  ui.visa = el("select", {}, VISAS.map((v) => opt(v)));
  ui.cat = el("select", {}, [opt("Regular", "Interview"), opt("Dropbox", "Dropbox")]);
  ui.cc = el("input", { maxlength: "2", placeholder: "IN" });
  ui.before = el("input", { type: "date" });
  ui.minutes = el("select", {}, [5, 10, 15, 20, 30].map((m) => opt(String(m), `Every ${m} min`)));
  ui.go = el("button", { class: "btn" });
  ui.now = el("button", { class: "btn ghost", text: "Check now" });
  ui.msg = el("div", { class: "msg" });
  ui.next = el("div", { class: "note" });
  ui.res = el("div", { class: "res" });
  ui.body = el("div", { class: "b" }, [
    el("div", { class: "row" }, [el("label", {}, ["Visa", ui.visa]), el("label", {}, ["Appointment", ui.cat])]),
    el("div", { class: "row" }, [
      SYSTEM === "cgi" ? el("label", {}, ["Country code", ui.cc]) : el("label", {}, ["Check", ui.minutes]),
      el("label", {}, ["Alert if before", ui.before]),
    ]),
    SYSTEM === "cgi" ? el("label", {}, ["Check", ui.minutes]) : "",
    el("div", { class: "acts" }, [ui.go, ui.now]),
    ui.msg, ui.res, ui.next,
    el("div", { class: "note", text: "Keep this tab open. Reloading or leaving the page stops the agent: click the bookmark again." }),
  ]);
  sh.append(el("div", { class: "p" }, [el("div", { class: "h" }, [ui.dot, el("span", { text: "Visa slot agent" }), ui.min]), ui.body]));
  document.documentElement.append(root);

  ui.visa.value = s.visa; ui.cat.value = s.cat; ui.cc.value = s.cc; ui.before.value = s.alertBefore || ""; ui.minutes.value = String(s.minutes);
  function readForm() {
    s.visa = ui.visa.value; s.cat = ui.cat.value; s.cc = (ui.cc.value || "IN").trim().toUpperCase().slice(0, 2);
    s.alertBefore = ui.before.value; s.minutes = Number(ui.minutes.value) || 10; save();
  }
  [ui.visa, ui.cat, ui.cc, ui.before, ui.minutes].forEach((n) => n.addEventListener("change", () => { readForm(); if (running) schedule(); }));
  ui.go.onclick = () => (running ? stop() : start());
  ui.now.onclick = () => { readForm(); if (!busy) { clearTimeout(timer); runCheck(); } };
  ui.min.onclick = () => { ui.body.hidden = !ui.body.hidden; ui.min.textContent = ui.body.hidden ? "+" : "–"; };


  function say(text, err) { ui.msg.textContent = text; ui.msg.className = "msg" + (err ? " err" : ""); }
  function renderState() {
    ui.dot.className = "dot" + (running ? " on" : "");
    ui.go.textContent = running ? "Stop" : "Start";
    ui.go.className = "btn" + (running ? " ghost" : "");
    if (!running) say(ui.msg.textContent || "Pick your visa and press Start.");
  }
  function renderResults(cc, posts) {
    ui.res.replaceChildren(...posts.map((p) => el("div", { class: "r" }, [
      el("b", { text: p.city }),
      p.dates.length ? el("span", { class: "open", text: `${p.dates[0]} · ${p.dates.length} date${p.dates.length === 1 ? "" : "s"}` }) : el("span", { class: "none", text: "nothing open" }),
    ])));
  }
  setInterval(() => {
    ui.next.textContent = running && nextAt && !busy ? `Next check in ${Math.max(0, Math.round((nextAt - Date.now()) / 60000))} min · ${new Date().toLocaleTimeString()}` : "";
  }, 5000);
  renderState();

  window.__vsaAgent = { show() { root.style.display = ""; ui.body.hidden = false; }, stop, start };
})();
