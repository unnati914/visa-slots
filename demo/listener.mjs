// Separate process so its timestamps are not delayed by video encoding work.
// Prints "dash <ms>" when a dashboard user receives the Mumbai slot, "phone <ms> <json>" when ntfy delivers.
const topic = process.argv[2];
(async () => {
  const res = await fetch("http://localhost:8080/api/stream?country=IN");
  const dec = new TextDecoder();
  let done = false;
  for await (const chunk of res.body) {
    const t = Date.now();
    const s = dec.decode(chunk);
    if (!done && s.includes('"city":"Mumbai"') && s.includes('"earliest"')) { done = true; console.log("dash", t); }
  }
})();
(async () => {
  const res = await fetch(`https://ntfy.sh/${topic}/json`);
  const dec = new TextDecoder();
  let done = false;
  for await (const chunk of res.body) {
    const t = Date.now();
    for (const l of dec.decode(chunk).split("\n")) {
      try {
        const m = JSON.parse(l);
        if (!done && m.event === "message" && /^Slot open/.test(m.title || "")) { done = true; console.log("phone", t, JSON.stringify({ title: m.title, message: m.message })); }
      } catch {}
    }
  }
})();
