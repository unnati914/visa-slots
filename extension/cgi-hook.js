// Runs in the page itself on usvisascheduling.com. It does not send any requests of
// its own: it only looks at the calendar responses the site already loads for you,
// and passes any appointment dates to the extension.
(() => {
  const emit = (url, data) => {
    const dates = VSA.extractDates(data);
    if (VSA.looksLikeSlotUrl(url) || dates.length) {
      window.postMessage({ __vsa: true, url: String(url), dates, empty: dates.length === 0 }, location.origin);
    }
  };
  const tryParse = (text) => {
    try {
      return JSON.parse(text);
    } catch {
      return null;
    }
  };

  const origFetch = window.fetch;
  window.fetch = async function (...args) {
    const res = await origFetch.apply(this, args);
    try {
      const url = typeof args[0] === "string" ? args[0] : args[0]?.url;
      if (VSA.looksLikeSlotUrl(url)) {
        res.clone().text().then((t) => {
          const j = tryParse(t);
          if (j) emit(url, j);
        });
      }
    } catch {}
    return res;
  };

  const origOpen = XMLHttpRequest.prototype.open;
  XMLHttpRequest.prototype.open = function (method, url, ...rest) {
    this.__vsaUrl = url;
    return origOpen.call(this, method, url, ...rest);
  };
  const origSend = XMLHttpRequest.prototype.send;
  XMLHttpRequest.prototype.send = function (...args) {
    if (VSA.looksLikeSlotUrl(this.__vsaUrl)) {
      this.addEventListener("load", () => {
        try {
          const j = this.responseType === "json" ? this.response : tryParse(this.responseText);
          if (j) emit(this.__vsaUrl, j);
        } catch {}
      });
    }
    return origSend.apply(this, args);
  };
})();
