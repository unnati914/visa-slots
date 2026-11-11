const DEFAULTS = { enabled: true, server: "http://localhost:8080", country: "IN", city: "", visa: "B1/B2", cat: "Regular", minutes: 10, alertBefore: "2026-12-30" };
const FIELDS = ["visa", "cat", "country", "minutes", "alertBefore", "server"];
const $ = (id) => document.getElementById(id);
const esc = (s) => String(s).replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" })[c]);

async function load() {
  const { settings, status } = await chrome.storage.local.get(["settings", "status"]);
  const s = { ...DEFAULTS, ...settings };
  for (const f of FIELDS) $(f).value = s[f];
  $("enabled").checked = s.enabled;
  render(status);
}

async function save() {
  const s = { enabled: $("enabled").checked };
  for (const f of FIELDS) s[f] = $(f).value.trim();
  s.country = s.country.toUpperCase();
  s.minutes = Math.max(5, Number(s.minutes) || 10);
  $("minutes").value = s.minutes;
  await chrome.storage.local.set({ settings: s });
  await chrome.runtime.sendMessage({ type: "settings-changed" });
}

function render(st) {
  if (!st) return;
  const when = new Date(st.at).toLocaleTimeString();
  let h = `<div class="mute">Last check ${esc(when)}</div>`;
  for (const p of st.problems || []) h += `<div class="prob">${esc(p)}</div>`;
  for (const r of st.results || []) {
    h += `<div class="res"><span>${esc(r.city)}</span>${r.earliest ? `<b class="ok">${esc(r.earliest)} <span class="mute">(${r.count})</span></b>` : `<span class="mute">no slots</span>`}</div>`;
  }
  $("status").innerHTML = h;
}

$("save").onclick = save;
$("enabled").onchange = save;
$("now").onclick = async () => {
  await save();
  $("now").textContent = "Checking...";
  $("now").disabled = true;
  const st = await chrome.runtime.sendMessage({ type: "check-now" });
  $("now").textContent = "Check now";
  $("now").disabled = false;
  if (st && !st.busy) render(st);
};
chrome.storage.onChanged.addListener((c) => c.status && render(c.status.newValue));
load();
