// Shared helpers. Loaded in content scripts (both worlds) and in Node tests.
(function (g) {
  const pad = (n) => String(n).padStart(2, "0");
  const iso = (y, m, d) => (m >= 1 && m <= 12 && d >= 1 && d <= 31 ? `${y}-${pad(m)}-${pad(d)}` : null);
  // Accepts every date style these sites use: 2026-12-18, 2026-12-18T00:00:00,
  // 12/18/2026 (US), 2026/12/18, and .NET's /Date(1797552000000)/.
  function toISO(v) {
    let m;
    if ((m = /^(\d{4})[-/](\d{1,2})[-/](\d{1,2})/.exec(v))) return iso(m[1], +m[2], +m[3]);
    if ((m = /^(\d{1,2})\/(\d{1,2})\/(\d{4})/.exec(v))) return iso(m[3], +m[1], +m[2]);
    if ((m = /^\/Date\((\d{12,13})/.exec(v))) return new Date(+m[1]).toISOString().slice(0, 10);
    return null;
  }
  const SKIP_KEY = /birth|dob|created|modified|updated|expir|issue|submitted|paid|receipt/i;

  function today() {
    return new Date().toISOString().slice(0, 10);
  }

  // Pulls future appointment dates out of any JSON shape the booking sites return:
  // [{"date":"2027-01-15","business_day":true}]                       (AIS)
  // {"ScheduleDays":[{"Date":"2027-01-15T00:00:00", ...}]}             (CGI)
  // ["2027-01-15", ...]
  function extractDates(json, from) {
    const min = from || today();
    const out = new Set();
    (function walk(v, key, depth) {
      if (depth > 8 || v == null) return;
      if (typeof v === "string") {
        const d = toISO(v.trim());
        if (d && (key === null || /date|day|slot/i.test(key)) && !SKIP_KEY.test(key || "") && d >= min) out.add(d);
        return;
      }
      if (Array.isArray(v)) {
        for (const x of v) walk(x, key, depth + 1);
        return;
      }
      if (typeof v === "object") {
        // AIS marks non-bookable days with business_day: false
        if (v.business_day === false) return;
        if (v.IsAvailable === false || v.isAvailable === false || v.Available === false) return;
        for (const k of Object.keys(v)) walk(v[k], k, depth + 1);
      }
    })(json, null, 0);
    return [...out].sort();
  }

  // Locale in AIS URLs is like "en-mx" or "es-co"; the part after the dash is the country.
  function parseAisUrl(href) {
    const m = /^https:\/\/ais\.usvisa-info\.com\/([a-z]{2}-[a-z]{2})\/niv\/schedule\/(\d+)\//i.exec(href);
    if (!m) return null;
    return { locale: m[1], cc: m[1].split("-")[1].toUpperCase(), scheduleId: m[2] };
  }

  function aisDaysUrl(info, facilityId) {
    return `https://ais.usvisa-info.com/${info.locale}/niv/schedule/${info.scheduleId}/appointment/days/${facilityId}.json?appointments[expedite]=false`;
  }

  // Responses worth looking at on usvisascheduling.com.
  function looksLikeSlotUrl(url) {
    return /schedule-?days|scheduledays|available|availability|calendar|appointment\/days|get-family-consular|get-family-ofc/i.test(url || "");
  }

  const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
  const jitter = (lo, hi) => lo + Math.random() * (hi - lo);

  const api = { extractDates, parseAisUrl, aisDaysUrl, looksLikeSlotUrl, sleep, jitter };
  g.VSA = api;
  if (typeof module !== "undefined") module.exports = api;
})(typeof globalThis !== "undefined" ? globalThis : this);
