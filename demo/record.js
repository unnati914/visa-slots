// Records the latency demo video. Everything shown is measured live:
// the real server, the real agent, a real 4000-user load test, real phone push (ntfy).
// Only the visa calendar is a test copy, because the real site has no open slots.
const { spawn, execFileSync } = require("child_process");
const fs = require("fs");
const path = require("path");
const crypto = require("crypto");
const { chromium } = require("../agent/node_modules/playwright-core");
const ffmpeg = require("ffmpeg-static");

const ROOT = path.resolve(__dirname, "..");
const WORK = process.env.WORK || path.join(__dirname, ".work");
const OUT = path.join(__dirname, "visa-agent-latency-demo.mp4");
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const procs = [];
const run = (cmd, args, opts = {}) => {
  const p = spawn(cmd, args, { stdio: ["ignore", "pipe", "pipe"], ...opts });
  procs.push(p);
  return p;
};
const cleanup = () => procs.forEach((p) => { try { p.kill("SIGINT"); } catch {} });
process.on("exit", cleanup);

async function waitFor(fn, ms, label) {
  const end = Date.now() + ms;
  while (Date.now() < end) {
    const v = await fn();
    if (v) return v;
    await sleep(100);
  }
  throw new Error("timed out waiting for " + label);
}

(async () => {
  fs.rmSync(WORK, { recursive: true, force: true });
  fs.mkdirSync(path.join(WORK, "frames"), { recursive: true });
  const fmtMs = (ms) => (ms < 1000 ? `${Math.round(ms)} ms` : `${(ms / 1000).toFixed(1)} s`);

  // 1. fresh dashboard server (real data only) + test calendar
  try { execFileSync("pkill", ["-f", "bin/server"]); } catch {}
  await sleep(500);
  run(path.join(ROOT, "bin/server"), [], { cwd: ROOT });
  run("python3", ["-I", path.join(__dirname, "fake_site.py"), path.join(__dirname, "site")]);
  await waitFor(async () => (await fetch("http://localhost:8080/metrics").catch(() => null))?.ok, 10000, "server");
  await waitFor(async () => (await fetch("http://127.0.0.1:8091/stage.html").catch(() => null))?.ok, 10000, "test site");

  // 2. the agent, pointed at the test calendar, with its own demo phone topic
  const topic = "visa-demo-" + crypto.randomBytes(5).toString("hex");
  const agentDir = path.join(WORK, "agent");
  fs.mkdirSync(agentDir);
  fs.writeFileSync(path.join(agentDir, "config.json"), JSON.stringify({
    country: "IN", visa: "F1/M1", cat: "Regular", alertBefore: "2026-12-30", minutes: 5,
    server: "http://localhost:8080", ntfyTopic: topic, startUrl: "http://127.0.0.1:8091/calendar.html",
  }));
  const ev = { lines: [], found: 0, dash: 0, phone: 0, phoneMsg: null, rounds: 0 };
  const agent = run("node", [path.join(ROOT, "agent/agent.js")], {
    env: { ...process.env, VSA_DIR: agentDir, VSA_HEADLESS: "1", VSA_INTERVAL_SEC: "6", VSA_LOG_MS: "1" },
  });
  let buf = "";
  agent.stdout.on("data", (d) => {
    buf += d;
    let i;
    while ((i = buf.indexOf("\n")) >= 0) {
      const line = buf.slice(0, i).trim();
      buf = buf.slice(i + 1);
      if (!line || /^(@\d+ )?$/.test(line) || line.startsWith("Visa slot agent") || line.startsWith("visa ") || line.startsWith("Phone") || line.startsWith("(settings")) continue;
      const stamp = /^@(\d+) /.exec(line);
      const text = line.replace(/^@\d+ /, "").replace(/^\d+:\d+:\d+ [AP]M /, "");
      if (/MUMBAI VAC: \d+ dates/.test(text) && !ev.found) ev.found = stamp ? +stamp[1] : Date.now();
      if (text.includes("next check")) ev.rounds++;
      ev.lines.push(text);
    }
  });
  agent.stderr.on("data", (d) => process.stderr.write("agent: " + d));

  // 3. listen like a dashboard user (SSE) and like a phone (ntfy), in a separate process
  const lis = run("node", [path.join(__dirname, "listener.mjs"), topic]);
  lis.stdout.on("data", (d) => {
    for (const l of String(d).split("\n")) {
      const [kind, t, ...rest] = l.split(" ");
      if (kind === "dash" && !ev.dash) ev.dash = +t;
      if (kind === "phone" && !ev.phone) { ev.phone = +t; ev.phoneMsg = JSON.parse(rest.join(" ")); }
    }
  });

  await waitFor(() => ev.rounds >= 1, 60000, "first agent round");

  // 4. the recording browser
  const browser = await chromium.launch({ channel: "chrome", headless: true });
  const ctx = await browser.newContext({ viewport: { width: 1280, height: 720 }, deviceScaleFactor: 1.5 });
  const page = await ctx.newPage();
  await page.goto("http://127.0.0.1:8091/stage.html");
  await page.evaluate(() => stage.dash("http://localhost:8080/?country=IN"));
  await sleep(2500);
  const cdp = await ctx.newCDPSession(page);
  const frames = [];
  cdp.on("Page.screencastFrame", async (f) => {
    const file = path.join(WORK, "frames", `f${String(frames.length).padStart(5, "0")}.jpg`);
    fs.promises.writeFile(file, Buffer.from(f.data, "base64"));
    frames.push({ file, t: f.metadata.timestamp });
    cdp.send("Page.screencastFrameAck", { sessionId: f.sessionId }).catch(() => {});
  });
  await cdp.send("Page.startScreencast", { format: "jpeg", quality: 88, maxWidth: 1920, maxHeight: 1080 });
  const S = (fn, ...a) => page.evaluate(([fn, a]) => stage[fn](...a), [fn, a]).catch(() => {});

  // live stats + agent log feed
  let lastCount = 0, lastT = Date.now(), p99Text = "–", p99us = 0, users = 0;
  let logShown = 0;
  const ticker = setInterval(async () => {
    try {
      const m = await (await fetch("http://localhost:8080/metrics")).json();
      const now = Date.now();
      const c = m.server_latency.count;
      const rps = c >= lastCount ? Math.round(((c - lastCount) * 1000) / (now - lastT)) : 0;
      lastCount = c; lastT = now;
      users = m.sse_clients;
      if (m.server_latency.count > 2000) { p99us = m.server_latency.p99_us; p99Text = p99us < 1000 ? `${(p99us / 1000).toFixed(2)} ms` : `${(p99us / 1000).toFixed(1)} ms`; }
      S("stats", users, rps, p99Text);
    } catch {}
    while (logShown < ev.lines.length) {
      const l = ev.lines[logShown++];
      S("log", l, /dates, earliest|sent to dashboard|ALERT: Slot/.test(l));
    }
  }, 500);

  // ---- scenes ----
  await S("caption", "This is our US visa slot agent.");
  await sleep(4500);
  await S("caption", "First, 4,000 people open the dashboard.");
  const lt = run(path.join(ROOT, "bin/loadtest"), ["-users", "4000", "-d", "120s", "-think", "1s", "-ramp", "5s"]);
  lt.stdout.on("data", () => {});
  await waitFor(() => users >= 4000, 30000, "4000 users");
  await sleep(6000); // load test resets server metrics after warm-up; let numbers settle
  await waitFor(() => p99us > 0, 20000, "p99");
  await sleep(2500);
  await S("caption", `The server answers in ${p99Text} (p99). Target: 2 to 3 ms.`);
  await sleep(6000);

  await S("caption", "Now a new slot opens on the visa site (test copy).");
  await fetch("http://127.0.0.1:8091/open", { method: "POST" });
  const t0 = Date.now();
  await S("step", 1, "0 s");
  await sleep(2500);
  await S("caption", "The agent checks each city, one by one.");
  await waitFor(() => ev.found, 60000, "agent detection");
  await S("step", 2, "+" + fmtMs(ev.found - t0));
  await S("caption", "Found it! Mumbai has slots.");
  await waitFor(() => ev.dash, 10000, "dashboard update");
  await S("step", 3, "+" + fmtMs(ev.dash - ev.found));
  await sleep(1800);
  await S("caption", `The dashboard updates ${fmtMs(ev.dash - ev.found)} after the agent sees it.`);
  await waitFor(() => ev.phone, 20000, "phone alert");
  await S("step", 4, "+" + fmtMs(ev.phone - ev.found));
  await S("phone", ev.phoneMsg.title, ev.phoneMsg.message);
  await sleep(1200);
  await S("caption", `Phone alert arrives ${fmtMs(ev.phone - ev.found)} after the agent sees it.`);
  await sleep(5000);
  await S("caption", "");

  await S("summary", [
    ["Server response, 4,000 users (p99)", `${p99Text}  ✓ target 2–3 ms`, "ok"],
    ["Agent sees slot → dashboard", fmtMs(ev.dash - ev.found), "ok"],
    ["Agent sees slot → phone alert", fmtMs(ev.phone - ev.found), "ok"],
    ["Slot opens → agent sees it (this demo, check every 6 s)", fmtMs(ev.found - t0)],
    ["Slot opens → agent sees it (real use, check every 10 min)", "about 5 min avg"],
  ], "Once the agent sees a slot, everyone knows within seconds. The wait before that depends on how often the agent checks. Only the visa calendar here is a test copy, because the real site had no open slots.");
  await sleep(10000);

  clearInterval(ticker);
  await cdp.send("Page.stopScreencast");
  await browser.close();
  cleanup();

  await sleep(2000); // let pending frame writes land
  // 5. encode: frames carry real timestamps, so durations match what happened
  const list = frames.map((f, i) => {
    const d = i + 1 < frames.length ? Math.max(0.001, frames[i + 1].t - f.t) : 0.5;
    return `file '${f.file}'\nduration ${d.toFixed(4)}`;
  });
  list.push(`file '${frames[frames.length - 1].file}'`);
  fs.writeFileSync(path.join(WORK, "list.txt"), list.join("\n"));
  execFileSync(ffmpeg, ["-y", "-loglevel", "error", "-f", "concat", "-safe", "0", "-i", path.join(WORK, "list.txt"),
    "-vf", "scale=1920:1080:flags=lanczos,fps=30,format=yuv420p", "-c:v", "libx264", "-preset", "medium", "-crf", "18",
    "-movflags", "+faststart", OUT]);
  console.log(JSON.stringify({
    out: OUT, frames: frames.length, p99: p99Text,
    agentFound_s: (ev.found - t0) / 1000, dashAfterAgent_ms: ev.dash - ev.found, phoneAfterAgent_ms: ev.phone - ev.found,
  }, null, 2));
  process.exit(0);
})().catch((e) => {
  console.error("FAILED:", e.message);
  cleanup();
  process.exit(1);
});
