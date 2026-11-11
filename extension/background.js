// The agent loop. Every N minutes: find your open visa tabs, ask them for slots,
// send the slots to the dashboard server, and alert you when an earlier date opens.
const AIS_TABS = "https://ais.usvisa-info.com/*/niv/schedule/*/appointment*";
const CGI_TABS = "https://www.usvisascheduling.com/*";
const DEFAULTS = { enabled: true, server: "http://localhost:8080", country: "IN", city: "", visa: "B1/B2", cat: "Regular", minutes: 10, alertBefore: "2026-12-30" };

const settings = async () => ({ ...DEFAULTS, ...(await chrome.storage.local.get("settings")).settings });

async function schedule() {
  const s = await settings();
  await chrome.alarms.clear("check");
  if (s.enabled) chrome.alarms.create("check", { delayInMinutes: 0.2, periodInMinutes: Math.max(5, Number(s.minutes) || 10) });
}

chrome.runtime.onInstalled.addListener(schedule);
chrome.runtime.onStartup.addListener(schedule);
chrome.alarms.onAlarm.addListener((a) => a.name === "check" && runCheck());
chrome.runtime.onMessage.addListener((msg, _s, reply) => {
  if (msg?.type === "check-now") runCheck().then(reply);
  else if (msg?.type === "settings-changed") schedule().then(() => reply(true));
  else return;
  return true;
});

let running = false;
async function runCheck() {
  if (running) return { busy: true };
  running = true;
  try {
    const s = await settings();
    const tabs = [...(await chrome.tabs.query({ url: AIS_TABS })), ...(await chrome.tabs.query({ url: CGI_TABS }))];
    const status = { at: Date.now(), results: [], problems: [] };
    if (!tabs.length) {
      status.problems.push("No visa tab open. Log in to your visa account and keep the appointment page open in a tab.");
    }
    for (const tab of tabs) {
      chrome.tabs.update(tab.id, { autoDiscardable: false }).catch(() => {});
      let r;
      try {
        r = await chrome.tabs.sendMessage(tab.id, { type: "check", city: s.city });
      } catch {
        status.problems.push("Reload the visa tab once so the agent can attach to it.");
        continue;
      }
      if (!r) continue;
      if (r.error) status.problems.push(r.error);
      if (r.signedOut) notify("signed-out", "Visa agent paused", "You were signed out. Log in again so the agent can keep checking.");
      if (!r.posts?.length) continue;

      const cc = (r.cc || s.country || "").toUpperCase();
      const report = { agent: "vsa-ext/1.0", system: r.system, cc, visa: s.visa, cat: s.cat, checkedAt: Date.now(), posts: r.posts };
      try {
        const res = await fetch(s.server.replace(/\/$/, "") + "/api/report", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify(report),
        });
        const body = await res.json().catch(() => ({}));
        if (!res.ok) status.problems.push(`Server rejected report: ${body.error || res.status}`);
        if (body.rejected?.length) status.problems.push(`Server did not recognise: ${body.rejected.join(", ")}`);
      } catch (e) {
        status.problems.push(`Cannot reach server ${s.server}: ${e.message}`);
      }
      for (const p of r.posts) status.results.push({ cc, city: p.city, earliest: p.dates[0] || "", count: p.dates.length });
      await alertIfBetter(s, cc, r.posts);
    }
    await chrome.storage.local.set({ status });
    const open = status.results.filter((x) => x.count).length;
    chrome.action.setBadgeText({ text: status.problems.length && !status.results.length ? "!" : open ? String(open) : "" });
    chrome.action.setBadgeBackgroundColor({ color: open ? "#1a7f4b" : "#b45309" });
    return status;
  } finally {
    running = false;
  }
}

// Alert when a consulate's earliest date moves earlier than last time, or beats your target date.
async function alertIfBetter(s, cc, posts) {
  const { best = {} } = await chrome.storage.local.get("best");
  for (const p of posts) {
    const key = `${cc}|${p.city}|${s.visa}|${s.cat}`;
    const earliest = p.dates[0];
    if (!earliest) continue;
    const prev = best[key];
    const beatsTarget = s.alertBefore && earliest < s.alertBefore;
    if ((prev && earliest < prev) || (beatsTarget && earliest !== prev)) {
      notify(key, `Earlier slot: ${p.city}`, `${s.visa} ${s.cat}: ${earliest}${prev ? ` (was ${prev})` : ""}. Book it on your visa tab now.`);
    }
    best[key] = earliest;
  }
  await chrome.storage.local.set({ best });
}

function notify(id, title, message) {
  chrome.notifications.create(id + "|" + Date.now(), { type: "basic", iconUrl: "icon.png", title, message, priority: 2, requireInteraction: true });
}

// Clicking an alert jumps to your visa tab so you can book.
chrome.notifications.onClicked.addListener(async () => {
  const [tab] = [...(await chrome.tabs.query({ url: AIS_TABS })), ...(await chrome.tabs.query({ url: CGI_TABS }))];
  if (tab) {
    chrome.tabs.update(tab.id, { active: true });
    chrome.windows.update(tab.windowId, { focused: true });
  }
});
