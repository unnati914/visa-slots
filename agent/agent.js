// Visa slot agent: no extension needed.
//
// Opens its own Chrome window. You log in to usvisascheduling.com once (you solve the
// CAPTCHA, it never does). From then on, every few minutes it switches the OFC Post
// dropdown through every consulate, reads the calendar the site loads, sends the
// results to the dashboard, and pushes an alert to your phone when a slot opens.
//
// Run: ./start.sh   (or: node agent.js)
const fs = require("fs");
const path = require("path");
const crypto = require("crypto");
const { chromium } = require("playwright-core");
const VSA = require("../extension/lib.js");

const DIR = process.env.VSA_DIR || __dirname; // VSA_DIR lets tests use a separate config
const CONFIG = path.join(DIR, "config.json");
const STATE = path.join(DIR, "state.json");
const PROFILE = path.join(DIR, ".chrome-profile"); // keeps you logged in between restarts

const DEFAULTS = {
  country: "IN",
  visa: "F1/M1",
  cat: "Regular",
  alertBefore: "2026-12-30",
  minutes: 10,
  server: "http://localhost:8080",
  ntfyTopic: "",
  telegramToken: "",
  telegramChatId: "",
  startUrl: "https://www.usvisascheduling.com/en-US/",
};

const readJSON = (f, d) => {
  try {
    return { ...d, ...JSON.parse(fs.readFileSync(f, "utf8")) };
  } catch {
    return { ...d };
  }
};
const writeJSON = (f, v) => fs.writeFileSync(f, JSON.stringify(v, null, 2));
// VSA_LOG_MS=1 adds an exact epoch-ms stamp (used by the demo recorder for timing).
const log = (...a) => console.log(...(process.env.VSA_LOG_MS ? [`@${Date.now()}`] : []), new Date().toLocaleTimeString(), ...a);

const cfg = readJSON(CONFIG, DEFAULTS);
if (!cfg.ntfyTopic) cfg.ntfyTopic = "visa-slots-" + crypto.randomBytes(6).toString("hex");
cfg.minutes = Math.max(5, Number(cfg.minutes) || 10);
writeJSON(CONFIG, cfg);
const state = readJSON(STATE, { calendarUrl: "", best: {}, lastAlert: {} });
const saveState = () => writeJSON(STATE, state);

// ---------- phone alerts ----------
async function alert(title, message, { key, everyMin = 0, click } = {}) {
  if (key && everyMin) {
    const last = state.lastAlert[key] || 0;
    if (Date.now() - last < everyMin * 60_000) return;
    state.lastAlert[key] = Date.now();
    saveState();
  }
  log(`ALERT: ${title}: ${message}`);
  const jobs = [];
  if (cfg.ntfyTopic && cfg.ntfyTopic !== "off") {
    jobs.push(
      fetch(`https://ntfy.sh/${cfg.ntfyTopic}`, {
        method: "POST",
        body: message,
        headers: { Title: title, Priority: "urgent", Tags: "calendar", ...(click ? { Click: click } : {}) },
      })
    );
  }
  if (cfg.telegramToken && cfg.telegramChatId) {
    jobs.push(
      fetch(`https://api.telegram.org/bot${cfg.telegramToken}/sendMessage`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ chat_id: cfg.telegramChatId, text: `${title}\n${message}` }),
      })
    );
  }
  for (const r of await Promise.allSettled(jobs)) if (r.status === "rejected") log("alert failed:", r.reason?.message);
}

// ---------- page helpers ----------
// Finds the consulate dropdown, tags it, and returns its options.
async function postOptions(page) {
  return page.evaluate(() => {
    const pick =
      document.querySelector("#post_select, select[name*='post' i], select[id*='post' i], select[id*='consul' i]") ||
      [...document.querySelectorAll("select")].find((s) => {
        const o = [...s.options].filter((x) => x.value);
        return o.length >= 1 && o.length <= 20 && o.every((x) => !/\d/.test(x.textContent) && x.textContent.trim().length < 40);
      });
    if (!pick) return null;
    pick.setAttribute("data-vsa", "post");
    return [...pick.options].filter((o) => o.value).map((o) => ({ value: o.value, label: o.textContent.trim() }));
  });
}

async function signedOut(page) {
  if (/sign-?in|login|account\/log/i.test(new URL(page.url()).pathname)) return true;
  return page.evaluate(() => !!document.querySelector("input[type=password]")).catch(() => false);
}

// Collects calendar responses the site loads (we only read them, never send our own).
const waiters = new Set();
function watchResponses(context) {
  context.on("response", async (res) => {
    const url = res.url();
    if (!VSA.looksLikeSlotUrl(url)) return;
    let json = null;
    try {
      json = JSON.parse(await res.text());
    } catch {
      return;
    }
    const dates = VSA.extractDates(json);
    for (const w of waiters) w({ url, dates });
  });
}
function nextCapture(ms) {
  return new Promise((resolve) => {
    const fn = (cap) => {
      waiters.delete(fn);
      clearTimeout(t);
      resolve(cap);
    };
    const t = setTimeout(() => {
      waiters.delete(fn);
      resolve(null);
    }, ms);
    waiters.add(fn);
  });
}

// Minimizes or restores the agent's own Chrome window, so it stays out of your way
// and only pops up when it needs you (logged out, calendar page lost).
async function setWindow(page, windowState) {
  if (process.env.VSA_HEADLESS || cfg.minimize === false) return;
  try {
    const s = await page.context().newCDPSession(page);
    const { windowId } = await s.send("Browser.getWindowForTarget");
    await s.send("Browser.setWindowBounds", { windowId, bounds: { windowState } });
    await s.detach();
  } catch {}
}

// ---------- one check of every consulate ----------
// Returns false when it needs you (logged out or not on the calendar page).
async function check(page) {
  if (await signedOut(page)) {
    await alert("Visa agent: logged out", "Log in again in the agent's Chrome window, then open the calendar page.", { key: "out", everyMin: 30 });
    return false;
  }
  let opts = await postOptions(page);
  if (!opts && state.calendarUrl) {
    await page.goto(state.calendarUrl, { waitUntil: "domcontentloaded" }).catch(() => {});
    await page.waitForTimeout(6000);
    if (await signedOut(page)) return check(page);
    opts = await postOptions(page);
  }
  if (!opts) {
    await alert("Visa agent: needs the calendar page", "Open Schedule/Reschedule Appointment in the agent's Chrome window.", { key: "nocal", everyMin: 30 });
    return false;
  }

  for (let i = 0; i < opts.length; i++) {
    if (i > 0) await VSA.sleep(VSA.jitter(3000, 6000));
    const o = opts[i];
    const wait = nextCapture(12000);
    await page.selectOption("select[data-vsa=post]", o.value).catch(() => {});
    const cap = await wait;
    if (!cap) {
      log(`${o.label}: no calendar response (skipped)`);
      continue;
    }
    log(`${o.label}: ${cap.dates.length ? cap.dates.length + " dates, earliest " + cap.dates[0] : "no slots"}`);
    // Send each consulate the moment it is read, so a slot is not held back by the rest of the round.
    const p = { city: o.label, dates: cap.dates };
    await Promise.all([report(p), alertFor(p)]);
  }
  saveState();
  return true;
}

async function report(p) {
  try {
    const res = await fetch(cfg.server.replace(/\/$/, "") + "/api/report", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ agent: "vsa-node/1.0", system: "cgi", cc: cfg.country, visa: cfg.visa, cat: cfg.cat, checkedAt: Date.now(), posts: [p] }),
    });
    const body = await res.json().catch(() => ({}));
    if (!res.ok) log("dashboard rejected report:", body.error || res.status);
    else if (body.rejected?.length) log("dashboard did not recognise:", body.rejected.join(", "));
    else if (p.dates.length) log(`${p.city}: sent to dashboard`);
  } catch {
    log(`dashboard not reachable at ${cfg.server} (alerts still work)`);
  }
}

// Phone alert when a consulate's earliest date is earlier than before, or before your target date.
async function alertFor(p) {
  const key = `${p.city}|${cfg.visa}|${cfg.cat}`;
  const earliest = p.dates[0];
  const prev = state.best[key];
  if (earliest && ((prev && earliest < prev) || (cfg.alertBefore && earliest < cfg.alertBefore && earliest !== prev))) {
    await alert(`Slot open: ${p.city}`, `Earliest ${earliest}${prev ? ` (was ${prev})` : ""}, ${p.dates.length} date(s). Book now in the agent's Chrome window.`, {
      click: cfg.startUrl,
    });
  }
  state.best[key] = earliest || "";
}

// ---------- main ----------
(async () => {
  console.log(`
Visa slot agent
  visa ${cfg.visa} ${cfg.cat}, country ${cfg.country}, alert before ${cfg.alertBefore}, every ${cfg.minutes} min
  Phone alerts: install the free "ntfy" app, tap +, subscribe to topic:  ${cfg.ntfyTopic}
  (settings live in agent/config.json)
`);
  const context = await chromium.launchPersistentContext(PROFILE, {
    channel: "chrome",
    headless: !!process.env.VSA_HEADLESS,
    viewport: null,
    // keep checking at full speed while the window is minimized or behind other windows
    args: ["--disable-background-timer-throttling", "--disable-backgrounding-occluded-windows", "--disable-renderer-backgrounding"],
  });
  watchResponses(context);
  const page = context.pages()[0] || (await context.newPage());
  context.on("close", () => {
    log("Chrome window closed. Agent stopped.");
    process.exit(0);
  });
  await page.goto(state.calendarUrl || cfg.startUrl, { waitUntil: "domcontentloaded" }).catch(() => {});
  await alert("Visa agent started", `Watching ${cfg.visa} slots in ${cfg.country}. You will get alerts here.`);

  // Wait until you are logged in and on the calendar page.
  if (!(await postOptions(page).catch(() => null))) {
    log("Log in in the Chrome window that just opened, then open Schedule/Reschedule Appointment so the OFC Post dropdown shows. Waiting...");
    while (!(await postOptions(page).catch(() => null))) await page.waitForTimeout(5000);
  }
  state.calendarUrl = page.url();
  saveState();
  log("Calendar page found. Checking now, then every", cfg.minutes, "minutes. The window is minimized; it comes back only if it needs you.");
  await setWindow(page, "minimized");

  let needsYou = false;
  for (;;) {
    try {
      const ok = await check(page);
      if (!ok && !needsYou) await setWindow(page, "normal");
      if (ok && needsYou) await setWindow(page, "minimized");
      needsYou = !ok;
    } catch (e) {
      log("check failed:", e.message);
    }
    const wait = process.env.VSA_INTERVAL_SEC ? Number(process.env.VSA_INTERVAL_SEC) * 1000 : cfg.minutes * 60_000 * VSA.jitter(0.9, 1.2);
    log(`next check in ${wait >= 60000 ? Math.round(wait / 60000) + " min" : Math.round(wait / 1000) + " s"}`);
    await page.waitForTimeout(wait).catch(() => process.exit(0));
  }
})();
