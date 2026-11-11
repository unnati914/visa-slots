// Runs on your logged-in usvisascheduling.com scheduling page. On each check it picks
// each consulate in the dropdown in turn, like you would by hand, and records the
// dates the site's own calendar loads (captured by cgi-hook.js).
(() => {
  let lastCapture = null; // {dates, label, at}
  const listeners = new Set();

  window.addEventListener("message", (e) => {
    if (e.source !== window || !e.data?.__vsa) return;
    const cap = { dates: e.data.dates, label: selectedLabel(), at: Date.now() };
    lastCapture = cap;
    for (const fn of listeners) fn(cap);
  });

  // The consulate dropdown. Prefer known ids, otherwise the select whose options look
  // like city names (short text, no digits).
  function postSelect() {
    const known = document.querySelector("#post_select, select[name*='post' i], select[id*='post' i], select[id*='consul' i]");
    if (known) return known;
    return [...document.querySelectorAll("select")].find((s) => {
      const opts = [...s.options].filter((o) => o.value);
      return opts.length >= 1 && opts.length <= 20 && opts.every((o) => !/\d/.test(o.textContent) && o.textContent.trim().length < 40);
    });
  }
  function selectedLabel() {
    const s = postSelect();
    return s && s.selectedIndex >= 0 ? s.options[s.selectedIndex].textContent.trim() : "";
  }

  function waitForCapture(ms) {
    return new Promise((resolve) => {
      const fn = (cap) => {
        listeners.delete(fn);
        clearTimeout(t);
        resolve(cap);
      };
      const t = setTimeout(() => {
        listeners.delete(fn);
        resolve(null);
      }, ms);
      listeners.add(fn);
    });
  }

  function signedOut() {
    return /sign-?in|login|account\/log/i.test(location.pathname) || !!document.querySelector("input[type=password]");
  }

  async function check(fallbackCity) {
    if (signedOut()) return { system: "cgi", signedOut: true, error: "Signed out. Log in again and open the scheduling page." };
    const sel = postSelect();
    const posts = [];
    if (sel) {
      const opts = [...sel.options].filter((o) => o.value);
      for (let i = 0; i < opts.length; i++) {
        if (i > 0) await VSA.sleep(VSA.jitter(3000, 6000));
        const o = opts[i];
        const wait = waitForCapture(12000);
        sel.value = o.value;
        sel.dispatchEvent(new Event("input", { bubbles: true }));
        sel.dispatchEvent(new Event("change", { bubbles: true }));
        const cap = await wait;
        if (cap) posts.push({ city: o.textContent.trim(), dates: cap.dates });
      }
      if (posts.length) return { system: "cgi", posts };
    }
    // Could not drive the dropdown: report whatever the calendar showed most recently.
    if (lastCapture && Date.now() - lastCapture.at < 30 * 60 * 1000) {
      return { system: "cgi", posts: [{ city: lastCapture.label || fallbackCity || "", dates: lastCapture.dates }], passive: true };
    }
    return { system: "cgi", error: "No calendar seen yet. Open the appointment calendar once (pick a consulate) so the agent can learn the page." };
  }

  chrome.runtime.onMessage.addListener((msg, _sender, reply) => {
    if (msg?.type !== "check") return;
    check(msg.city).then(reply, (e) => reply({ system: "cgi", error: String(e) }));
    return true;
  });
})();
