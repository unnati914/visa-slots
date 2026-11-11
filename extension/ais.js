// Runs on your logged-in ais.usvisa-info.com appointment page. On each check it asks
// the site for the calendar of every consulate in the dropdown, the same request the
// page makes when you pick a consulate yourself, spaced out a few seconds apart.
(() => {
  const FACILITY_SELECT = "#appointments_consulate_appointment_facility_id";

  async function check() {
    const info = VSA.parseAisUrl(location.href);
    if (!info) return { system: "ais", error: "Open the Reschedule Appointment page of your account." };
    const select = document.querySelector(FACILITY_SELECT);
    if (!select) return { system: "ais", cc: info.cc, error: "Consulate dropdown not found. Open the appointment page (Reschedule Appointment)." };

    const facilities = [...select.options].filter((o) => o.value).map((o) => ({ id: o.value, city: o.textContent.trim() }));
    const posts = [];
    for (let i = 0; i < facilities.length; i++) {
      if (i > 0) await VSA.sleep(VSA.jitter(3000, 7000));
      const f = facilities[i];
      let res;
      try {
        res = await fetch(VSA.aisDaysUrl(info, f.id), {
          credentials: "same-origin",
          headers: { Accept: "application/json", "X-Requested-With": "XMLHttpRequest" },
        });
      } catch (e) {
        return { system: "ais", cc: info.cc, posts, error: "Network error: " + e.message };
      }
      const type = res.headers.get("content-type") || "";
      if (res.status === 401 || res.redirected || !type.includes("json")) {
        return { system: "ais", cc: info.cc, posts, signedOut: true, error: "Signed out. Log in again and reopen the appointment page." };
      }
      if (res.status === 429) {
        return { system: "ais", cc: info.cc, posts, error: "The site says too many requests. Increase the check interval." };
      }
      const json = await res.json().catch(() => null);
      posts.push({ city: f.city, dates: VSA.extractDates(json) });
    }
    return { system: "ais", cc: info.cc, posts };
  }

  chrome.runtime.onMessage.addListener((msg, _sender, reply) => {
    if (msg?.type !== "check") return;
    check().then(reply, (e) => reply({ system: "ais", error: String(e) }));
    return true; // async reply
  });
})();
