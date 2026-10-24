"use strict";
// The panel page ("Control Room" style). One page, with views chosen by the
// address after # (see route): the servers dashboard, one server with its
// Overview, Players, Settings, Backups and Console tabs, and the Backups,
// Help, Settings, Import, Add server, Setup guide, People and Account pages.
//
// What shows depends on who's signed in: the owner sees everything; anyone
// else sees only the servers the owner gave them, with the buttons their
// role on each allows (see access).

const $ = (id) => document.getElementById(id);
const labels = {
  running: "Running", starting: "Starting", stopping: "Stopping", stopped: "Stopped",
  installing: "Installing", importing: "Importing", updating: "Updating", restoring: "Restoring", crashed: "Crashed", error: "Needs attention",
};
let selected = null;      // server ID shown in the detail section
let tab = "overview";
let consoleStream = null;
let settings = {};

async function api(path, options = {}) {
  const res = await fetch(path, {
    ...options,
    headers: { "Content-Type": "application/json", "X-Block-Party": "1", ...(options.headers || {}) },
  });
  let body = null;
  try { body = await res.json(); } catch (_) { /* empty */ }
  if (res.status === 401 && !["/api/auth/state", "/api/auth/login", "/api/auth/login/code", "/api/auth/setup", "/api/auth/reset", "/api/auth/password"].includes(path)) {
    signedOut(); // the session ended (signed out elsewhere, or expired)
  }
  if (!res.ok) throw new Error((body && body.error) || res.statusText);
  return body;
}
const post = (path, body) => api(path, { method: "POST", body: JSON.stringify(body || {}) });
const del = (path) => api(path, { method: "DELETE" });
const sid = () => `/api/servers/${encodeURIComponent(selected)}`;

function el(tag, attrs = {}, ...children) {
  const e = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (k === "class") e.className = v;
    else if (k.startsWith("on")) e.addEventListener(k.slice(2), v);
    else if (v !== false && v != null) e.setAttribute(k, v === true ? "" : v);
  }
  for (const c of children) if (c != null) e.append(c);
  return e;
}

// ---- server list (the dashboard) ----

let serverList = [];       // last /api/servers answer
let latestVersion = "";    // newest Bedrock release, when known
let joinInfo = null;       // console list name and IP, from /api/info
let me = { owner: false, canAdd: false, name: "" }; // who's signed in

// Access levels on one server, as /api/servers reports them.
const SEE = 1, RUN = 2, CARE = 3;
const access = (s) => (s && s.access) || 0;

function action(id, what) {
  return async (ev) => {
    ev.currentTarget.disabled = true;
    try { await post(`/api/servers/${encodeURIComponent(id)}/${what}`); }
    catch (err) { alert(err.message); }
    refresh();
  };
}

const busyStates = ["installing", "importing", "updating", "restoring", "starting", "stopping"];
const isLive = (s) => s.state === "running" || s.state === "starting";

// Words that differ between the two styles: Treehouse uses plainer ones.
const words = {
  control: { start: "Start", stop: "Stop", allow: "Allow", running: "Running", stopped: "Stopped", waiting: "Tried to join", nobody: "Nobody", noVersion: "…" },
  treehouse: { start: "Turn on", stop: "Turn off", allow: "Let in", running: "On", stopped: "Off", waiting: "Wants to join", nobody: "Nobody playing", noVersion: "" },
};
const word = (k) => (words[prefs.style] || words.control)[k];

function stateEl(s) {
  const text = s.state === "running" || s.state === "stopped" ? word(s.state) : (labels[s.state] || s.state);
  return el("span", { class: `state ${s.state}` }, el("span", { class: "dot" }), text);
}

// Each server gets its own colour band in Treehouse, the same every time.
const bands = [["#5fae4f", "#8b5a2b"], ["#e39ac6", "#b0648f"], ["#6fb3e0", "#3b7bb0"], ["#e3cf7b", "#b39b3f"],
  ["#e0925c", "#a95f2a"], ["#a99be0", "#6f5fb0"], ["#6fcfbf", "#3a8f82"], ["#b8b2a6", "#8a8478"]];
function bandStyle(id) {
  let h = 0;
  for (const c of id) h = (h * 31 + c.charCodeAt(0)) >>> 0;
  const [a, b] = bands[h % bands.length];
  return `--band: ${a}; --band2: ${b}`;
}

function greeting() {
  const h = new Date().getHours();
  const part = h < 5 ? "Good evening" : h < 12 ? "Good morning" : h < 18 ? "Good afternoon" : "Good evening";
  return `${part}, ${$("side-user").textContent || "there"}`;
}

// needsUpdate says whether a server is behind the newest release, and
// whether it's far behind (a different 1.x, or ten or more versions back).
function needsUpdate(s) {
  if (s.type === "hytale") return s.updateTo ? { far: false, to: s.updateTo } : null;
  if (s.type === "java") {
    if (!s.updateTo) return s.consolesUpdate ? { far: false, consoles: true } : null;
    const to = s.updateTo.split(" ")[0].split(".").map(Number), v = (s.version || "").split(".").map(Number);
    return { far: to[0] !== v[0] || to[1] !== v[1], to: s.updateTo };
  }
  if (!latestVersion || !s.version || s.preview) return null;
  if (!newer(latestVersion, s.version)) return null;
  const l = latestVersion.split(".").map(Number), v = s.version.split(".").map(Number);
  return { far: l[0] !== v[0] || l[1] !== v[1] || (l[2] || 0) - (v[2] || 0) >= 10 };
}

function newer(a, b) {
  const pa = a.split(".").map(Number), pb = b.split(".").map(Number);
  for (let i = 0; i < Math.max(pa.length, pb.length); i++) {
    const x = pa[i] || 0, y = pb[i] || 0;
    if (x !== y) return x > y;
  }
  return false;
}

function whenShort(iso) {
  if (!iso) return "Never";
  const d = new Date(iso);
  const today = new Date();
  const yesterday = new Date(Date.now() - 864e5);
  const time = d.toLocaleTimeString([], { hour: "numeric", minute: "2-digit" });
  if (d.toDateString() === today.toDateString()) return `Today ${time}`;
  if (d.toDateString() === yesterday.toDateString()) return `Yesterday ${time}`;
  return d.toLocaleDateString([], { month: "short", day: "numeric" }) + " " + time;
}

function playingText(s) {
  if (s.players.length) return s.players.map((p) => p.name).join(", ");
  return s.state === "running" ? word("nobody") : "";
}

const serverHref = (id, tab) => `#/server/${encodeURIComponent(id)}/${tab || "overview"}`;

function startStopButton(s) {
  const busy = busyStates.includes(s.state);
  if (isLive(s)) {
    // Someone who may only turn it on can't turn it off.
    return access(s) >= RUN ? el("button", { class: "danger", onclick: action(s.id, "stop"), disabled: s.state === "stopping" }, word("stop")) : null;
  }
  // Outside its hours only the owner can open it (and is asked first).
  const closed = s.hours && !s.hours.open;
  if (closed && !me.owner) {
    return el("button", { class: "go", disabled: true, title: hoursNote(s.hours) }, word("start"));
  }
  const start = action(s.id, "start");
  return el("button", { class: "go", disabled: busy, onclick: closed
    ? (ev) => { if (confirm(`${s.name} is closed for the night (${hoursNote(s.hours).replace(/^Closed for the night, /, "")}).\n\nStart it anyway? It stays open until it would open anyway, unless you stop it.`)) start(ev); }
    : start }, word("start"));
}

function renderServerRow(s) {
  const upd = needsUpdate(s);
  const extra = [];
  if (s.players.length) extra.push(`${s.players.length} playing`);
  if (s.waiting) extra.push(`${s.waiting} waiting to be let in`);
  if (upd && access(s) >= CARE) extra.push("update ready");
  const open = access(s) >= RUN;
  return el("div", { class: "srow", style: bandStyle(s.id) },
    el("div", {},
      open ? el("a", { class: "name", href: serverHref(s.id) }, s.name) : el("span", { class: "name" }, s.name),
      s.outside ? el("span", { class: "badge outside", title: "Friends outside the house can join" }, "Friends outside") : null,
      el("div", { class: "sub" }, `port ${s.port}`),
      s.message ? el("div", { class: "note warn" }, s.message) : null,
      s.hours ? el("div", { class: "note muted" }, hoursNote(s.hours)) : null),
    el("div", {}, stateEl(s)),
    el("div", { class: "cell-muted" }, playingText(s), s.waiting ? el("div", { class: "note warn" }, `${s.waiting} waiting to be let in`) : null),
    el("div", {},
      el("div", { class: "mono" }, s.type === "java" ? (s.version ? "Java " + s.version : "Java") : s.type === "hytale" ? (s.version ? "Hytale " + s.version : "Hytale") : (s.version || word("noVersion"))),
      upd && access(s) >= CARE ? el("div", { class: "note upd" + (upd.far ? " far" : "") }, upd.far ? "Well behind: update" : "Update ready") : null),
    el("div", { class: "cell-muted" }, whenShort(s.lastBackup)),
    el("div", { class: "acts" },
      startStopButton(s),
      open ? el("a", { class: "button", href: serverHref(s.id) }, "Open") : null),
    extra.length ? el("div", { class: "mobile-extra" }, extra.join(" · ")) : null);
}

function renderDashboard(list, force) {
  const key = JSON.stringify([list, latestVersion, prefs.style, me]);
  if (key === dashKey && !force) return; // unchanged: keep keyboard focus
  dashKey = key;
  $("servers-title").textContent = prefs.style === "treehouse" ? greeting() : "Servers";
  $("dash-attempts-title").textContent = word("waiting");
  const head = el("div", { class: "srow head" },
    el("div", {}, "Server"), el("div", {}, "Status"), el("div", {}, "Playing"),
    el("div", {}, "Version"), el("div", {}, "Last backup"), el("div", {}));
  $("servers").replaceChildren(...(list.length ? [head, ...list.map(renderServerRow)] : []));
  $("servers").hidden = list.length === 0;
  $("empty").hidden = list.length > 0;
  if (!list.length) {
    $("empty").replaceChildren(...(me.owner
      ? ["No servers yet. ", el("a", { href: "#/add" }, "Add a server"), " to download Bedrock and start one, or ", el("a", { href: "#/import" }, "import"), " your servers from Crafty."]
      : me.canAdd
        ? ["No servers yet. ", el("a", { href: "#/add" }, "Add a server"), ", or ask ", ownerName(), " to give you one."]
        : ["No servers for you yet. Ask ", ownerName(), " to give you one on the People page."]));
  }
  const running = list.filter((s) => s.state === "running").length;
  const playing = list.reduce((n, s) => n + s.players.length, 0);
  $("servers-summary").textContent = list.length
    ? `${list.length} server${list.length === 1 ? "" : "s"} · ${running} running · ${playing} ${playing === 1 ? "person" : "people"} playing`
    : "";

  // Update note
  const behind = list.filter((s) => access(s) >= CARE && needsUpdate(s) && !busyStates.includes(s.state));
  $("update-banner").hidden = behind.length === 0;
  if (behind.length) {
    const javaTo = behind.find((s) => s.type === "java");
    const hytaleTo = behind.find((s) => s.type === "hytale");
    const headline = behind.some((s) => s.type === "bedrock" || !s.type) ? `Bedrock ${latestVersion} is out. `
      : hytaleTo && !javaTo ? `Hytale ${hytaleTo.updateTo} is out. `
        : javaTo && javaTo.updateTo ? `Paper ${javaTo.updateTo.split(" ")[0]} is out. ` : "New console support for Java servers is out. ";
    $("update-banner-text").replaceChildren(el("b", {}, headline),
      `${behind.length} server${behind.length === 1 ? " can" : "s can"} update. Each one is backed up first and started again if it was running.`);
    $("update-all").textContent = behind.length === 1 ? "Update it" : `Update all ${behind.length}`;
    checkNightly();
  }

  // Backups card
  const never = list.filter((s) => !s.lastBackup).map((s) => s.name);
  if (!list.length) $("dash-backups").textContent = "No servers yet.";
  else if (never.length) $("dash-backups").textContent = `Not backed up yet: ${never.join(", ")}.`;
  else {
    const oldest = list.reduce((a, b) => (new Date(a.lastBackup) <= new Date(b.lastBackup) ? a : b));
    $("dash-backups").textContent = list.length === 1
      ? `Last backup ${whenShort(oldest.lastBackup).toLowerCase()}.`
      : `All ${list.length} are backed up. Longest ago: ${oldest.name}, ${whenShort(oldest.lastBackup).toLowerCase()}.`;
  }
  loadDashAttempts(list);
  if (me.owner) loadDashOutside();
}

let dashOutsideAt = 0;
async function loadDashOutside() {
  if (Date.now() - dashOutsideAt < 15000) return;
  dashOutsideAt = Date.now();
  try {
    const st = await api("/api/outside");
    const open = (st.servers || []).filter((x) => x.reached).length;
    $("dash-outside-text").textContent = !st.settings.enabled
      ? "Off. Friends who don't live here can't join."
      : `On · ${open} server${open === 1 ? "" : "s"} open to them` + (st.address ? ` at ${st.address}` : "") + ".";
  } catch (_) { /* shown on the page itself */ }
}

$("update-all").addEventListener("click", async (ev) => {
  const button = ev.currentTarget;
  const behind = serverList.filter((s) => access(s) >= CARE && needsUpdate(s) && !busyStates.includes(s.state));
  const playing = behind.reduce((n, s) => n + s.players.length, 0);
  const note = playing ? `\n\n${playing} player(s) will be disconnected.` : "";
  if (!confirm(`Update ${behind.map((s) => s.name).join(", ")} to the newest version? Each is backed up first.${note}`)) return;
  button.disabled = true;
  for (const s of behind) {
    try { await post(`/api/servers/${encodeURIComponent(s.id)}/update`); } catch (err) { alert(`${s.name}: ${err.message}`); }
  }
  button.disabled = false;
  refresh();
});

// Tried to join, across servers. Only servers with someone waiting are asked.
let attemptsLoadedAt = 0;
async function loadDashAttempts(list) {
  const waiting = list.filter((s) => s.waiting > 0 && access(s) >= RUN);
  if (!waiting.length) { $("dash-attempts").hidden = true; return; }
  if (Date.now() - attemptsLoadedAt < 5000) return;
  attemptsLoadedAt = Date.now();
  const rows = [];
  for (const s of waiting) {
    try {
      const v = await api(`/api/servers/${encodeURIComponent(s.id)}/players`);
      for (const a of v.attempts || []) {
        rows.push(row(a.name, `${s.name} · ${ago(a.at)}`,
          btn(word("allow"), async () => {
            try { await post(`/api/servers/${encodeURIComponent(s.id)}/allowlist`, { name: a.name }); } catch (err) { alert(err.message); }
            attemptsLoadedAt = 0;
            refresh();
          }, "primary")));
      }
    } catch (_) { /* shown on the server's page */ }
  }
  $("dash-attempts-list").replaceChildren(...rows);
  $("dash-attempts").hidden = rows.length === 0;
}

function renderBackupsOverview(list) {
  list = list.filter((s) => access(s) >= RUN);
  $("backups-none").hidden = list.length > 0;
  $("backups-overview").hidden = list.length === 0;
  const head = el("div", { class: "srow head" },
    el("div", {}, "Server"), el("div", {}, "Status"), el("div", {}, ""), el("div", {}, ""), el("div", {}, "Last backup"), el("div", {}));
  $("backups-overview").replaceChildren(head, ...list.map((s) => el("div", { class: "srow" },
    el("div", {}, el("a", { class: "name", href: serverHref(s.id, "backups") }, s.name)),
    el("div", {}, stateEl(s)), el("div", {}), el("div", {}),
    el("div", { class: "cell-muted" }, whenShort(s.lastBackup)),
    el("div", { class: "acts" }, el("a", { class: "button", href: serverHref(s.id, "backups") }, "Backups")),
    el("div", { class: "mobile-extra" }, `Last backup: ${whenShort(s.lastBackup)}`))));
}

let detailKey = "";   // what the server page header last showed
let ovPlayersKey = ""; // what the Overview player list last showed
let dashKey = "";      // what the dashboard last showed

function renderDetailHead(s) {
  const key = JSON.stringify([s.id, s.name, s.state, s.version, s.build, s.updateTo, s.consoles, s.consolesUpdate, s.consolesLive, s.consolesWaiting, s.port, s.message, latestVersion, s.players.length, prefs.style, s.access, s.hours]);
  if (key === detailKey) return; // unchanged: keep keyboard focus where it is
  detailKey = key;
  $("detail-name").textContent = s.name;
  const upd = needsUpdate(s);
  $("detail-facts").replaceChildren(stateEl(s),
    s.type === "hytale" ? el("span", {}, "Hytale ", el("span", { class: "mono" }, s.version || "…"))
    : s.type === "java"
      ? el("span", {}, "Java ", el("span", { class: "mono" }, s.version || "…"), s.build ? el("span", { class: "muted" }, ` · Paper build ${s.build}`) : null,
        s.consolesWaiting
          ? el("span", { class: "warn", title: `Geyser doesn't support Minecraft ${s.consolesWaiting} yet; consoles can join once a newer Geyser is out` }, " · consoles after a Geyser update")
          : s.consoles ? el("span", { class: "muted", title: s.consolesLive ? "Consoles, phones and tablets can join now" : "Consoles can join while it's running with console support" }, " · consoles too") : null)
      : el("span", {}, "Bedrock ", el("span", { class: "mono" }, s.version || "…")),
    el("span", {}, "Port ", el("span", { class: "mono" }, String(s.port))),
    s.hours ? el("span", { class: s.hours.open ? "" : "warn" }, hoursNote(s.hours)) : null);
  $("detail-message").textContent = s.message || "";
  $("detail-message").hidden = !s.message;
  const busy = busyStates.includes(s.state);
  const copying = s.state === "importing";
  const updBtn = el("button", { class: upd ? "info" : "", onclick: confirmUpdate(s), disabled: copying || s.state === "installing" || s.state === "updating" || s.state === "restoring" },
    "Update", upd ? el("span", { class: "long" }, upd.consoles ? " console support" : ` to ${upd.to ? upd.to.split(" ")[0] : latestVersion}`) : null);
  $("detail-actions").replaceChildren(...[startStopButton(s),
    el("button", { onclick: action(s.id, "restart"), disabled: !isLive(s) || busy }, "Restart"),
    access(s) >= CARE ? updBtn : null].filter(Boolean));
  for (const t of ["players", "settings", "addons", "backups"]) $("tab-" + t).hidden = copying;
  $("tab-addons").hidden = copying || (s.type && s.type !== "bedrock"); // Bedrock add-ons
  $("tab-plugins").hidden = copying || s.type !== "java" || access(s) < CARE; // Java plugins
  $("ov-backups-link").textContent = access(s) >= CARE ? "Restore or download" : "See backups";
}

function confirmUpdate(s) {
  const run = action(s.id, "update");
  return (ev) => {
    const note = s.players.length ? `\n\n${s.players.length} player(s) will be disconnected.` : "";
    if (confirm(`Update ${s.name}? It's backed up first, then started again if it was running.${note}`)) run(ev);
  };
}

async function refresh() {
  if (!signedIn) return;
  try {
    const list = await api("/api/servers");
    if (!signedIn) return; // signed out while this was loading
    serverList = list;
    importsFinished(list);
    if (view === "servers") renderDashboard(list);
    if (view === "backups") renderBackupsOverview(list);
    if (view === "server") {
      const s = list.find((x) => x.id === selected);
      if (s && access(s) < RUN) {
        location.hash = "#/"; // the owner changed what this account may do
      } else if (s) {
        renderDetailHead(s);
        if (tab === "settings") { renderHold(); renderMemory(); renderConsoles(); renderHours(); }
        if (tab === "overview") renderOverviewStatus(s);
      } else {
        resetSettings(); // it's gone (a failed import, say): nothing to save
        location.hash = "#/";
      }
    }
  } catch (err) {
    if (!signedIn) return;
    $("servers").hidden = false;
    $("servers").replaceChildren(el("p", { class: "error card" }, "Can't reach the panel: " + err.message));
  }
}

// checkUpdates asks which Bedrock release is newest. The panel checks with
// Mojang in the background, so the first answer may be empty: ask again soon.
async function checkUpdates(tries = 6) {
  try {
    const u = await api("/api/updates");
    if (u.latest && u.latest !== latestVersion) { latestVersion = u.latest; refresh(); }
    if (!u.latest && !u.error && tries > 0 && signedIn) setTimeout(() => checkUpdates(tries - 1), 5000);
  } catch (_) { /* try again later */ }
}

// ---- pages ----

let view = "servers";

// route shows the page the address names: #/, #/backups, #/help,
// #/import, #/add, #/guide, #/people, #/account, #/server/<id>/<tab>. (#/joining, the
// old address of the joining help, opens Help.)
let acceptedHash = "#/"; // the address of the page on screen

// stay puts the address back to the page on screen, after the person chose
// not to leave it (unsaved settings). It doesn't add to the history.
function stay() {
  history.replaceState(null, "", acceptedHash);
}

function route() {
  if (!signedIn) return;
  let parts;
  try {
    parts = location.hash.replace(/^#\/?/, "").split("/").map(decodeURIComponent);
  } catch (_) {
    parts = [];
  }
  let next = parts[0] || "servers";
  if (view === "notify" && notifyDirty && next !== "notify") {
    if (!confirm("Leave without saving your notification settings?")) { stay(); return; }
    notifyDirty = false;
  }
  if (next === "joining") next = "help";
  if (!["servers", "backups", "help", "import", "add", "guide", "people", "outside", "upkeep", "notify", "account", "server", "settings", "playtime"].includes(next)) next = "servers";
  if ((["import", "guide", "people", "outside", "upkeep", "notify", "settings", "playtime"].includes(next) && !me.owner) || (next === "add" && !me.canAdd)) {
    location.hash = "#/"; // not for this account
    return;
  }
  if (next === "server") {
    const id = parts[1];
    const t = ["overview", "players", "settings", "addons", "plugins", "backups", "console"].includes(parts[2]) ? parts[2] : "overview";
    if (!id) { location.hash = "#/"; return; }
    const known = serverList.find((x) => x.id === id);
    if (known && access(known) < RUN) { location.hash = "#/"; return; }
    if (id !== selected || view !== "server") {
      if (settingsDirty() && !confirm("Leave without saving your settings changes?")) { stay(); return; }
      closeConsole();
      closeOverviewConsole();
      resetSettings();
      resetBackups();
      resetAddons();
      resetPlugins();
      clearServerPanes();
      selected = id;
      $("detail-name").textContent = "";
      $("detail-facts").replaceChildren();
      $("detail-actions").replaceChildren();
      const s = serverList.find((x) => x.id === id);
      if (s) renderDetailHead(s);
    }
    const s = serverList.find((x) => x.id === id);
    let tabOK = !(s && s.state === "importing" && ["players", "settings", "addons", "plugins", "backups"].includes(t));
    if (s && t === "plugins" && (s.type !== "java" || access(s) < CARE)) tabOK = false;
    if (s && t === "addons" && s.type && s.type !== "bedrock") tabOK = false;
    acceptedHash = location.hash;
    setView("server");
    showTab(tabOK ? t : "overview");
    refresh();
    return;
  }
  if (view === "server") {
    if (settingsDirty() && !confirm("Leave without saving your settings changes?")) { stay(); return; }
    resetSettings();
    resetBackups();
    closeConsole();
    closeOverviewConsole();
    selected = null;
  }
  acceptedHash = location.hash || "#/";
  setView(next);
  if (next === "servers") renderDashboard(serverList, true);
  if (next === "backups") renderBackupsOverview(serverList);
  if (next === "import") loadImports();
  if (next === "add") {
    $("add-owner").value = me.owner ? settings.ownerGamertag || "" : "";
    $("add-kid-note").hidden = me.owner;
    showEdition();
    $("add-name").focus();
  }
  if (next === "people") loadPeople();
  if (next === "account") loadTwo();
  else if ($("two-codes-list").textContent) { $("two-codes-list").textContent = ""; $("two-codes").hidden = true; $("two-actions").hidden = false; }
  if (next === "outside") loadOutside(false);
  if (next === "upkeep") loadUpkeep();
  if (next === "notify") loadNotify();
  if (next === "playtime") { closeKidForm(); loadPlaytime(); }
  if (next === "backups" && me.owner) loadDeleted();
  if (next === "guide") openGuide(1);
  if (next === "help") fillJoinInfo();
  refresh();
}

function setView(v) {
  view = v;
  for (const sec of document.querySelectorAll(".view")) sec.hidden = sec.id !== "view-" + v;
  // Pages without their own menu entry light up the one they're reached from.
  const navKey = { server: "servers", add: "servers", import: "servers", guide: "help", outside: "settings", upkeep: "settings", notify: "settings", playtime: "settings", people: me.owner ? "people" : "account" }[v] || v;
  for (const a of document.querySelectorAll("[data-nav]")) {
    if (a.dataset.nav === navKey) a.setAttribute("aria-current", "page"); else a.removeAttribute("aria-current");
  }
  window.scrollTo(0, 0);
}

function showTab(which) {
  tab = which;
  for (const t of ["overview", "players", "settings", "addons", "plugins", "backups", "console"]) {
    const a = $("tab-" + t);
    a.href = serverHref(selected, t);
    if (t === which) a.setAttribute("aria-current", "page"); else a.removeAttribute("aria-current");
    $("pane-" + t).hidden = t !== which;
  }
  if (which !== "console") closeConsole();
  if (which !== "overview") closeOverviewConsole();
  if (which === "console") openConsole();
  else if (which === "settings") { if (!settingsView || !settingsDirty()) loadSettings(); }
  else if (which === "backups") loadBackups();
  else if (which === "addons") loadAddons();
  else if (which === "plugins") loadPlugins();
  else if (which === "players") loadPlayers();
  else loadOverview();
}

window.addEventListener("hashchange", route);

// Links inside a server page that switch tabs.
for (const a of document.querySelectorAll("[data-goto]")) {
  a.addEventListener("click", (ev) => { ev.preventDefault(); location.hash = serverHref(selected, a.dataset.goto); });
}

// ---- overview tab ----

let ovStream = null;
const ovLines = [];

function closeOverviewConsole() {
  if (ovStream) ovStream.close();
  ovStream = null;
  ovLines.length = 0;
  $("ov-console").replaceChildren();
}

function renderOverviewStatus(s) {
  const key = JSON.stringify([s.id, s.state, s.players]);
  if (key === ovPlayersKey) return;
  ovPlayersKey = key;
  $("ov-playing-title").textContent = s.players.length ? `Playing now · ${s.players.length}` : "Playing now";
  $("ov-online").replaceChildren(...(s.players.length ? s.players.map((p) => row(p.name,
    `since ${new Date(p.since).toLocaleTimeString([], { hour: "numeric", minute: "2-digit" })}`,
    btn("Message", () => {
      const text = prompt(`Message to ${p.name}:`);
      if (text) post(sid() + "/message", { name: p.name, text }).catch((err) => alert(err.message));
    }),
    btn("Kick", () => {
      const reason = prompt(`Kick ${p.name}? Optional reason:`, "");
      if (reason !== null) post(sid() + "/kick", { name: p.name, reason }).catch((err) => alert(err.message));
    }, "danger"))) : [emptyRow(s.state === "running" ? "Nobody is playing." : "The server isn't running.")]));
  $("ov-command").hidden = s.state !== "running";
}

async function loadOverview() {
  if (!signedIn || view !== "server" || tab !== "overview" || !selected) return;
  const id = selected;
  const s = serverList.find((x) => x.id === id);
  if (s) renderOverviewStatus(s);
  if (!ovStream) {
    ovStream = new EventSource(sid() + "/console");
    ovStream.onmessage = (ev) => {
      ovLines.push(ev.data);
      if (ovLines.length > 9) ovLines.splice(0, ovLines.length - 9);
      $("ov-console").replaceChildren(...ovLines.map((l) => el("span", { class: lineClass(l) }, l + "\n")));
    };
  }
  const [settingsV, players, backups, acts, outsideV] = await Promise.all([
    api(`/api/servers/${encodeURIComponent(id)}/settings`).catch(() => null),
    api(`/api/servers/${encodeURIComponent(id)}/players`).catch(() => null),
    api(`/api/servers/${encodeURIComponent(id)}/backups`).catch(() => null),
    api(`/api/activity?server=${encodeURIComponent(id)}&limit=6`).catch(() => null),
    api(`/api/servers/${encodeURIComponent(id)}/outside`).catch(() => null),
  ]);
  if (id !== selected || tab !== "overview") return;
  renderOverviewOutside(outsideV, id);
  if (acts) $("ov-activity").replaceChildren(...(acts.length ? acts.map((e) => activityRow(e, false)) : [emptyRow("Nothing yet.")]));
  // World
  const fields = {};
  for (const g of (settingsV && settingsV.groups) || []) for (const f of g.fields) fields[f.key] = f;
  const shown = (key, label) => {
    const f = fields[key];
    if (!f) return [];
    let v = f.value;
    if (f.options) { const o = f.options.find((x) => x.value === v); if (o) v = o.label; }
    if (f.type === "bool") v = v === "true" ? "On" : "Off";
    return [el("div", {}, el("dt", {}, label), el("dd", {}, v || "(not set)"))];
  };
  $("ov-world").replaceChildren(...shown("gamemode", "Game mode"), ...shown("difficulty", "Difficulty"),
    ...shown("level-name", "World"), ...shown("allow-cheats", "Cheats"));
  // Who can join
  if (!players) $("ov-join").textContent = "Couldn't load the players.";
  if (players) {
    const ops = (players.operators || []).length;
    const allow = (players.allowlist || []).length;
    const wait = (players.attempts || []).length;
    $("ov-join").textContent = (players.allowlistEnabled ? `Allowlist on · ${allow} ${allow === 1 ? "person" : "people"}` : "Allowlist off: anyone who can reach it can join")
      + ` · ${ops} operator${ops === 1 ? "" : "s"}. ` + (wait ? `${wait} waiting to be let in.` : "Nobody waiting to be let in.");
  }
  // Backups
  if (!backups) $("ov-backups").textContent = "Couldn't load the backups.";
  if (backups) {
    const last = (backups.backups || [])[0];
    const p = backups.plan;
    const plan = p.every === "off" ? "Automatic backups are off." : (p.every === "daily" ? `Every day at ${p.at} · keeps ${p.keep}.` : `Every ${p.every.replace("h", " hours").replace(/^1 hours$/, "hour")} · keeps ${p.keep}.`);
    $("ov-backups").textContent = (backups.running ? "Backing up now… " : (last ? `Last: ${whenShort(last.time).toLowerCase()}. ` : "No backups yet. ")) + plan;
    $("ov-backup-now").disabled = backups.running;
  }
}

$("ov-command").addEventListener("submit", async (ev) => {
  ev.preventDefault();
  const input = $("ov-command-input");
  const command = input.value.trim();
  if (!command) return;
  try { await post(sid() + "/command", { command }); input.value = ""; } catch (err) { alert(err.message); }
});
for (const b of document.querySelectorAll("[data-cmd]")) {
  b.addEventListener("click", async () => {
    try { await post(sid() + "/command", { command: b.dataset.cmd }); } catch (err) { alert(err.message); }
  });
}
$("ov-backup-now").addEventListener("click", async (ev) => {
  ev.currentTarget.disabled = true; // before any await: currentTarget is gone after
  try { await post(sid() + "/backups"); } catch (err) { alert(err.message); }
  setTimeout(loadOverview, 1500);
});

// ---- players tab ----

function showPlayersError(err) {
  $("players-error").textContent = err ? err.message : "";
  $("players-error").hidden = !err;
}

// playersCall runs a Players tab request and shows the answer, unless the
// page has moved on to another server meanwhile.
async function playersCall(fn) {
  const id = selected;
  try {
    const v = await fn();
    if (id !== selected) return;
    showPlayersError(null);
    if (v) renderPlayers(v);
  } catch (err) {
    if (id === selected) showPlayersError(err);
  }
}

// clearServerPanes empties what the Players and Overview tabs show, so
// nothing from one server is left on another's page.
function clearServerPanes() {
  for (const id of ["online", "allowlist", "operators", "attempts", "played", "ov-online", "ov-world", "ov-activity"]) $(id).replaceChildren();
  playedFor = "";
  $("attempts-box").hidden = true;
  showPlayersError(null);
  for (const id of ["ov-join", "ov-backups", "ov-outside-text"]) $(id).textContent = "…";
  $("ov-outside-actions").replaceChildren();
  $("ov-outside-problem").hidden = true;
  $("ov-invite-box").hidden = true;
  $("ov-invite").value = "";
  detailKey = "";
  ovPlayersKey = "";
}

async function loadPlayers() {
  if (!selected) return;
  await playersCall(() => api(sid() + "/players"));
  loadPlayed();
}

// The week's play on this server, looked at once a minute at most.
let playedFor = "", playedAt = 0;
async function loadPlayed() {
  const id = selected;
  if (playedFor === id && Date.now() - playedAt < 60000) return;
  playedFor = id; playedAt = Date.now();
  try {
    const list = await api(sid() + "/played");
    if (id !== selected) return;
    $("played").replaceChildren(...(list.length
      ? list.map((p) => row(playerLabel(p.name), `${fmtMinutes(p.minutes)} · last played ${whenShort(p.last)}`))
      : [emptyRow("Nobody has played this week.")]));
  } catch (_) { playedFor = ""; }
}

function row(name, detail, ...buttons) {
  return el("li", {}, el("span", { class: "who" }, name, detail ? el("small", {}, detail) : null), ...buttons);
}

function btn(text, onclick, cls, disabled) {
  return el("button", { class: cls || "", onclick, disabled: !!disabled }, text);
}

function emptyRow(text) {
  return el("li", {}, el("span", { class: "empty" }, text));
}

function ago(iso) {
  const s = Math.round((Date.now() - new Date(iso)) / 1000);
  if (s < 60) return "just now";
  if (s < 3600) return `${Math.round(s / 60)} min ago`;
  return `${Math.round(s / 3600)} h ago`;
}

// playerLabel shows a console player on a Java server (".Gamer_Tag") by
// their gamertag.
function playerLabel(name, console) {
  if (!console && !(name || "").startsWith(".")) return name;
  return el("span", {}, name.slice(1).replace(/_/g, " "), " ", el("span", { class: "badge", title: "Plays on a console, phone or tablet" }, "console"));
}

// playerKindChanged sets an add form's placeholder for who's being added.
function playerKindChanged(kind, input, what) {
  const word = $(kind).value === "console" ? "Gamertag" : "Java username";
  $(input).placeholder = word;
  $(input).setAttribute("aria-label", `${word} to ${what}`);
}
$("allow-kind").addEventListener("change", () => playerKindChanged("allow-kind", "allow-name", "allow"));
$("op-kind").addEventListener("change", () => playerKindChanged("op-kind", "op-name", "make operator"));

// typedName is what an add form sends: console players on a Java server
// are known by "." and their gamertag.
function typedName(kind, input) {
  const name = $(input).value.trim();
  if (!name || $(kind).hidden || $(kind).value !== "console") return name;
  return "." + name.replace(/^\./, "");
}

function renderPlayers(v) {
  for (const k of ["allow-kind", "op-kind"]) {
    $(k).hidden = !(v.java && v.consoles);
    if ($(k).hidden) $(k).value = "java";
  }
  const nameWord = v.hytale ? "Hytale username" : v.java ? "Java username" : "Gamertag";
  $("allow-name").placeholder = nameWord;
  $("op-name").placeholder = nameWord;
  $("allow-name").setAttribute("aria-label", nameWord + " to allow");
  $("op-name").setAttribute("aria-label", nameWord + " to make operator");
  if (v.java) { playerKindChanged("allow-kind", "allow-name", "allow"); playerKindChanged("op-kind", "op-name", "make operator"); }
  for (const k of ["online", "allowlist", "operators", "attempts"]) v[k] = v[k] || [];
  const ops = new Set(v.operators.filter((o) => !o.pending).map((o) => o.name.toLowerCase()));
  const listed = new Set(v.allowlist.map((a) => a.name.toLowerCase()));
  const enc = encodeURIComponent;

  // Tried to join
  $("attempts-box").hidden = v.attempts.length === 0;
  $("attempts").replaceChildren(...v.attempts.map((a) => row(v.java ? playerLabel(a.name) : a.name,
    `${ago(a.at)}${a.verified ? "" : " · Xbox sign-in not checked (PlayStation or Switch)"}`,
    btn(word("allow"), () => playersCall(() => post(sid() + "/allowlist", { name: a.name })), "primary"),
    btn("Dismiss", () => playersCall(() => del(sid() + "/attempts/" + enc(a.name)))))));

  // Online
  $("online").replaceChildren(...(v.online.length ? v.online.map((p) => row(v.java ? playerLabel(p.name) : p.name,
    `playing since ${new Date(p.since).toLocaleTimeString([], { hour: "numeric", minute: "2-digit" })}`,
    ops.has(p.name.toLowerCase()) ? el("span", { class: "badge" }, "operator")
      : btn("Make operator", () => playersCall(() => post(sid() + "/operators", { name: p.name }))),
    v.hytale ? null : btn("Message", () => {
      const text = prompt(`Message to ${p.name}:`);
      if (text) playersCall(() => post(sid() + "/message", { name: p.name, text }));
    }),
    v.hytale ? null : btn("Kick", () => {
      const reason = prompt(`Kick ${p.name}? Optional reason:`, "");
      if (reason !== null) playersCall(() => post(sid() + "/kick", { name: p.name, reason }));
    }, "danger"),
  )) : [emptyRow(v.running ? "Nobody is playing." : "The server isn't running.")]));
  $("say-form").hidden = !v.running || !!v.hytale;

  // Allowlist
  $("allow-enabled").checked = v.allowlistEnabled;
  $("allow-enabled-label").textContent = v.allowlistEnabled ? "On" : "Off";
  $("allow-help").textContent = v.allowlistEnabled
    ? "Only these players can join."
    : "Off: anyone who can reach the server can join. The list is kept for when you turn it back on.";
  $("allowlist").replaceChildren(...(v.allowlist.length ? v.allowlist.map((a) => row(v.java ? playerLabel(a.name, a.console) : a.name,
    a.pending ? "added when the server next starts"
      : v.hytale ? (/^[0-9a-f-]{36}$/i.test(a.name) ? "hasn't played here yet (shown by account ID)" : "")
        : (v.java || a.xuid ? "" : "hasn't joined yet"),
    ops.has(a.name.toLowerCase()) ? el("span", { class: "badge" }, "operator") : null,
    btn("Remove", () => {
      if (confirm(`Remove ${a.name} from the allowlist?`)) playersCall(() => del(sid() + "/allowlist/" + enc(a.name)));
    }, "danger"),
  )) : [emptyRow("Nobody yet.")]));

  // Operators
  $("operators").replaceChildren(...(v.operators.length ? v.operators.map((o) => row(v.java ? playerLabel(o.name, o.console) : o.name,
    o.pending ? (v.java && !o.console ? "made an operator when the server next starts" : "becomes operator when they next join") : (listed.has(o.name.toLowerCase()) || !v.allowlistEnabled ? "" : "not on the allowlist"),
    btn("Remove", () => playersCall(() => del(sid() + "/operators/" + enc(o.pending ? o.name : (o.xuid || o.name))))),
  )) : [emptyRow("No operators.")]));
}

$("allow-form").addEventListener("submit", (ev) => {
  ev.preventDefault();
  const name = typedName("allow-kind", "allow-name");
  if (!name) return;
  playersCall(async () => { const v = await post(sid() + "/allowlist", { name }); $("allow-name").value = ""; return v; });
});
$("op-form").addEventListener("submit", (ev) => {
  ev.preventDefault();
  const name = typedName("op-kind", "op-name");
  if (!name) return;
  playersCall(async () => { const v = await post(sid() + "/operators", { name }); $("op-name").value = ""; return v; });
});
$("say-form").addEventListener("submit", (ev) => {
  ev.preventDefault();
  const text = $("say-text").value.trim();
  if (!text) return;
  playersCall(async () => { const v = await post(sid() + "/message", { text }); $("say-text").value = ""; return v; });
});
$("allow-enabled").addEventListener("change", (ev) => {
  const enabled = ev.target.checked;
  if (!enabled && !confirm("Turn the allowlist off? Anyone who can reach this server will be able to join.")) {
    ev.target.checked = true;
    return;
  }
  const id = selected;
  post(sid() + "/allowlist-enabled", { enabled }).then((v) => {
    if (id === selected && v) { showPlayersError(null); renderPlayers(v); }
  }).catch((err) => {
    if (id !== selected) return;
    ev.target.checked = !enabled;
    alert(err.message);
  });
});

// ---- settings tab ----

let settingsView = null; // last loaded settings
let settingsFor = null;  // server ID settingsView and edits belong to
let rawFor = null;       // server ID the raw editor text belongs to
let savedFor = null;     // server ID the "Restart now" notice belongs to
const edits = {};        // key -> new value, for unsaved changes

function settingsDirty() { return Object.keys(edits).length > 0; }

// resetSettings forgets everything the Settings tab holds, so nothing from
// one server can be saved to another.
function resetSettings() {
  for (const k of Object.keys(edits)) delete edits[k];
  settingsView = settingsFor = rawFor = savedFor = null;
  $("settings-groups").replaceChildren();
  $("settings-bar").hidden = true;
  $("settings-saved").hidden = true;
  $("raw-box").open = false;
  $("raw-text").value = "";
  showSettingsError(null);
}

function showSettingsError(err) {
  $("settings-error").textContent = err ? err.message : "";
  $("settings-error").hidden = !err;
}

async function loadSettings() {
  if (!selected) return;
  const id = selected;
  try {
    const v = await api(sid() + "/settings");
    if (id !== selected) return; // switched servers while loading
    settingsView = v;
    settingsFor = id;
    for (const k of Object.keys(edits)) delete edits[k];
    $("rename-name").value = settingsView.name;
    renderHold();
    renderHours(true);
    renderMemory();
    renderConsoles();
    renderSettings();
    showSettingsError(null);
  } catch (err) { showSettingsError(err); }
}

function current(f) { return f.key in edits ? edits[f.key] : f.value; }

function setEdit(f, value, redraw = true) {
  if (value === f.value) delete edits[f.key]; else edits[f.key] = value;
  if (redraw) renderSettings(); else updateSaveBar();
}

function updateSaveBar() {
  const n = Object.keys(edits).length;
  $("settings-bar").hidden = n === 0;
  $("settings-dirty").textContent = `${n} unsaved change${n === 1 ? "" : "s"}`;
}

function control(f) {
  const val = current(f);
  switch (f.type) {
    case "select": {
      const options = f.options.map((o) => el("option", { value: o.value, selected: o.value === val }, o.label));
      if (!f.options.some((o) => o.value === val)) {
        // The file has a value that isn't one of the usual choices: show it
        // as it is rather than pretending it's the first choice.
        options.unshift(el("option", { value: val, selected: true }, `${val || "(not set)"} (as in the file)`));
      }
      return el("select", { "aria-label": f.label, onchange: (e) => setEdit(f, e.target.value) }, ...options);
    }
    case "bool": {
      const odd = val !== "true" && val !== "false";
      return el("label", { class: "switch" },
        el("input", { type: "checkbox", checked: val.toLowerCase() === "true", onchange: (e) => setEdit(f, e.target.checked ? "true" : "false") }),
        el("span", {}, odd ? `${val || "(not set)"} (as in the file)` : (val === "true" ? "On" : "Off")));
    }
    case "int":
      return el("input", { type: "number", min: f.min, max: f.max, value: val, "aria-label": f.label,
        oninput: (e) => setEdit(f, String(e.target.value).trim(), false),
        onchange: (e) => setEdit(f, String(e.target.value).trim()) });
    case "text":
      return el("input", { type: "text", maxlength: f.max || 64, value: val, "aria-label": f.label,
        oninput: (e) => setEdit(f, e.target.value, false),
        onchange: (e) => setEdit(f, e.target.value) });
    default: {
      let shown = val || "(not set)";
      if (val === "true" || val === "false") shown = val === "true" ? "On" : "Off";
      if (f.key === "transport") shown = { nethernet: "NetherNet", raknet: "RakNet" }[val] || shown;
      return el("span", { class: "ro" }, shown);
    }
  }
}

function renderSettings() {
  const v = settingsView;
  if (!v) return;
  const srv = serverList.find((x) => x.id === selected);
  const hytale = !!srv && srv.type === "hytale";
  $("raw-file").textContent = hytale ? "config.json" : "server.properties";
  $("raw-text").setAttribute("aria-label", hytale ? "config.json" : "server.properties");
  $("raw-help").textContent = hytale
    ? "Hytale's own settings file (JSON). Hytale writes it while it runs, so stop the server to change settings here or above."
    : "For settings that aren't above. The port and LAN settings are put back as the panel needs them. Changes apply after a restart.";
  const cards = v.groups.map((g) => el("div", { class: "card" },
    el("h3", {}, g.title),
    ...g.fields.map((f) => {
      const dirty = f.key in edits;
      const warn = dirty && f.warning && (f.warnWhen === "*" || f.warnWhen === edits[f.key]);
      return el("div", { class: "field" + (dirty ? " dirty" : "") },
        el("div", { class: "top" }, el("span", { class: "label" }, f.label), control(f)),
        f.help ? el("div", { class: "help" }, f.help) : null,
        f.link === "players" ? el("div", { class: "help" }, el("a", { href: serverHref(selected, "players") }, "Open the Players tab")) : null,
        dirty ? el("div", { class: "changed" }, `Changed from ${f.value || "(not set)"}`) : null,
        warn ? el("div", { class: "warn" }, "⚠ " + f.warning) : null);
    })));
  // Two columns on wide screens, both starting at the same top line: the
  // first groups on the left and the rest on the right, split where the
  // columns come out closest in length (so the order on a phone is the same).
  const size = v.groups.map((g) => 2 + g.fields.length);
  const total = size.reduce((a, b) => a + b, 0);
  let split = cards.length, best = Infinity, run = 0;
  for (let i = 0; i < cards.length; i++) {
    run += size[i];
    const diff = Math.abs(total - 2 * run);
    if (i < cards.length - 1 && diff < best) { best = diff; split = i + 1; }
  }
  $("settings-groups").replaceChildren(el("div", { class: "col" }, ...cards.slice(0, split)), el("div", { class: "col" }, ...cards.slice(split)));
  updateSaveBar();
}

$("settings-discard").addEventListener("click", () => {
  for (const k of Object.keys(edits)) delete edits[k];
  renderSettings();
});

$("settings-form").addEventListener("submit", async (ev) => {
  ev.preventDefault();
  if (!settingsDirty()) return;
  if (settingsFor !== selected) { resetSettings(); loadSettings(); return; }
  const id = settingsFor;
  try {
    const res = await post(`/api/servers/${encodeURIComponent(id)}/settings`, { values: { ...edits } });
    if (id !== selected) return;
    savedFor = id;
    settingsView = res.settings;
    for (const k of Object.keys(edits)) delete edits[k];
    renderSettings();
    showSettingsError(null);
    const n = res.changes.length;
    const running = settingsView.running;
    $("settings-saved-text").textContent = n === 0 ? "Nothing changed."
      : `Saved ${n} setting${n === 1 ? "" : "s"}.` + (running ? " Restart the server to apply them. Players will be disconnected briefly." : " They'll apply when the server starts.");
    $("settings-restart").hidden = !(running && n > 0);
    $("settings-saved").hidden = false;
    $("settings-saved").scrollIntoView({ behavior: "smooth", block: "nearest" });
  } catch (err) { showSettingsError(err); }
});

$("settings-saved-close").addEventListener("click", () => { $("settings-saved").hidden = true; });
$("settings-restart").addEventListener("click", async () => {
  $("settings-saved").hidden = true;
  if (!savedFor || savedFor !== selected) return;
  try { await post(`/api/servers/${encodeURIComponent(savedFor)}/restart`); } catch (err) { showSettingsError(err); }
  refresh();
});

$("rename-form").addEventListener("submit", async (ev) => {
  ev.preventDefault();
  try {
    await post(sid() + "/rename", { name: $("rename-name").value });
    showSettingsError(null);
    refresh();
  } catch (err) { showSettingsError(err); }
});

async function loadRaw() {
  const id = selected;
  try {
    const res = await api(sid() + "/properties");
    if (id !== selected) return;
    $("raw-text").value = res.text;
    rawFor = id;
  } catch (err) { showSettingsError(err); }
}
$("raw-box").addEventListener("toggle", () => { if ($("raw-box").open) loadRaw(); });
$("raw-reload").addEventListener("click", loadRaw);
$("raw-save").addEventListener("click", async () => {
  if (!rawFor || rawFor !== selected) { loadRaw(); return; } // text belongs to another server
  if (settingsDirty() && !confirm("You have unsaved changes above. Saving the file discards them. Continue?")) return;
  const id = rawFor;
  try {
    const res = await api(`/api/servers/${encodeURIComponent(id)}/properties`, { method: "PUT", body: JSON.stringify({ text: $("raw-text").value }) });
    if (id !== selected) return;
    $("raw-text").value = res.text;
    await loadSettings();
    savedFor = id;
    const running = settingsView && settingsView.running;
    $("settings-saved-text").textContent = $("raw-file").textContent + " saved." + (running ? " Restart the server to apply it." : "");
    $("settings-restart").hidden = !running;
    $("settings-saved").hidden = false;
  } catch (err) { showSettingsError(err); }
});

window.addEventListener("beforeunload", (ev) => { if (settingsDirty() || notifyDirty) { ev.preventDefault(); ev.returnValue = ""; } });

// ---- console tab ----

function lineClass(line) {
  if (line.startsWith("[Panel]")) return "panel";
  if (line.startsWith("> ")) return "cmd";
  if (/\bERROR\b/.test(line)) return "err";
  if (/\bWARN\b/.test(line)) return "warn";
  return "";
}

function appendLine(text) {
  const log = $("console-log");
  const atBottom = log.scrollHeight - log.scrollTop - log.clientHeight < 40;
  log.append(el("span", { class: lineClass(text) }, text + "\n"));
  while (log.childNodes.length > 2000) log.firstChild.remove();
  if (atBottom) log.scrollTop = log.scrollHeight;
}

function openConsole() {
  if (consoleStream) return;
  $("console-log").replaceChildren();
  $("console-error").hidden = true;
  consoleStream = new EventSource(sid() + "/console");
  consoleStream.onmessage = (ev) => appendLine(ev.data);
  $("console-input").focus();
}

function closeConsole() {
  if (consoleStream) consoleStream.close();
  consoleStream = null;
}

$("console-form").addEventListener("submit", async (ev) => {
  ev.preventDefault();
  const input = $("console-input");
  const command = input.value.trim();
  if (!command || !selected) return;
  try {
    await post(sid() + "/command", { command });
    input.value = "";
    $("console-error").hidden = true;
  } catch (err) {
    $("console-error").textContent = err.message;
    $("console-error").hidden = false;
  }
});

// ---- add server ----

$("add-form").addEventListener("submit", async (ev) => {
  ev.preventDefault();
  $("add-error").hidden = true;
  try {
    const java = editionChoice() === "java";
    const hytale = editionChoice() === "hytale";
    const s = await post("/api/servers", hytale ? {
      name: $("add-name").value, edition: "hytale",
      memoryMB: Number($("add-memory").value), acceptEula: $("add-eula").checked,
    } : java ? {
      name: $("add-name").value, edition: "java", javaName: $("add-java").value.trim(),
      memoryMB: Number($("add-memory").value), acceptEula: $("add-eula").checked,
    } : {
      name: $("add-name").value,
      ownerGamertag: $("add-owner").value.trim(),
      acceptEula: $("add-eula").checked,
    });
    if (me.owner && !java && !hytale) settings.ownerGamertag = $("add-owner").value.trim();
    if (me.owner && java && $("add-java").value.trim()) settings.ownerJavaName = $("add-java").value.trim();
    $("add-form").reset();
    await refresh();
    location.hash = serverHref(s.id, "console");
  } catch (err) {
    $("add-error").textContent = err.message;
    $("add-error").hidden = false;
  }
});

// ---- backups tab ----

let backupsFor = null; // server ID the plan form belongs to
let planDirty = false;
const kindLabels = {
  scheduled: "Automatic", manual: "Made by hand",
  "before-update": "Before an update", "before-restore": "Before a restore", "before-delete": "Before deleting a world",
  final: "When the server was deleted",
};

function resetBackups() {
  backupsFor = null;
  planDirty = false;
  // Nothing from another server can be saved here until this one's plan loads.
  for (const id of ["plan-every", "plan-at", "plan-keep"]) $(id).disabled = true;
  $("backups").replaceChildren();
  $("backups-summary").textContent = "";
  $("plan-save").disabled = true;
  showBackupsError(null);
}

function showBackupsError(err) {
  $("backups-error").textContent = err ? err.message : "";
  $("backups-error").hidden = !err;
}

function size(bytes) {
  if (bytes >= 1e9) return (bytes / 1e9).toFixed(1) + " GB";
  if (bytes >= 1e6) return (bytes / 1e6).toFixed(1) + " MB";
  return Math.max(1, Math.round(bytes / 1e3)) + " KB";
}

async function loadBackups() {
  if (!selected) return;
  const id = selected;
  try {
    const v = await api(sid() + "/backups");
    if (id !== selected) return;
    renderBackups(v, id);
    showBackupsError(null);
  } catch (err) { showBackupsError(err); }
}

async function backupsCall(fn) {
  const id = selected;
  try {
    const v = await fn();
    if (id !== selected) return;
    showBackupsError(null);
    if (v && v.backups) renderBackups(v, id);
    else loadBackups();
  } catch (err) { showBackupsError(err); }
  refresh();
}

function renderBackups(v, id) {
  if (backupsFor !== id || !planDirty) {
    $("plan-every").value = v.plan.every;
    $("plan-at").value = v.plan.at || "04:00";
    $("plan-keep").value = v.plan.keep;
    planDirty = false;
    $("plan-save").disabled = true;
  }
  backupsFor = id;
  const care = access(serverList.find((x) => x.id === id)) >= CARE;
  $("plan-form").hidden = !care;
  for (const x of ["plan-every", "plan-at"]) $(x).disabled = false;
  updatePlanForm();
  const utc = /^(UTC|GMT|Etc\/UTC)$/.test(v.timeZone);
  $("plan-help").replaceChildren(
    "A server nobody has played on since its last backup is skipped, so the kept backups aren't all the same. Backups you make by hand stay until you delete them; the panel also keeps the last 5 from before updates and 3 from before restores. ",
    `Times use the panel's clock: ${v.timeZone}, now ${v.now}.`,
    ...(utc ? [el("span", { class: "warn" }, " To use your own time zone, set TZ (Unraid does this itself; with docker run, add -e TZ=America/Denver or yours).")] : []));
  $("backups-last-error").textContent = v.lastError || "";
  $("backups-last-error").hidden = !v.lastError;
  $("backup-now").disabled = v.running;
  const list = v.backups || [];
  $("backups-summary").textContent = v.running
    ? "Working on it…"
    : (list.length ? `${list.length} backup${list.length === 1 ? "" : "s"}, ${size(v.total)} in all. ` + (care ? "Restoring backs up the current world first, so you can undo it." : `To restore one, ask ${ownerWord()}.`) : "");
  const enc = encodeURIComponent;
  $("backups").replaceChildren(...(list.length ? list.map((b) => {
    const when = new Date(b.time).toLocaleString([], { dateStyle: "medium", timeStyle: "short" });
    const detail = `${kindLabels[b.kind] || b.kind} · ${size(b.size)}${b.full ? " · includes the server version from before the update" : ""}`;
    if (!care) return row(when, detail);
    return row(when, detail,
      el("a", { class: "button", href: `${sid()}/backups/${enc(b.name)}`, download: b.name }, "Download"),
      btn("Restore", () => {
        if (b.kind === "before-delete") {
          if (confirm(`Put back the world that was deleted on ${when}? It's added next to the other worlds; nothing else changes.`)) {
            backupsCall(() => post(`${sid()}/backups/${enc(b.name)}/restore`));
          }
          return;
        }
        const extra = b.full ? " This also puts back the server version from before that update (press Update to go forward again)." : "";
        if (confirm(`Put the world back as it was on ${when}?\n\nThe server stops, the current world is backed up first (so you can undo this), and it starts again if it was running.${extra}`)) {
          backupsCall(() => post(`${sid()}/backups/${enc(b.name)}/restore`));
        }
      }, "", v.running),
      btn("Delete", () => {
        if (confirm(`Delete the backup from ${when}? This can't be undone.`)) {
          backupsCall(() => del(`${sid()}/backups/${enc(b.name)}`));
        }
      }, "danger", v.running));
  }) : [emptyRow(v.running ? "Making the first backup…" : "No backups yet.")]));
}

function updatePlanForm() {
  const every = $("plan-every").value;
  $("plan-at-label").hidden = every !== "daily";
  $("plan-keep").disabled = every === "off";
}

for (const id of ["plan-every", "plan-at", "plan-keep"]) {
  $(id).addEventListener("input", () => { planDirty = true; $("plan-save").disabled = false; updatePlanForm(); });
}
$("plan-form").addEventListener("submit", (ev) => {
  ev.preventDefault();
  if (backupsFor !== selected) return;
  const plan = { every: $("plan-every").value, at: $("plan-at").value, keep: Number($("plan-keep").value) || 7 };
  const id = selected;
  backupsCall(async () => {
    const v = await api(sid() + "/backups/plan", { method: "PUT", body: JSON.stringify(plan) });
    if (id === selected) {
      planDirty = false;
      backupsFor = null; // take the saved plan
    }
    return v;
  });
});
$("backup-now").addEventListener("click", () => backupsCall(() => post(sid() + "/backups")));

// ---- play time ----

// fmtMinutes: 80 → "1 h 20 min".
function fmtMinutes(n) {
  n = Math.max(0, Math.round(n || 0));
  const h = Math.floor(n / 60), m = n % 60;
  if (!h) return `${m} min`;
  return m ? `${h} h ${m} min` : `${h} h`;
}

// fmtClock: "21:00" or a date → "9:00 PM", as this device writes times.
function fmtClock(v) {
  const d = typeof v === "string" && /^\d\d:\d\d$/.test(v) ? new Date(`2000-01-01T${v}:00`) : new Date(v);
  return d.toLocaleTimeString([], { hour: "numeric", minute: "2-digit" });
}

// fmtWhen: when a time is, from now ("9:00 PM", "tomorrow 7:00 AM").
function fmtWhen(iso) {
  const d = new Date(iso), now = new Date();
  const days = Math.round((new Date(d.toDateString()) - new Date(now.toDateString())) / 864e5);
  if (days <= 0) return fmtClock(d);
  if (days === 1) return "tomorrow " + fmtClock(d);
  return d.toLocaleDateString([], { weekday: "long" }) + " " + fmtClock(d);
}

// hoursNote is a server's hours in a few words.
function hoursNote(h) {
  if (h.open) return h.late ? `Open late, until ${fmtWhen(h.closes)}` : `Open until ${fmtWhen(h.closes)}`;
  return `Closed for the night, opens ${fmtWhen(h.opens)}`;
}

function spanText(sp) { return `${fmtClock(sp.from)} to ${fmtClock(sp.until)}`; }

// A server's open hours (Settings tab, owner only).
let hoursDirty = false;
function renderHours(fresh) {
  const s = serverList.find((x) => x.id === selected);
  $("hours-card").hidden = !me.owner || !s;
  if (!s || !me.owner) return;
  if (fresh) { hoursDirty = false; $("hours-error").hidden = true; }
  const h = s.hours;
  if (!hoursDirty) {
    const set = h || hoursDefaults;
    $("hours-on").checked = !!h;
    $("hours-school-from").value = set.school.from;
    $("hours-school-until").value = set.school.until;
    $("hours-weekend-from").value = set.weekend.from;
    $("hours-weekend-until").value = set.weekend.until;
  }
  $("hours-times").classList.toggle("off", !$("hours-on").checked);
  for (const id of ["hours-school-from", "hours-school-until", "hours-weekend-from", "hours-weekend-until"]) $(id).disabled = !$("hours-on").checked;
  $("hours-save").hidden = !hoursDirty;
  $("hours-now").hidden = !h || hoursDirty;
  if (h) {
    $("hours-status").replaceChildren(el("b", {}, hoursNote(h) + "."),
      h.open ? "" : " You can still start it yourself: it then stays open until it would open anyway.");
    $("hours-late-end").hidden = !h.late;
    if (!$("hours-late").value) $("hours-late").value = h.late ? new Date(h.late).toTimeString().slice(0, 5) : "";
  }
}
let hoursDefaults = { school: { from: "07:00", until: "21:00" }, weekend: { from: "08:00", until: "22:00" } };

function hoursChanged() {
  hoursDirty = true;
  renderHours();
}
$("hours-on").addEventListener("change", hoursChanged);
for (const id of ["hours-school-from", "hours-school-until", "hours-weekend-from", "hours-weekend-until"]) $(id).addEventListener("input", hoursChanged);

async function hoursCall(fn) {
  const id = selected;
  $("hours-error").hidden = true;
  try {
    await fn();
    hoursDirty = false;
    $("hours-late").value = "";
    await refresh();
    if (id === selected) renderHours();
  } catch (err) {
    if (id !== selected) return;
    $("hours-error").textContent = err.message;
    $("hours-error").hidden = false;
  }
}
$("hours-save").addEventListener("click", () => hoursCall(() => post(sid() + "/hours", {
  on: $("hours-on").checked,
  school: { from: $("hours-school-from").value, until: $("hours-school-until").value },
  weekend: { from: $("hours-weekend-from").value, until: $("hours-weekend-until").value },
})));
$("hours-late-save").addEventListener("click", () => {
  if (!$("hours-late").value) { $("hours-late").focus(); return; }
  hoursCall(() => post(sid() + "/open-late", { until: $("hours-late").value }));
});
$("hours-late-end").addEventListener("click", () => hoursCall(() => post(sid() + "/open-late", { until: "" })));

// The Play time page (owner only).
let playtime = null;
let kidEditing = null; // the kid being changed, "" for a new one, null when the form is closed

function showPlaytimeError(err) {
  $("playtime-error").textContent = err ? err.message : "";
  $("playtime-error").hidden = !err;
}

async function loadPlaytime() {
  try {
    const v = await api("/api/playtime");
    showPlaytimeError(null);
    renderPlaytime(v);
  } catch (err) { showPlaytimeError(err); }
}

function weekChart(week) {
  const max = Math.max(60, ...week.map((d) => d.minutes));
  const today = week.length - 1;
  return el("div", { class: "week", role: "img", "aria-label": "Minutes played each day this week: " + week.map((d) => `${new Date(d.day + "T12:00").toLocaleDateString([], { weekday: "short" })} ${fmtMinutes(d.minutes)}`).join(", ") },
    ...week.map((d, i) => el("div", { class: "day" + (i === today ? " today" : ""), title: fmtMinutes(d.minutes) },
      el("div", { class: "area" },
        el("small", {}, d.minutes ? fmtMinutes(d.minutes).replace(" min", "m").replace(" h", "h") : ""),
        el("div", { class: "bar" + (d.minutes ? "" : " none"), style: `height: ${Math.max(2, Math.round(d.minutes / max * 50))}px` })),
      el("small", {}, i === today ? "Today" : new Date(d.day + "T12:00").toLocaleDateString([], { weekday: "short" })))));
}

function renderPlaytime(v) {
  playtime = v;
  hoursDefaults = v.defaultHours;
  const kids = v.kids || [];
  const cards = kids.map((k) => {
    const limited = k.limit.on || k.hours.on;
    const rules = [];
    if (k.limit.on) rules.push(`Daily limit ${fmtMinutes(k.limit.school)} on school days, ${fmtMinutes(k.limit.weekend)} at weekends`);
    if (k.hours.on) rules.push(`Plays ${spanText(k.hours.school)} Sunday to Thursday, ${spanText(k.hours.weekend)} Friday and Saturday`);
    let left = null;
    if (k.left != null) {
      left = k.left > 0
        ? el("span", {}, " · ", el("b", {}, fmtMinutes(k.left)), k.why === "bedtime" ? " until bedtime" : " left")
        : el("span", { class: "warn" }, k.why === "bedtime" ? " · outside their play hours" : " · no time left today");
    }
    const extra = (n) => btn(`+${n >= 60 ? n / 60 + " h" : n + " min"}`, () => kidCall(() => post("/api/playtime/kids/extra", { name: k.name, minutes: n })));
    return el("section", { class: "card kid" },
      el("h2", {}, k.name, k.playing.length ? el("span", { class: "badge outside" }, "Playing on " + k.playing.join(", ")) : null),
      el("p", { class: "names" }, k.players.length ? "Plays as " + k.players.join(", ") : "No names yet: edit to add the names they play as."),
      el("p", { class: "today" }, "Today ", el("b", {}, fmtMinutes(k.today)), " played", left,
        k.extraToday ? el("span", { class: "muted" }, ` (with ${fmtMinutes(k.extraToday)} extra)`) : null),
      weekChart(k.week || []),
      el("p", { class: "rules" }, rules.length ? rules.join(". ") + "." : "No limits: they can play whenever their servers are open."),
      el("div", { class: "row" },
        ...(limited ? [el("span", { class: "muted small" }, "More time today:"), extra(15), extra(30), extra(60)] : []),
        k.extraToday ? btn("Take extra back", () => kidCall(() => post("/api/playtime/kids/extra", { name: k.name, minutes: 0 }))) : null,
        btn("Edit", () => openKidForm(k))));
  });
  $("kids").replaceChildren(...(cards.length ? [el("div", { class: "col" }, ...cards.filter((_, i) => i % 2 === 0)), el("div", { class: "col" }, ...cards.filter((_, i) => i % 2 === 1))]
    : [el("div", { class: "col" }, el("p", { class: "card muted" }, "No kids yet. Add one to see how long they play, and to set a daily limit or bedtime if you want."))]));
  $("others").replaceChildren(...((v.others || []).length
    ? v.others.map((p) => row(playerLabel(p.name), `${fmtMinutes(p.minutes)} on ${(p.servers || []).join(", ")}`,
      ...(kids.length ? [addToKidSelect(p.name, kids)] : [])))
    : [emptyRow("Nobody else has played this week.")]));
  $("server-hours").replaceChildren(...((v.servers || []).length
    ? v.servers.map((x) => row(x.name, `${spanText(x.hours.school)} Sunday to Thursday, ${spanText(x.hours.weekend)} Friday and Saturday. ${hoursNote(x.hours)}.`,
      el("a", { class: "button", href: serverHref(x.id, "settings") }, "Change")))
    : [emptyRow("No server has open hours: they're open any time.")]));
}

function addToKidSelect(name, kids) {
  const sel = el("select", { "aria-label": `Add ${name} to a kid`, onchange: () => {
    const k = kids.find((x) => x.name === sel.value);
    sel.value = "";
    if (k) kidCall(() => post("/api/playtime/kids", { old: k.name, kid: { ...k, players: [...k.players, name] } }));
  } }, el("option", { value: "" }, "Add to…"), ...kids.map((k) => el("option", { value: k.name }, k.name)));
  return sel;
}

async function kidCall(fn) {
  try {
    const v = await fn();
    showPlaytimeError(null);
    if (v) renderPlaytime(v);
    return true;
  } catch (err) { showPlaytimeError(err); return false; }
}

function kidFormState() {
  const on = (box, ids) => { for (const id of ids) $(id).disabled = !$(box).checked; };
  on("kid-limit-on", ["kid-limit-school", "kid-limit-weekend"]);
  on("kid-hours-on", ["kid-school-from", "kid-school-until", "kid-weekend-from", "kid-weekend-until"]);
  $("kid-limit-times").classList.toggle("off", !$("kid-limit-on").checked);
  $("kid-hours-times").classList.toggle("off", !$("kid-hours-on").checked);
}
$("kid-limit-on").addEventListener("change", kidFormState);
$("kid-hours-on").addEventListener("change", kidFormState);

function openKidForm(k) {
  const d = playtime || { defaultHours: hoursDefaults, defaultLimit: { school: 120, weekend: 180 } };
  kidEditing = k ? k.name : "";
  $("kid-form-title").textContent = k ? `Change ${k.name}` : "Add a kid";
  $("kid-name").value = k ? k.name : "";
  $("kid-players").value = k ? k.players.join("\n") : "";
  const limit = k ? k.limit : { on: false, ...d.defaultLimit };
  const hours = k ? k.hours : { on: false, ...d.defaultHours };
  $("kid-limit-on").checked = !!limit.on;
  $("kid-limit-school").value = limit.school;
  $("kid-limit-weekend").value = limit.weekend;
  $("kid-hours-on").checked = !!hours.on;
  $("kid-school-from").value = hours.school.from;
  $("kid-school-until").value = hours.school.until;
  $("kid-weekend-from").value = hours.weekend.from;
  $("kid-weekend-until").value = hours.weekend.until;
  $("kid-delete").hidden = !k;
  $("kid-error").hidden = true;
  kidFormState();
  $("kid-form").hidden = false;
  $("kid-add").hidden = true;
  $("kid-form").scrollIntoView({ behavior: "smooth", block: "start" });
  $("kid-name").focus({ preventScroll: true });
}

function closeKidForm() {
  kidEditing = null;
  $("kid-form").hidden = true;
  $("kid-add").hidden = false;
}

$("kid-add").addEventListener("click", () => openKidForm(null));
$("kid-cancel").addEventListener("click", closeKidForm);
$("kid-delete").addEventListener("click", async () => {
  const name = kidEditing;
  if (!name || !confirm(`Take ${name} off Play time? Their limits and bedtime stop, and their time is no longer counted under their name.`)) return;
  if (await kidCall(() => post("/api/playtime/kids/delete", { name }))) closeKidForm();
});
$("kid-form").addEventListener("submit", async (ev) => {
  ev.preventDefault();
  const kid = {
    name: $("kid-name").value,
    players: $("kid-players").value.split(/[\n,]/).map((x) => x.trim()).filter(Boolean),
    limit: { on: $("kid-limit-on").checked, school: Number($("kid-limit-school").value) || 0, weekend: Number($("kid-limit-weekend").value) || 0 },
    hours: { on: $("kid-hours-on").checked,
      school: { from: $("kid-school-from").value, until: $("kid-school-until").value },
      weekend: { from: $("kid-weekend-from").value, until: $("kid-weekend-until").value } },
  };
  try {
    renderPlaytime(await post("/api/playtime/kids", { old: kidEditing || "", kid }));
    showPlaytimeError(null);
    closeKidForm();
  } catch (err) {
    $("kid-error").textContent = err.message;
    $("kid-error").hidden = false;
  }
});

// A kid sees their own time left on the Servers page, when the owner has
// put them on Play time under their account's name.
async function loadMyTime() {
  if (!signedIn || me.owner) { $("my-time-banner").hidden = true; return; }
  let v = {};
  try { v = await api("/api/me/playtime"); } catch (_) { /* leave it */ }
  if (v.left == null) { $("my-time-banner").hidden = true; return; }
  $("my-time-text").replaceChildren(...(v.left > 0
    ? [el("b", {}, fmtMinutes(v.left)), v.why === "bedtime" ? " until bedtime." : " of play left today.", ` You've played ${fmtMinutes(v.today)} today.`]
    : [el("b", {}, v.why === "bedtime" ? "It's outside your play hours." : "That's all your play time for today."), " See you tomorrow!"]));
  $("my-time-banner").hidden = false;
}

// ---- keeping a version, deleting a server ----

function renderHold() {
  const s = serverList.find((x) => x.id === selected);
  $("hold-card").hidden = access(s) < CARE;
  if (!s) return;
  $("hold-version").checked = !!s.holdVersion;
  const game = s.type === "java" ? "Minecraft Java" : s.type === "hytale" ? "Hytale" : "Bedrock";
  $("hold-label").textContent = `Keep this server on its ${game} version (overnight updates skip it)`;
  const extra = s.type === "java" && s.consoles ? " Console support (Geyser) is still kept up to date." : "";
  $("hold-help").textContent = s.preview
    ? "This is a preview server, which overnight updates always skip."
    : (s.holdVersion ? `It stays on ${s.version ? game + " " + s.version : "its current version"} until you press Update or untick this.${extra}` : `When overnight updates are on, it's updated to each new ${game} version while nobody is playing.`);
}

$("hold-version").addEventListener("change", async (ev) => {
  const box = ev.currentTarget;
  const id = selected;
  try {
    await post(`/api/servers/${encodeURIComponent(id)}/hold-version`, { hold: box.checked });
    await refresh();
    if (id === selected) renderHold();
  } catch (err) { box.checked = !box.checked; showSettingsError(err); }
});

$("delete-server").addEventListener("click", async (ev) => {
  const b = ev.currentTarget;
  const s = serverList.find((x) => x.id === selected);
  if (!s) return;
  const typed = prompt(`Delete ${s.name}? It's stopped, a final backup is made, and it's removed from the panel. You can bring it back from the Backups page.\n\nType the server's name to confirm:`);
  if (typed === null) return;
  if (typed.trim().toLowerCase() !== s.name.trim().toLowerCase()) { alert("That isn't the server's name, so nothing was deleted."); return; }
  b.disabled = true;
  b.textContent = "Stopping and backing up…";
  try {
    await del(`/api/servers/${encodeURIComponent(s.id)}`);
    resetSettings();
    location.hash = "#/backups";
  } catch (err) { showSettingsError(err); }
  b.disabled = false;
  b.textContent = "Delete server…";
  refresh();
});

// ---- deleted servers (Backups page, owner) ----

async function loadDeleted() {
  try {
    const list = await api("/api/deleted-servers");
    if (view !== "backups") return;
    $("deleted-box").hidden = list.length === 0;
    $("deleted-list").replaceChildren(...list.map((d) => row(d.name,
      `Deleted ${whenShort(d.deleted).toLowerCase()} · ${d.backups} backup${d.backups === 1 ? "" : "s"}, ${mb1(d.size)} · port ${d.port}`,
      d.canBringBack ? btn("Bring back", async (ev) => {
        const b = ev.currentTarget;
        if (!confirm(`Bring back ${d.name}? The server is downloaded again and its world and settings are put back. It stays stopped until you start it.`)) return;
        b.disabled = true;
        try {
          const st = await post(`/api/deleted-servers/${encodeURIComponent(d.id)}/bring-back`);
          await refresh();
          location.hash = serverHref(st.id, "console");
        } catch (err) { showDeletedError(err); b.disabled = false; }
      }, "primary") : null,
      btn("Remove backups", async () => {
        if (!confirm(`Remove all of ${d.name}'s backups for good? It can't be brought back after this.`)) return;
        try { await del(`/api/deleted-servers/${encodeURIComponent(d.id)}`); showDeletedError(null); } catch (err) { showDeletedError(err); }
        loadDeleted();
      }, "danger"))));
    showDeletedError(null);
  } catch (err) { showDeletedError(err); }
}

function showDeletedError(err) {
  $("deleted-error").textContent = err ? err.message : "";
  $("deleted-error").hidden = !err;
}

// ---- overnight upkeep (owner) ----

async function loadUpkeep() {
  try {
    const n = await api("/api/nightly");
    if (view !== "upkeep") return;
    $("upkeep-update").checked = n.autoUpdate;
    $("upkeep-restart").checked = n.dailyRestart;
    $("upkeep-at").value = n.at;
    $("upkeep-saved").hidden = true;
    const zone = Intl.DateTimeFormat().resolvedOptions().timeZone;
    $("upkeep-zone").textContent = `The panel's clock is used (see the Backups tab for its time zone; this browser is on ${zone}). The window lasts three hours.`;
    $("upkeep-error").hidden = true;
  } catch (err) { $("upkeep-error").textContent = err.message; $("upkeep-error").hidden = false; }
}

$("upkeep-form").addEventListener("submit", async (ev) => {
  ev.preventDefault();
  try {
    await api("/api/nightly", { method: "PUT", body: JSON.stringify({
      autoUpdate: $("upkeep-update").checked, dailyRestart: $("upkeep-restart").checked, at: $("upkeep-at").value }) });
    $("upkeep-saved").hidden = false;
    $("upkeep-error").hidden = true;
    nightlyOn = null;
  notifyView = null;
  notifyDirty = false;
  for (const k of Object.keys(notifyClear)) delete notifyClear[k];
  $("notify-cards").replaceChildren();
  } catch (err) { $("upkeep-error").textContent = err.message; $("upkeep-error").hidden = false; }
});
for (const id of ["upkeep-update", "upkeep-restart", "upkeep-at"]) $(id).addEventListener("input", () => { $("upkeep-saved").hidden = true; });

// nightlyOn: whether overnight updates are on (owner), for the update banner.
let nightlyOn = null;
async function checkNightly() {
  if (!me.owner || nightlyOn !== null) return;
  try { nightlyOn = (await api("/api/nightly")).autoUpdate; } catch (_) { return; }
  $("update-auto-link").hidden = nightlyOn;
}

// ---- notifications (owner) ----

let notifyView = null;   // last loaded settings (without secrets)
let notifyDirty = false;
const notifyClear = {};  // secrets to remove on save

const channelInfo = {
  ntfy: { title: "Phone (ntfy)", blurb: "Push notifications through the free ntfy app (Android and iPhone), no account needed." },
  discord: { title: "Discord", blurb: "Messages in a channel of your Discord server." },
  email: { title: "Email", blurb: "Through an email account's SMTP server. For Gmail, make an app password (Google Account, Security) and use smtp.gmail.com, port 587." },
  sms: { title: "Text messages (Twilio)", blurb: "Texts through Twilio Programmable Messaging. Each text costs a little, so by default only crashes and failed backups are texted, at most 20 a day." },
};

function markNotifyDirty() {
  notifyDirty = true;
  $("notify-dirty").hidden = false;
}

async function loadNotify() {
  try {
    const v = await api("/api/notifications");
    if (view !== "notify") return;
    notifyView = v;
    notifyDirty = false;
    for (const k of Object.keys(notifyClear)) delete notifyClear[k];
    renderNotify();
    $("notify-error").hidden = true;
  } catch (err) { $("notify-error").textContent = err.message; $("notify-error").hidden = false; }
}

function nField(label, input, wide) {
  return el("label", { class: wide ? "wide" : "" }, label, input);
}

function nInput(obj, key, attrs = {}) {
  const i = el("input", { value: obj[key] == null ? "" : String(obj[key]), autocomplete: "off", autocapitalize: "off", spellcheck: "false", ...attrs });
  i.addEventListener("input", () => {
    obj[key] = attrs.type === "number" ? Number(i.value) : i.value;
    markNotifyDirty();
  });
  return i;
}

function nList(obj, key, attrs = {}) {
  const i = el("input", { value: (obj[key] || []).join(", "), autocomplete: "off", autocapitalize: "off", spellcheck: "false", ...attrs });
  i.addEventListener("input", () => {
    obj[key] = i.value.split(/[,;\n]/).map((x) => x.trim()).filter(Boolean);
    markNotifyDirty();
  });
  return i;
}

// nSecret is a password-like field: empty keeps what's saved.
function nSecret(ch, obj, key, name, label, placeholder) {
  const saved = notifyView.secrets[name] && !notifyClear[name];
  const i = el("input", { type: "password", autocomplete: "new-password", placeholder: saved ? "Saved; type to replace it" : placeholder });
  i.addEventListener("input", () => { obj[key] = i.value; markNotifyDirty(); });
  const note = saved ? el("span", { class: "secret-note" }, "Saved. ",
    el("a", { href: "#", onclick: (ev) => {
      ev.preventDefault();
      notifyClear[name] = true;
      obj[key] = "";
      markNotifyDirty();
      ev.target.closest(".notify-card").replaceWith(notifyCard(ch)); // just this card
    } }, "Remove it")) : null;
  return el("label", { class: "wide" }, label, i, note);
}

function notifyCard(ch) {
  const c = notifyView[ch];
  const info = channelInfo[ch];
  const on = el("input", { type: "checkbox", checked: c.on });
  on.addEventListener("change", () => { c.on = on.checked; markNotifyDirty(); card.classList.toggle("off", !c.on); });
  const fields = [];
  if (ch === "ntfy") {
    const link = `${c.server}/${c.topic}`;
    fields.push(el("p", { class: "muted small wide" }, "In the ntfy app, add a subscription to the topic below (on the server below, if you changed it). Anyone who knows the topic can read the messages, so keep it hard to guess. Web: ",
      el("a", { href: link, target: "_blank", rel: "noopener" }, link)));
    fields.push(nField("Topic", nInput(c, "topic", { class: "mono", maxlength: "64" })));
    fields.push(nField("Server", nInput(c, "server", { placeholder: "https://ntfy.sh" })));
    fields.push(nSecret(ch, c, "token", "ntfy.token", "Access token (only for a private ntfy server)", "Optional"));
  } else if (ch === "discord") {
    fields.push(el("p", { class: "muted small wide" }, "In Discord: the channel's settings, Integrations, Webhooks, New Webhook, Copy Webhook URL. Paste it here."));
    fields.push(nSecret(ch, c, "webhook", "discord.webhook", "Webhook link", "https://discord.com/api/webhooks/…"));
  } else if (ch === "email") {
    fields.push(nField("SMTP server", nInput(c, "host", { placeholder: "smtp.gmail.com" })));
    fields.push(nField("Port", nInput(c, "port", { type: "number", min: "1", max: "65535", placeholder: "587" })));
    fields.push(nField("Username", nInput(c, "username", { placeholder: "Usually your email address" })));
    fields.push(nField("From", nInput(c, "from", { placeholder: "you@gmail.com", type: "email" })));
    fields.push(nSecret(ch, c, "password", "email.password", "Password (an app password for Gmail)", "Password"));
    fields.push(nField("Send to (separate several with commas)", nList(c, "to", { placeholder: "you@example.com" }), true));
  } else if (ch === "sms") {
    fields.push(el("p", { class: "muted small wide" }, "From the Twilio Console: the Account SID and Auth Token (Account Info), and a phone number you bought there or a Messaging Service SID. US numbers need A2P 10DLC registration or a verified toll-free number before texts are delivered."));
    fields.push(nField("Account SID", nInput(c, "accountSid", { class: "mono", placeholder: "AC…" })));
    fields.push(nField("From (number or MG… SID)", nInput(c, "from", { placeholder: "+1 555 123 4567" })));
    fields.push(nSecret(ch, c, "authToken", "sms.authToken", "Auth Token", "Auth Token"));
    fields.push(nField("Text these numbers (separate with commas)", nList(c, "to", { placeholder: "+1 555 987 6543" }), true));
    fields.push(nField("At most this many texts a day", nInput(c, "maxPerDay", { type: "number", min: "1", max: "500" })));
  }
  const events = notifyView.events.map((e) => {
    const box = el("input", { type: "checkbox", checked: (c.events || []).includes(e.kind) });
    box.addEventListener("change", () => {
      const set = new Set(c.events || []);
      if (box.checked) set.add(e.kind); else set.delete(e.kind);
      c.events = notifyView.events.map((x) => x.kind).filter((k) => set.has(k));
      markNotifyDirty();
    });
    return el("label", { class: "check" }, box, el("span", {}, e.label));
  });
  const test = btn("Send a test", async (ev) => {
    const b = ev.currentTarget;
    if (notifyDirty) { alert("Save first, then send a test."); return; }
    b.disabled = true;
    b.textContent = "Sending…";
    try {
      await post(`/api/notifications/test/${ch}`);
      b.textContent = "Sent: check it arrived";
    } catch (err) {
      b.textContent = "Send a test";
      alert(`${info.title}: ${err.message}`);
    }
    b.disabled = false;
    setTimeout(() => { b.textContent = "Send a test"; }, 6000);
  });
  const card = el("section", { class: "card notify-card" + (c.on ? "" : " off") },
    el("div", { class: "row between" }, el("h2", {}, info.title), el("label", { class: "switch" }, on, el("span", {}, "On"))),
    el("p", { class: "muted small" }, info.blurb),
    el("div", { class: "fields" }, ...fields),
    el("fieldset", {}, el("legend", {}, "Tell me when"), el("div", { class: "events" }, ...events)),
    el("div", { class: "row" }, test));
  return card;
}

function renderNotify() {
  $("notify-cards").replaceChildren(...["ntfy", "discord", "email", "sms"].map(notifyCard));
  $("notify-dirty").hidden = !notifyDirty;
}

$("notify-form").addEventListener("submit", async (ev) => {
  ev.preventDefault();
  if (!notifyView) return;
  const b = $("notify-save");
  b.disabled = true;
  try {
    const { ntfy, discord, email, sms } = notifyView;
    const v = await api("/api/notifications", { method: "PUT", body: JSON.stringify({ settings: { ntfy, discord, email, sms }, clear: notifyClear }) });
    notifyView = v;
    notifyDirty = false;
    for (const k of Object.keys(notifyClear)) delete notifyClear[k];
    renderNotify();
    $("notify-error").hidden = true;
    $("notify-dirty").hidden = false;
    $("notify-dirty").textContent = "Saved.";
    setTimeout(() => { if (!notifyDirty) $("notify-dirty").hidden = true; $("notify-dirty").textContent = "Unsaved changes"; }, 2500);
  } catch (err) { $("notify-error").textContent = err.message; $("notify-error").hidden = false; $("notify-error").scrollIntoView({ block: "nearest" }); }
  b.disabled = false;
});

// ---- Java servers ----

function editionChoice() {
  const r = document.querySelector("input[name=edition]:checked");
  return r ? r.value : "bedrock";
}

function showEdition() {
  const edition = editionChoice();
  const java = edition === "java", hytale = edition === "hytale";
  $("add-owner-label").hidden = java || hytale;
  $("add-java-label").hidden = !java;
  $("add-memory-label").hidden = !java && !hytale;
  $("hytale-account").hidden = !hytale;
  const mem = $("add-memory");
  if (hytale && Number(mem.value) < 4096) mem.value = "4096";
  if (java && mem.dataset.edition === "hytale") mem.value = "2048";
  mem.dataset.edition = edition;
  $("edition-help").textContent = hytale
    ? "For Hytale. The server is downloaded with your Hytale account, and needs at least 4 GB of memory. Players join with the address and port."
    : java
      ? "For Minecraft: Java Edition on a PC or Mac. Runs Paper, which is faster than Mojang's own server and takes plugins. Consoles, phones and tablets can join too (through Geyser, installed for you)."
      : "For consoles, phones, tablets and Windows: the Minecraft most kids play.";
  $("add-eula-name").textContent = hytale ? "Hytale EULA" : "Minecraft EULA";
  $("add-eula-link").href = hytale ? "https://hytale.com/eula" : "https://www.minecraft.net/eula";
  $("add-source").textContent = hytale ? "The server is downloaded from Hytale, and Java from adoptium.net."
    : java ? "Paper is downloaded from papermc.io, Java from adoptium.net, and console support from geysermc.org and hangar.papermc.io." : "The server is downloaded from Mojang.";
  const submit = document.querySelector("#add-form button[type=submit]");
  if (submit) submit.disabled = hytale && !hytaleSignedIn;
  if (hytale) loadHytaleAccount();
  if (java && !$("add-java").value) $("add-java").value = me.owner ? settings.ownerJavaName || "" : "";
}
for (const r of document.querySelectorAll("input[name=edition]")) r.addEventListener("change", showEdition);

// ---- Hytale account ----

let hytaleTimer = null;
let hytaleSignedIn = false;

async function loadHytaleAccount() {
  clearTimeout(hytaleTimer);
  try { renderHytaleAccount(await api("/api/hytale/account")); } catch (err) { $("hytale-status").textContent = err.message; }
}

function renderHytaleAccount(v) {
  hytaleSignedIn = !!v.signedIn;
  const p = v.pending;
  $("hytale-code").hidden = !(p && p.code);
  if (p && p.code) {
    $("hytale-step").textContent = `Step ${p.step} of ${p.of} (${p.for}):`;
    $("hytale-link").href = p.url;
    $("hytale-usercode").textContent = p.code;
  }
  $("hytale-status").textContent = v.signedIn
    ? `Signed in to Hytale as ${v.username || "your account"}. New Hytale servers are downloaded and signed in with it.`
    : p ? "Signing in to Hytale: the panel needs two codes, one for downloads and one for servers."
      : me.owner ? "Hytale servers need your Hytale account: the panel downloads the server with it and signs servers in so players can join. You sign in once." + (v.error ? " " + v.error : "")
        : "The owner has to sign the panel in to Hytale before Hytale servers can be added.";
  $("hytale-signin").hidden = !me.owner || v.signedIn || !!p;
  $("hytale-cancel").hidden = !me.owner || !p;
  $("hytale-signout").hidden = !me.owner || !v.signedIn || !!p;
  const submit = document.querySelector("#add-form button[type=submit]");
  if (submit) submit.disabled = editionChoice() === "hytale" && !v.signedIn;
  if (p && view === "add") hytaleTimer = setTimeout(loadHytaleAccount, 3000);
}

$("hytale-signin").addEventListener("click", async (ev) => {
  ev.currentTarget.disabled = true;
  try { renderHytaleAccount(await post("/api/hytale/signin")); } catch (err) { $("hytale-status").textContent = err.message; }
  ev.target.disabled = false;
});
$("hytale-cancel").addEventListener("click", async () => {
  try { renderHytaleAccount(await post("/api/hytale/signin/cancel")); } catch (err) { $("hytale-status").textContent = err.message; }
  setTimeout(loadHytaleAccount, 1000);
});
$("hytale-signout").addEventListener("click", async () => {
  if (!confirm("Sign the panel out of Hytale? Hytale servers can't be updated or started until you sign in again.")) return;
  try { renderHytaleAccount(await del("/api/hytale/account")); } catch (err) { $("hytale-status").textContent = err.message; }
});

function renderMemory() {
  const s = serverList.find((x) => x.id === selected);
  const show = !!s && (s.type === "java" || s.type === "hytale") && access(s) >= CARE;
  $("memory-card").hidden = !show;
  if (!show) return;
  const sel = $("memory-select");
  const v = String(s.memoryMB || 2048);
  if (![...sel.options].some((o) => o.value === v)) sel.append(el("option", { value: v }, `${Math.round(Number(v) / 102.4) / 10} GB`));
  if (document.activeElement !== sel) sel.value = v;
}
function renderConsoles() {
  const s = serverList.find((x) => x.id === selected);
  const show = !!s && s.type === "java" && access(s) >= CARE;
  $("consoles-card").hidden = !show;
  if (!show) return;
  const box = $("consoles-on");
  if (document.activeElement !== box) box.checked = !!s.consoles;
  $("consoles-on-label").textContent = s.consoles ? "On" : "Off";
  $("consoles-help").textContent = !s.consoles
    ? "Off: only Minecraft Java Edition (PC and Mac) can join."
    : s.consolesWaiting
      ? `Geyser, which lets consoles join, doesn't support Minecraft ${s.consolesWaiting} yet, so Xbox, PlayStation, Switch, phones and tablets can't join this server until it does. That's usually a few weeks after a new Minecraft version. The panel installs the new Geyser overnight (or press Update) as soon as it's out.`
      : s.consolesLive
      ? "Xbox, PlayStation, Switch, phones and tablets can join this server now, on the same port, and show up in the console server list. Add them to the allowlist by gamertag (choose \"Console, phone or tablet\" on the Players tab)."
      : "Xbox, PlayStation, Switch, phones and tablets can join while the server runs with console support (Geyser and Floodgate, installed and kept up to date by the panel). If it was just turned on, restart the server.";
}
$("consoles-on").addEventListener("change", async (ev) => {
  const box = ev.currentTarget;
  const on = box.checked;
  if (!on && !confirm("Turn console players off? Xbox, PlayStation, Switch, phone and tablet players won't be able to join this server.")) {
    box.checked = true;
    return;
  }
  try {
    await post(`${sid()}/consoles`, { on });
    showSettingsError(null);
    await refresh();
  } catch (err) { showSettingsError(err); box.checked = !on; }
  renderConsoles();
});

$("memory-select").addEventListener("change", async (ev) => {
  const sel = ev.currentTarget;
  try {
    await post(`${sid()}/memory`, { memoryMB: Number(sel.value) });
    showSettingsError(null);
    await refresh();
  } catch (err) { showSettingsError(err); }
  renderMemory();
});

// ---- add-ons tab ----

let addonsFor = null; // server ID the add-ons tab shows

function showAddonsError(err) {
  $("addons-error").textContent = err ? err.message : "";
  $("addons-error").hidden = !err;
}

function resetAddons() {
  addonsFor = null;
  $("addons-list").replaceChildren();
  $("worlds-list").replaceChildren();
  $("addons-world").textContent = "";
  $("addons-restart").hidden = true;
  showAddonsError(null);
}

async function loadAddons() {
  if (!selected) return;
  const id = selected;
  if (addonsFor !== id) resetAddons();
  try {
    const [packs, worlds] = await Promise.all([api(sid() + "/packs"), api(sid() + "/worlds")]);
    if (id !== selected) return;
    addonsFor = id;
    renderAddons(packs, worlds);
    showAddonsError(null);
  } catch (err) { if (id === selected) showAddonsError(err); }
}

const mb1 = (bytes) => (bytes >= 1e9 ? (bytes / 1e9).toFixed(1) + " GB" : bytes >= 1e6 ? (bytes / 1e6).toFixed(1) + " MB" : Math.max(1, Math.round(bytes / 1e3)) + " KB");

function addonsNeedRestart(running) {
  $("addons-restart").hidden = !running;
}

function renderAddons(v, wv) {
  const s = serverList.find((x) => x.id === selected);
  const care = access(s) >= CARE;
  for (const e of document.querySelectorAll("#pane-addons [data-care]")) e.hidden = !care;
  $("addons-world").textContent = v.world;
  const enc = encodeURIComponent;
  $("addons-list").replaceChildren(...(v.packs.length ? v.packs.map((p) => {
    const bits = [p.kind === "behavior" ? "Behavior pack" : "Resource pack"];
    if (p.version) bits.push(p.version);
    if (p.size) bits.push(mb1(p.size));
    const name = el("span", { class: "who" }, el("span", {}, p.name,
      p.enabled ? null : el("span", { class: "tag" }, "Off"),
      p.missing ? el("span", { class: "tag warn" }, "Files missing") : null,
      p.needsBeta ? el("span", { class: "tag warn", title: "Uses scripting that needs the world's Beta APIs experiment" }, "Needs Beta APIs") : null),
      el("small", {}, bits.join(" · ") + (p.description ? " · " + p.description : "")),
      p.needsBeta && care ? el("small", { class: "warn" }, "This add-on needs the Beta APIs experiment, which can't be turned on from the panel. Turn it on for the world in the game (Edit world, Experiments), then export the world and upload it below.") : null);
    const buttons = [];
    if (care) {
      if (!p.missing) {
        buttons.push(btn(p.enabled ? "Turn off" : "Turn on", () => addonsCall(async () => {
          const nv = await post(`${sid()}/packs/${enc(p.uuid)}`, { enabled: !p.enabled });
          addonsNeedRestart(nv.running);
          return nv;
        })));
      }
      buttons.push(btn("Remove", () => {
        if (!confirm(`Remove ${p.name} from ${v.world}? Things in the world that came from it may disappear.`)) return;
        addonsCall(async () => { const nv = await del(`${sid()}/packs/${enc(p.uuid)}`); addonsNeedRestart(nv.running); return nv; });
      }, "danger"));
    }
    return el("li", { class: p.enabled ? "" : "off" }, name, ...buttons);
  }) : [emptyRow(care ? "No add-ons yet. Drop one above." : "No add-ons in this world.")]));

  const owner = me.owner;
  $("worlds-list").replaceChildren(...wv.worlds.map((w) => {
    const bits = [];
    if (w.name && w.name !== w.folder) bits.push(w.name);
    if (w.size) bits.push(mb1(w.size)); else if (w.current) bits.push("made when the server first starts");
    if (w.packs) bits.push(`${w.packs} add-on pack${w.packs === 1 ? "" : "s"}`);
    const buttons = [];
    if (w.current) buttons.push(el("span", { class: "badge" }, "Playing"));
    else {
      buttons.push(btn("Play this world", () => {
        const note = wv.running ? " The server restarts, so players are disconnected for a moment." : "";
        if (!confirm(`Switch the server to ${w.folder}? The current world is kept.${note}`)) return;
        addonsCall(async () => { await post(`${sid()}/worlds/${enc(w.folder)}/play`); return null; });
      }));
    }
    if (care && w.size) buttons.push(el("a", { class: "button", href: `${sid()}/worlds/${enc(w.folder)}/download`, download: "" }, "Download"));
    if (owner && !w.current) {
      buttons.push(btn("Delete", () => {
        if (!confirm(`Delete the world ${w.folder}? A backup of all the worlds is made first, so it can be put back from Backups.`)) return;
        addonsCall(async () => { await del(`${sid()}/worlds/${enc(w.folder)}`); return null; });
      }, "danger"));
    }
    return row(w.folder, bits.join(" · "), ...buttons);
  }));
}

async function addonsCall(fn) {
  const id = selected;
  try {
    await fn();
    if (id === selected) showAddonsError(null);
  } catch (err) { if (id === selected) showAddonsError(err); }
  if (id === selected) loadAddons();
  refresh();
}

// upload sends a file with progress shown in the drop box.
function upload(box, url, file) {
  return new Promise((resolve, reject) => {
    const bar = box.querySelector(".upload-progress");
    const progress = bar.querySelector("progress");
    const label = bar.querySelector("span");
    bar.hidden = false;
    box.classList.add("busy");
    progress.value = 0;
    label.textContent = `Uploading ${file.name}…`;
    const xhr = new XMLHttpRequest();
    xhr.open("POST", url);
    xhr.setRequestHeader("X-Block-Party", "1");
    xhr.setRequestHeader("X-File-Name", encodeURIComponent(file.name));
    xhr.upload.onprogress = (ev) => {
      if (!ev.lengthComputable) return;
      progress.value = Math.round((ev.loaded / ev.total) * 100);
      label.textContent = ev.loaded >= ev.total ? "Unpacking and installing…" : `Uploading ${file.name}… ${mb1(ev.loaded)} of ${mb1(ev.total)}`;
    };
    const done = () => { bar.hidden = true; box.classList.remove("busy"); };
    xhr.onload = () => {
      done();
      let body = null;
      try { body = JSON.parse(xhr.responseText); } catch (_) { /* empty */ }
      if (xhr.status === 401) { signedOut(); reject(new Error("Signed out")); return; }
      if (xhr.status < 200 || xhr.status >= 300) { reject(new Error((body && body.error) || xhr.statusText || "Upload failed")); return; }
      resolve(body);
    };
    xhr.onerror = () => { done(); reject(new Error("The upload was cut off. Check the connection and try again.")); };
    xhr.send(file);
  });
}

function dropZone(boxId, inputId, handle) {
  const box = $(boxId);
  const input = $(inputId);
  input.addEventListener("change", () => { if (input.files[0]) handle(input.files[0]); input.value = ""; });
  box.addEventListener("dragover", (ev) => { ev.preventDefault(); box.classList.add("over"); });
  box.addEventListener("dragleave", () => box.classList.remove("over"));
  box.addEventListener("drop", (ev) => {
    ev.preventDefault();
    box.classList.remove("over");
    const f = ev.dataTransfer.files[0];
    if (f) handle(f);
  });
}

dropZone("addon-drop", "addon-file", async (file) => {
  const id = selected;
  try {
    const res = await upload($("addon-drop"), `/api/servers/${encodeURIComponent(id)}/packs`, file);
    if (id !== selected) return;
    const names = res.installed.map((p) => p.name).join(", ");
    const skipped = res.skipped.length ? `\n\nSkipped: ${res.skipped.join("; ")}` : "";
    const beta = res.installed.some((p) => p.needsBeta) ? "\n\nSomething in it needs the Beta APIs experiment: see the note next to it." : "";
    alert(`Installed ${names} in ${res.world}.${skipped}${beta}`);
    addonsNeedRestart(res.running);
    showAddonsError(null);
  } catch (err) { if (id === selected) showAddonsError(err); }
  if (id === selected) loadAddons();
});

dropZone("world-drop", "world-file", async (file) => {
  const id = selected;
  const play = $("world-play").checked;
  if (play && !confirm(`Upload ${file.name} and switch the server to it? The current world is kept. If the server is running, it restarts.`)) return;
  try {
    const res = await upload($("world-drop"), `/api/servers/${encodeURIComponent(id)}/worlds?play=${play ? 1 : 0}`, file);
    if (id !== selected) return;
    alert(res.playing ? `Added ${res.world.folder}; the server plays it now.` : `Added ${res.world.folder}. Press "Play this world" to switch to it.`);
    showAddonsError(null);
  } catch (err) { if (id === selected) showAddonsError(err); }
  if (id === selected) loadAddons();
  refresh();
});

$("addons-restart-now").addEventListener("click", async () => {
  $("addons-restart").hidden = true;
  try { await post(sid() + "/restart"); } catch (err) { showAddonsError(err); }
  refresh();
});
$("addons-restart-later").addEventListener("click", () => { $("addons-restart").hidden = true; });
// ---- plugins tab (Java) ----

let pluginsFor = null; // server ID the plugins tab shows
let pluginsView = null; // last loaded, to mark search results already installed
let pluginUpdates = {}; // file -> newer version
let pluginResults = null; // last search, kept while the list reloads

function showPluginsError(err) {
  $("plugins-error").textContent = err ? err.message : "";
  $("plugins-error").hidden = !err;
  if (err) $("plugins-error").scrollIntoView({ block: "nearest" });
}

function resetPlugins() {
  pluginsFor = null;
  pluginsView = null;
  pluginUpdates = {};
  pluginResults = null;
  $("plugins-list").replaceChildren();
  $("plugin-results").replaceChildren();
  $("plugin-notes").hidden = true;
  $("plugin-q").value = "";
  $("plugins-restart").hidden = true;
  showPluginsError(null);
}

async function loadPlugins() {
  if (!selected) return;
  const id = selected;
  if (pluginsFor !== id) resetPlugins();
  try {
    const v = await api(sid() + "/plugins");
    if (id !== selected) return;
    const first = pluginsFor !== id;
    pluginsFor = id;
    renderPlugins(v);
    showPluginsError(null);
    if (first) {
      api(sid() + "/plugins/updates").then((u) => {
        if (id !== selected) return;
        pluginUpdates = u || {};
        if (pluginsView) renderPlugins(pluginsView);
      }).catch(() => {});
      if (me.owner && pluginResults === null) searchPlugins("");
    }
  } catch (err) { if (id === selected) showPluginsError(err); }
}

function renderPlugins(v) {
  pluginsView = v;
  if (pluginResults) renderPluginResults($("plugin-q").value.trim());
  const owner = me.owner;
  $("plugins-find").hidden = !owner;
  $("plugins-ask").hidden = owner;
  $("plugins-mc").textContent = v.minecraft || "…";
  $("plugins-restart").hidden = !v.restartNeeded;
  const enc = encodeURIComponent;
  $("plugins-list").replaceChildren(...(v.plugins.length ? v.plugins.map((p) => {
    const bits = [];
    if (p.version) bits.push(p.version);
    if (p.console) bits.push("console support, kept up to date by the panel");
    else if (p.source) bits.push(`from ${p.source === "hangar" ? "Hangar" : "Modrinth"}` + (p.for ? ` (${p.for} needs it)` : ""));
    else bits.push("added by hand");
    const newer = pluginUpdates[p.file];
    const title = p.page ? el("a", { href: p.page, target: "_blank", rel: "noopener noreferrer" }, p.title || p.name) : (p.title || p.name);
    const name = el("span", { class: "who" }, el("span", {}, title,
      p.enabled ? null : el("span", { class: "tag" }, "Off"),
      newer ? el("span", { class: "tag warn" }, `${newer} available`) : null),
      el("small", {}, bits.join(" · ")));
    const buttons = [];
    if (!p.console) {
      if (owner && newer && p.source) buttons.push(btn("Update", (ev) => pluginCall(ev, () => post(`${sid()}/plugins/${enc(p.file)}/update`), () => { delete pluginUpdates[p.file]; }), "primary"));
      buttons.push(btn(p.enabled ? "Turn off" : "Turn on", (ev) => pluginCall(ev, () => post(`${sid()}/plugins/${enc(p.file)}/enabled`, { enabled: !p.enabled }))));
      if (owner) buttons.push(btn("Remove", (ev) => {
        const users = v.plugins.filter((x) => x.enabled && (x.needs || []).some((n) => n.toLowerCase() === p.name.toLowerCase())).map((x) => x.title || x.name);
        const warn = users.length ? `\n\n${users.join(", ")} needs it, and won't work without it.` : "";
        if (!confirm(`Remove ${p.title || p.name}? Its settings folder is kept, in case you add it again.${warn}`)) return;
        pluginCall(ev, () => del(`${sid()}/plugins/${enc(p.file)}`));
      }, "danger"));
    }
    return el("li", { class: p.enabled ? "" : "off" }, name, ...buttons);
  }) : [emptyRow(owner ? "No plugins yet. Find some below." : "No plugins yet.")]));
}

// pluginCall runs a plugin change, showing it's working on the button.
async function pluginCall(ev, fn, after) {
  const id = selected;
  const b = ev && ev.currentTarget;
  const label = b ? b.textContent : "";
  if (b) { b.disabled = true; b.textContent = "Working…"; }
  let ok = false;
  try {
    const v = await fn();
    ok = true;
    if (after) after();
    if (id === selected) { showPluginsError(null); if (v && v.plugins) renderPlugins(v); }
  } catch (err) { if (id === selected) showPluginsError(err); }
  if (b && b.isConnected) { b.disabled = false; b.textContent = label; }
  if (ok && id === selected) loadPlugins();
  refresh();
}

let pluginSearchSeq = 0;

async function searchPlugins(q) {
  const id = selected;
  const seq = ++pluginSearchSeq;
  $("plugin-results").replaceChildren(emptyRow(q ? "Searching…" : "Loading popular plugins…"));
  try {
    const r = await api(`${sid()}/plugins/search?q=${encodeURIComponent(q)}`);
    if (id !== selected || seq !== pluginSearchSeq) return; // a newer search is on its way
    pluginResults = r.results || [];
    $("plugin-notes").textContent = (r.notes || []).join(" ");
    $("plugin-notes").hidden = !(r.notes || []).length;
    renderPluginResults(q);
  } catch (err) { if (id === selected && seq === pluginSearchSeq) { $("plugin-results").replaceChildren(); showPluginsError(err); } }
}

function renderPluginResults(q) {
  const fmt = (n) => (n >= 1e6 ? (n / 1e6).toFixed(1) + "M" : n >= 1e3 ? Math.round(n / 1e3) + "k" : String(n));
  $("plugin-results").replaceChildren(...(pluginResults.length ? pluginResults.map((p) => {
    const icon = p.icon ? el("img", { src: p.icon, alt: "", class: "plugin-icon", loading: "lazy", referrerpolicy: "no-referrer" }) : el("span", { class: "plugin-icon" });
    const title = p.page ? el("a", { href: p.page, target: "_blank", rel: "noopener noreferrer" }, p.name) : p.name;
    const who = el("span", { class: "who" }, el("span", {}, title, el("span", { class: "tag" }, p.source === "hangar" ? "Hangar" : "Modrinth")),
      el("small", {}, [p.author ? "by " + p.author : "", `${fmt(p.downloads)} downloads`, p.description].filter(Boolean).join(" · ")));
    const have = pluginsView && pluginsView.plugins.some((x) => x.source === p.source && x.id === p.id);
    return el("li", {}, icon, who, have ? el("span", { class: "badge" }, "Installed")
      : btn("Install", (ev) => pluginCall(ev, () => post(sid() + "/plugins", { source: p.source, id: p.id })), "primary"));
  }) : [emptyRow(q ? "Nothing found for that version of Minecraft. Try other words." : "Nothing to show.")]));
}

$("plugin-search-form").addEventListener("submit", (ev) => {
  ev.preventDefault();
  searchPlugins($("plugin-q").value.trim());
});
$("plugins-restart-now").addEventListener("click", async () => {
  $("plugins-restart").hidden = true;
  try { await post(sid() + "/restart"); } catch (err) { showPluginsError(err); }
  refresh();
});
$("plugins-restart-later").addEventListener("click", () => { $("plugins-restart").hidden = true; });

// Files dropped anywhere else would open in the browser and leave the panel.
window.addEventListener("dragover", (ev) => ev.preventDefault());
window.addEventListener("drop", (ev) => ev.preventDefault());

// ---- import ----

const importDraft = {}; // path -> {name, port, start}, kept while the list reloads
let importing = new Set(); // IDs of servers being copied

function mb(bytes) {
  if (!bytes) return "no world files";
  if (bytes >= 1e9) return (bytes / 1e9).toFixed(1) + " GB";
  return Math.max(1, Math.round(bytes / 1e6)) + " MB";
}

// importsFinished reloads the import screen when a copy ends, so a failure
// shows up there (a failed import leaves no server card behind).
function importsFinished(list) {
  const now = new Set(list.filter((s) => s.state === "importing").map((s) => s.id));
  const ended = [...importing].filter((id) => !now.has(id));
  importing = now;
  if (!ended.length) return;
  // A failed import leaves no card, so open the import screen to show why.
  const failed = ended.some((id) => !list.some((s) => s.id === id));
  if (failed && view !== "import") location.hash = "#/import";
  else if (view === "import") loadImports();
}

async function loadImports() {
  try {
    const v = await api("/api/import");
    renderImports(v || {});
  } catch (err) {
    showImportError(err);
  }
}

function showImportError(err) {
  $("import-error").textContent = err ? err.message : "";
  $("import-error").hidden = !err;
}

function renderImports(v) {
  $("import-missing").hidden = !!v.available;
  $("import-ready").hidden = !v.available;
  if (!v.available) {
    $("import-missing").replaceChildren(
      "Nothing to import yet. In Unraid, edit the container and set \"Crafty servers folder\" to the other panel's servers folder (for binhex's Crafty: /mnt/user/appdata/binhex-crafty-4/crafty/servers), then apply. With docker run, add:",
      el("code", { class: "block" }, "-v /mnt/user/appdata/binhex-crafty-4/crafty/servers:/import/crafty:ro"));
    return;
  }
  showImportError(v.message ? new Error(v.message) : null);
  const done = new Set((v.candidates || []).filter((c) => c.imported).map((c) => c.path));
  const failed = (v.results || []).filter((r) => !r.ok && !done.has(r.path));
  $("import-results").replaceChildren(...failed.slice(0, 5).map((r) =>
    el("li", { class: "failed" }, el("span", { class: "who" }, `${r.name}: import failed`, el("small", {}, r.error)))));
  const list = v.candidates || [];
  if (!list.length) {
    $("import-list").replaceChildren(emptyRow("No Bedrock servers found in the import folder."));
    return;
  }
  $("import-list").replaceChildren(...list.map(importRow));
}

function importRow(c) {
  const facts = `${c.path} · world ${c.world || "?"} · ${mb(c.worlds)} · port ${c.port || "?"}`;
  if (c.imported) {
    return el("li", {}, el("span", { class: "who" }, c.name, el("small", {}, facts)),
      el("span", { class: "badge" }, `Imported as ${c.importedAs}`));
  }
  const d = importDraft[c.path] || (importDraft[c.path] = {
    name: c.name.slice(0, 40), port: c.portAvailable ? String(c.port) : "", start: false,
  });
  const name = el("input", { maxlength: "40", value: d.name, "aria-label": "Name in the panel", autocomplete: "off" });
  name.addEventListener("input", () => { d.name = name.value; });
  const port = el("input", { type: "number", min: "19134", max: "19198", step: "2", value: d.port, placeholder: "auto", "aria-label": "Port", class: "port" });
  port.addEventListener("input", () => { d.port = port.value; });
  const start = el("input", { type: "checkbox" });
  start.checked = d.start;
  start.addEventListener("change", () => { d.start = start.checked; });
  const go = el("button", { class: "primary", type: "button" }, "Import");
  go.addEventListener("click", async () => {
    if (!$("import-stopped").checked) {
      showImportError(new Error("Stop the server in the other panel first, then tick the box above."));
      return;
    }
    go.disabled = true;
    try {
      const s = await post("/api/import", {
        path: c.path, name: d.name.trim(), port: d.port ? Number(d.port) : 0, start: d.start,
      });
      delete importDraft[c.path];
      showImportError(null);
      importing.add(s.id);
      await loadImports();
      await refresh();
    } catch (err) {
      showImportError(err);
      go.disabled = false;
    }
  });
  const note = c.port && !c.portAvailable
    ? el("small", { class: "warn" }, `Port ${c.port} can't be kept (it's taken or outside ${19134}–${19198}). Leave the port empty to pick a free one.`)
    : null;
  return el("li", { class: "import" },
    el("span", { class: "who" }, c.name, el("small", {}, facts), note),
    el("div", { class: "row" },
      el("label", { class: "inline" }, "Name", name),
      el("label", { class: "inline" }, "Port", port),
      el("label", { class: "check inline" }, start, el("span", {}, "Start when copied")),
      go));
}


// ---- friends outside the house ----

// inviteText tells a friend how to join a server from outside the house.
function inviteText(v, name) {
  if (v.hytale) {
    return [`Come play on "${name}" with me in Hytale!`, "",
      "Open Hytale, go to Servers, choose Add Server, and type",
      `  Address: ${v.address}:${v.port}`, "",
      "Send me your Hytale username first so I can put you on the allowlist."].join("\n");
  }
  const javaLines = ["Open Minecraft (Java Edition) on your PC or Mac, choose Multiplayer, then Add Server, and type",
    `  Server Address: ${v.address}:${v.port}`, ""];
  if (v.java && !v.javaConsoles) {
    return [`Come play on "${name}" with me in Minecraft: Java Edition!`, "", ...javaLines,
      "Send me your Minecraft Java username first so I can put you on the allowlist."].join("\n");
  }
  const lines = [`Come play on "${name}" with me in Minecraft!`, ""];
  if (v.java) lines.push("On a PC or Mac with Minecraft: Java Edition:", ...javaLines.map((l) => l && "  " + l));
  lines.push(v.java ? "On a phone, tablet or Windows (Minecraft Bedrock): go to Play, then Servers, then Add Server, and type"
    : "On a PC, phone or tablet: open Minecraft, go to Play, then Servers, then Add Server, and type",
    `  Server address: ${v.address}`, `  Port: ${v.bedrockPort || v.port}`, "");
  lines.push("On an Xbox, PlayStation or Switch (they can't type an address):");
  if (v.consoles && v.consoleDNS) {
    lines.push(`  1. In the console's network settings, set the DNS by hand: primary ${v.consoleDNS}, secondary 1.1.1.1.`,
      "  2. Restart the console, open Minecraft, go to Servers and join The Hive (or any featured server).",
      `  3. A list of servers appears instead. Pick "${name}".`,
      "  If the console can't get online with that DNS, try the way below instead.", "");
    lines.push("Another way for consoles:");
  }
  lines.push("  1. Set the DNS by hand: primary 104.238.130.180 (PlayStation: 45.55.68.52), secondary 8.8.8.8.",
    "     (That's BedrockConnect, a free public server list.)",
    "  2. Restart the console, open Minecraft, go to Servers and join any featured server.",
    `  3. Choose "Connect to a Server" and type ${v.address} and port ${v.bedrockPort || v.port}.`, "",
    v.java ? "Send me your gamertag (or Java username) first so I can put you on the allowlist."
      : "Send me your gamertag first so I can put you on the allowlist.");
  return lines.join("\n");
}

function renderOverviewOutside(v, id) {
  const s = serverList.find((x) => x.id === id);
  const care = access(s) >= CARE;
  const actions = [];
  $("ov-outside-problem").hidden = true;
  $("ov-invite-box").hidden = true;
  if (!v) { $("ov-outside-text").textContent = "Couldn't load this."; $("ov-outside-actions").replaceChildren(); return; }
  if (!v.enabled) {
    $("ov-outside-text").replaceChildren(...(me.owner
      ? ["Friends who don't live here can't join yet. ", el("a", { href: "#/outside" }, "Set up outside access")]
      : ["Friends who don't live here can't join yet. Ask ", ownerName(), " to turn on outside access."]));
    $("ov-outside-actions").replaceChildren();
    return;
  }
  if (v.open) {
    $("ov-outside-text").replaceChildren(v.reached ? "Open: friends on the allowlist can join from anywhere at " : "Marked open, but friends can't join right now. Address: ",
      el("b", { class: "mono" }, `${v.address}:${v.port}`), ".");
  } else {
    $("ov-outside-text").textContent = "Closed: only people in the house can join.";
  }
  const notes = [];
  if (v.problem) notes.push(`It can't be reached while ${v.problem}. Turn it back on (Players or Settings tab).`);
  if (v.open && v.shared) notes.push("Your internet provider (or a second router) shares this home's address, so friends can't reach it. See Friends outside the house.");
  if (v.open && !v.problem && !v.opened && me.owner) {
    const fw = (v.forwards || []).join(", ");
    notes.push(v.upnp && v.portError
      ? `The router didn't open port ${v.port}: ${v.portError}. Forward ${fw} to this panel by hand.`
      : (v.upnp ? "" : `Forward ${fw} to this panel in your router (see Friends outside the house).`));
  }
  const note = notes.filter(Boolean).join(" ");
  $("ov-outside-problem").textContent = note;
  $("ov-outside-problem").hidden = !note;
  if (care) {
    actions.push(btn(v.open ? "Close to friends outside" : "Open to friends outside", async (ev) => {
      const b = ev.currentTarget;
      if (!v.open && !confirm(`Open ${s ? s.name : "this server"} to friends outside the house?\n\nOnly players on its allowlist can join, and their Xbox sign-in is checked.`)) return;
      b.disabled = true;
      try {
        const nv = await post(`/api/servers/${encodeURIComponent(id)}/outside`, { open: !v.open });
        if (id === selected) renderOverviewOutside(nv, id);
        refresh();
      } catch (err) { alert(err.message); b.disabled = false; }
    }, v.open ? "" : "primary"));
  }
  if (v.open && v.address) {
    const text = inviteText(v, s ? s.name : "my server");
    $("ov-invite").value = text;
    $("ov-invite-box").hidden = false;
    actions.push(btn("Copy invite", async (ev) => {
      const b = ev.currentTarget;
      try {
        await navigator.clipboard.writeText(text);
        b.textContent = "Copied";
        setTimeout(() => { b.textContent = "Copy invite"; }, 2000);
      } catch (_) {
        $("ov-invite-box").open = true;
        $("ov-invite").select();
      }
    }));
  }
  $("ov-outside-actions").replaceChildren(...actions);
}

let outsideDirty = false;

function showOutsideError(err) {
  $("outside-error").textContent = err ? err.message : "";
  $("outside-error").hidden = !err;
}

async function loadOutside(check) {
  try {
    const st = check ? await post("/api/outside/check") : await api("/api/outside");
    if (view !== "outside") return;
    renderOutside(st);
    showOutsideError(null);
  } catch (err) { showOutsideError(err); }
}

function fact(ok, text) { return checkItem(ok, text); }

function renderOutside(st) {
  const set = st.settings;
  if (!outsideDirty) {
    $("outside-enabled").checked = set.enabled;
    $("outside-address").value = set.address || "";
    $("outside-upnp").checked = set.upnp;
    $("outside-consoles").checked = set.consoles;
  }
  $("outside-address").placeholder = st.publicIP ? `Leave empty to use ${st.publicIP}, or type a hostname` : "mc.example.com, or leave empty for this home's internet address";
  const facts = [];
  if (st.publicIP) facts.push(fact(!st.shared, `This home's internet address is ${st.publicIP}` + (st.publicIPFrom === "router" ? " (from the router)." : ".")));
  else if (set.enabled) facts.push(fact(false, st.publicIPError || "Looking up this home's internet address…"));
  if (st.shared) facts.push(fact(false, "Your internet provider or a second router shares this address (called CGNAT or double NAT), so forwarded ports can't be reached from outside. Ask your provider for a public IP, or put the router in front into bridge mode."));
  if (set.address && st.addressIP && !st.addressWarning) facts.push(fact(true, `${set.address} leads here (${st.addressIP}).`));
  if (st.addressWarning) facts.push(fact(false, st.addressWarning));
  if (set.enabled && st.address) facts.push(fact(true, `Friends join at ${st.address}, with each server's own port.`));
  $("outside-facts").replaceChildren(...facts);
  $("outside-facts").hidden = facts.length === 0;

  const r = $("outside-router");
  if (!set.upnp) { r.textContent = ""; r.className = "small"; }
  else if (st.router) { r.textContent = `Router: ${st.router}.`; r.className = "small ok"; }
  else { r.textContent = st.routerError || (set.enabled ? "Looking for the router…" : ""); r.className = "small warn"; }

  // Runs of ports next to each other with the same type, use and state are
  // one row ("49152-49171").
  const ports = [];
  for (const p of st.ports || []) {
    const last = ports[ports.length - 1];
    if (last && last.proto === p.proto && last.for === p.for && last.opened === p.opened && last.error === p.error && last.to + 1 === p.port) {
      last.to = p.port;
    } else {
      ports.push({ ...p, to: p.port });
    }
  }
  $("outside-ports").replaceChildren(...(ports.length
    ? [el("tr", {}, el("th", {}, "Port"), el("th", {}, "Type"), el("th", {}, "Forward to"), el("th", {}, "For"), el("th", {}, "Router")),
      ...ports.map((p) => el("tr", {},
        el("td", { class: "mono", style: "white-space: nowrap" }, p.to > p.port ? `${p.port}-${p.to}` : String(p.port)), el("td", {}, p.proto), el("td", { class: "mono" }, st.localIP), el("td", {}, p.for),
        el("td", { class: p.opened ? "ok" : (p.error ? "warn" : "muted") }, p.opened ? "Opened" : (set.upnp ? (p.error || "…") : "Forward by hand"))))]
    : [el("tr", {}, el("td", { class: "muted" }, set.enabled ? "Nothing to forward yet: no server is open to friends outside." : "Nothing is forwarded while outside access is off."))]));

  $("outside-servers").replaceChildren(...((st.servers || []).length ? st.servers.map((x) => {
    const detail = x.problem ? `Can't be reached: ${x.problem}` : (x.reached ? `Open at ${st.address}:${x.port}` : (x.open ? "Open (outside access is off)" : `Closed · port ${x.port}`));
    return row(x.name, detail,
      el("a", { class: "button", href: serverHref(x.id) }, "Open page"));
  }) : [emptyRow("No servers yet.")]));
}

for (const id of ["outside-enabled", "outside-upnp", "outside-consoles", "outside-address"]) {
  $(id).addEventListener("input", () => { outsideDirty = true; $("outside-saved").hidden = true; });
}
$("outside-form").addEventListener("submit", async (ev) => {
  ev.preventDefault();
  const b = $("outside-save");
  b.disabled = true;
  b.textContent = "Saving…";
  try {
    const st = await api("/api/outside", { method: "PUT", body: JSON.stringify({
      enabled: $("outside-enabled").checked, address: $("outside-address").value.trim(),
      upnp: $("outside-upnp").checked, consoles: $("outside-consoles").checked }) });
    outsideDirty = false;
    dashOutsideAt = 0;
    renderOutside(st);
    showOutsideError(null);
    $("outside-saved").hidden = false;
  } catch (err) { showOutsideError(err); }
  b.disabled = false;
  b.textContent = "Save";
});
$("outside-check").addEventListener("click", async (ev) => {
  const b = ev.currentTarget;
  b.disabled = true;
  await loadOutside(true);
  b.disabled = false;
});

// ---- activity ----

function activityRow(e, withServer) {
  const d = new Date(e.time);
  const when = whenShort(e.time);
  const where = withServer && e.server ? ` · ${e.server}` : "";
  return el("li", {}, el("span", { class: "who" }, `${e.user || "The panel"} ${e.text}`,
    el("small", { title: d.toLocaleString() }, when + where)));
}

// ---- people (owner only) ----

let people = null; // last /api/users answer

const roleChoices = [["", "No access"], ["start", "Can turn it on"], ["run", "Runs it"], ["care", "Takes care of it"]];

function showPeopleError(err) {
  $("people-error").textContent = err ? err.message : "";
  $("people-error").hidden = !err;
}

async function loadPeople() {
  try {
    const [v, acts] = await Promise.all([api("/api/users"), api("/api/activity?limit=30")]);
    if (view !== "people") return;
    people = v;
    renderPeople();
    $("people-activity").replaceChildren(...(acts.length ? acts.map((e) => activityRow(e, true)) : [emptyRow("Nothing yet.")]));
    showPeopleError(null);
  } catch (err) { showPeopleError(err); }
}

// savePerson sends one change to what someone may do: {canAdd} or
// {grants: {serverID: role}}. Only what's sent changes, so quick changes
// in a row don't undo each other.
async function savePerson(u, change) {
  try {
    await api(`/api/users/${encodeURIComponent(u.username)}`, { method: "PUT", body: JSON.stringify(change) });
    showPeopleError(null);
  } catch (err) { showPeopleError(err); }
  loadPeople();
}

function renderPeople() {
  const v = people;
  if (!v) return;
  if (!v.users.length) {
    $("people-list").replaceChildren(el("p", { class: "card muted" }, "Nobody else has an account yet. Add someone below."));
    return;
  }
  $("people-list").replaceChildren(...v.users.map((u) => {
    const canAdd = el("input", { type: "checkbox", checked: u.canAdd });
    canAdd.addEventListener("change", () => savePerson(u, { canAdd: canAdd.checked }));
    const grants = v.servers.length ? v.servers.map((srv) => {
      const role = u.grants[srv.id] || "";
      const sel = el("select", { "aria-label": `${u.username} on ${srv.name}` },
        ...roleChoices.map(([val, label]) => el("option", { value: val, selected: val === role }, label)));
      sel.addEventListener("change", () => savePerson(u, { grants: { [srv.id]: sel.value } }));
      return el("li", { class: role ? "" : "none" }, el("span", {}, srv.name), sel);
    }) : [el("li", {}, el("span", {}, "No servers yet."))];
    return el("section", { class: "card person" },
      el("div", { class: "person-head" },
        el("span", { class: "avatar" }, u.username.slice(0, 1).toUpperCase()),
        el("div", { class: "grow" }, el("h2", {}, u.username),
          el("small", {}, (u.lastSeen ? `Last used the panel ${whenShort(u.lastSeen).toLowerCase()}` : "Hasn't signed in yet") + (u.twoFactor ? " · two-factor on" : ""))),
        u.twoFactor ? btn("Turn off two-factor", async () => {
          if (!confirm(`Turn off two-factor sign-in for ${u.username}? Do this if they lost their phone and recovery codes. They're signed out, and can turn it on again.`)) return;
          try { await post(`/api/users/${encodeURIComponent(u.username)}/2fa/reset`); showPeopleError(null); } catch (err) { showPeopleError(err); }
          loadPeople();
        }) : null,
        btn("New password", async () => {
          const pw = prompt(`New password for ${u.username} (at least 8 characters). They'll be signed out everywhere.`);
          if (!pw) return;
          try {
            await post(`/api/users/${encodeURIComponent(u.username)}/password`, { password: pw });
            showPeopleError(null);
            alert(`${u.username}'s password is changed. Tell them the new one.`);
          } catch (err) { showPeopleError(err); }
          loadPeople();
        }),
        btn("Remove", async () => {
          if (!confirm(`Remove ${u.username}'s account? They're signed out, and their servers stay as they are.`)) return;
          try { await del(`/api/users/${encodeURIComponent(u.username)}`); showPeopleError(null); } catch (err) { showPeopleError(err); }
          loadPeople();
        }, "danger")),
      el("ul", { class: "grants" }, ...grants),
      el("label", { class: "check" }, canAdd, el("span", {}, "Can add new servers (they take care of the ones they add)")));
  }));
}

$("person-form").addEventListener("submit", async (ev) => {
  ev.preventDefault();
  try {
    await post("/api/users", { username: $("person-name").value.trim(), password: $("person-pass").value, canAdd: $("person-canadd").checked });
    $("person-form").reset();
    formError("person-error", null);
    loadPeople();
  } catch (err) { formError("person-error", err); }
});

// ---- two-factor sign-in (account page) ----

function showTwoError(err) {
  $("two-error").textContent = err ? err.message : "";
  $("two-error").hidden = !err;
}

function renderTwo(v) {
  $("two-status").textContent = v.on
    ? `On: signing in asks for a code from your authenticator app after your password. ${v.recoveryLeft} recovery code${v.recoveryLeft === 1 ? "" : "s"} left.`
    : "Off. With it on, someone who learns your password still can't sign in without your phone. Worth it if the panel is reachable from outside your home.";
  $("two-start").hidden = v.on;
  $("two-new-codes").hidden = !v.on;
  $("two-off").hidden = !v.on;
  $("two-setup").hidden = true;
}

async function loadTwo() {
  try { renderTwo(await api("/api/auth/2fa")); showTwoError(null); } catch (err) { showTwoError(err); }
}

function showCodes(codes) {
  $("two-codes-list").textContent = codes.join("\n");
  $("two-codes").hidden = false;
  $("two-actions").hidden = true;
}

$("two-start").addEventListener("click", async () => {
  const password = prompt("Your password, to turn on two-factor sign-in:");
  if (!password) return;
  try {
    const r = await post("/api/auth/2fa/start", { password });
    $("two-qr").src = "data:image/svg+xml;base64," + btoa(r.qr);
    $("two-secret").textContent = r.secret;
    $("two-setup").hidden = false;
    $("two-start").hidden = true;
    $("two-code").value = "";
    $("two-code").focus();
    showTwoError(null);
  } catch (err) { showTwoError(err); }
});
// Recovery codes may have been used elsewhere since the page was opened.
document.addEventListener("visibilitychange", () => {
  if (signedIn && view === "account" && !document.hidden && $("two-setup").hidden && $("two-codes").hidden) loadTwo();
});
$("two-cancel").addEventListener("click", () => { $("two-qr").removeAttribute("src"); loadTwo(); });
$("two-confirm-form").addEventListener("submit", async (ev) => {
  ev.preventDefault();
  try {
    const r = await post("/api/auth/2fa/confirm", { code: $("two-code").value });
    $("two-qr").removeAttribute("src");
    $("two-secret").textContent = "";
    renderTwo(r.status);
    showCodes(r.recoveryCodes);
    showTwoError(null);
  } catch (err) { showTwoError(err); $("two-code").value = ""; }
});
$("two-new-codes").addEventListener("click", async () => {
  const password = prompt("Your password, to make new recovery codes (the old ones stop working):");
  if (!password) return;
  try { const r = await post("/api/auth/2fa/recovery", { password }); renderTwo(r.status); showCodes(r.recoveryCodes); showTwoError(null); } catch (err) { showTwoError(err); }
});
$("two-off").addEventListener("click", async () => {
  const password = prompt("Your password, to turn two-factor sign-in off:");
  if (!password) return;
  try { renderTwo(await post("/api/auth/2fa/disable", { password })); showTwoError(null); } catch (err) { showTwoError(err); }
});
$("two-codes-copy").addEventListener("click", async () => {
  try { await navigator.clipboard.writeText($("two-codes-list").textContent); $("two-codes-copy").textContent = "Copied"; } catch (_) { /* select by hand */ }
});
$("two-codes-done").addEventListener("click", () => {
  $("two-codes-list").textContent = "";
  $("two-codes").hidden = true;
  $("two-actions").hidden = false;
  $("two-codes-copy").textContent = "Copy";
});

// ---- signing in ----

let signedIn = false;
let timers = [];

function showOnly(formId) {
  for (const id of ["setup-form", "login-form", "reset-form", "code-form"]) $(id).hidden = id !== formId;
  const first = $(formId).querySelector("input");
  if (first) first.focus();
}

function formError(id, err) {
  $(id).textContent = err ? err.message : "";
  $(id).hidden = !err;
}

async function start() {
  let st;
  try {
    st = await api("/api/auth/state");
  } catch (err) {
    // Try again shortly (the container may be restarting).
    $("auth-view").hidden = false;
    for (const id of ["setup-form", "login-form", "reset-form"]) $(id).hidden = true;
    let note = $("auth-offline");
    if (!note) {
      note = el("p", { id: "auth-offline", class: "card error" });
      $("auth-view").prepend(note);
    }
    note.textContent = "Can't reach the panel (" + err.message + "). Trying again…";
    note.hidden = false;
    setTimeout(start, 3000);
    return;
  }
  if ($("auth-offline")) $("auth-offline").hidden = true;
  $("version").textContent = st.version ? "Version " + st.version : "";
  for (const id of ["version-2", "version-3"]) $(id).textContent = st.version ? "Block Party " + st.version : "";
  if (st.mode === "signedIn") {
    enterApp(st);
    return;
  }
  $("auth-view").hidden = false;
  $("app-view").hidden = true;
  if (st.mode === "setup") {
    $("setup-code-help").replaceChildren(st.usesOldPassword
      ? (st.oldPasswordFromEnv
        ? "Type the panel password you used before (the PANEL_PASSWORD setting). After this, it isn't used any more; you can remove it from the docker run command."
        : "Type the panel password you used before (it's in /data/panel-password). After this, it isn't used any more.")
      : "It's in the container's log. In Unraid, open the Docker tab, click the Block Party icon and choose Logs. Or run: docker logs blockparty 2>&1 | grep -i \"setup code\"");
    showOnly("setup-form");
  } else {
    showOnly("login-form");
  }
}

function enterApp(st) {
  signedIn = true;
  me = { owner: !!st.owner, canAdd: !!st.canAdd, name: st.username };
  for (const e of document.querySelectorAll("[data-owner-only]")) e.hidden = !me.owner;
  for (const e of document.querySelectorAll("[data-not-owner]")) e.hidden = me.owner;
  for (const e of document.querySelectorAll("[data-can-add]")) e.hidden = !me.canAdd;
  $("auth-view").hidden = true;
  $("app-view").hidden = false;
  $("account-name").textContent = st.username;
  $("side-user").textContent = st.username;
  $("side-avatar").textContent = (st.username || "?").slice(0, 1).toUpperCase();
  $("tab-user").textContent = st.username;
  $("reset-user").value = st.username;
  applyPrefs(st.prefs || {}, false);
  if (me.owner) api("/api/settings").then((s) => { settings = s || {}; }).catch(() => {});
  loadJoinInfo();
  loadMyTime();
  checkUpdates();
  timers.forEach(clearInterval);
  const onServer = (t) => signedIn && view === "server" && selected && tab === t && !document.hidden;
  timers = [
    setInterval(refresh, 2000),
    setInterval(() => { if (onServer("players")) loadPlayers(); }, 3000),
    setInterval(() => { if (onServer("backups")) loadBackups(); }, 3000),
    setInterval(() => { if (onServer("overview")) loadOverview(); }, 10000),
    setInterval(checkUpdates, 10 * 60 * 1000),
    setInterval(loadJoinInfo, 30 * 60 * 1000),
    setInterval(loadMyTime, 60 * 1000),
    setInterval(() => { if (signedIn && view === "outside" && !document.hidden) loadOutside(false); }, 10000),
  ];
  if (me.owner && !st.setupDone && !location.hash.startsWith("#/guide")) location.hash = "#/guide";
  else route();
}

// ---- console list info (Joining page, sidebar, dashboard) ----

async function loadJoinInfo() {
  try {
    joinInfo = await api("/api/info");
  } catch (_) { return; }
  fillJoinInfo();
}

// ownerName is the owner's username, for "ask Scott".
function ownerWord() { return (joinInfo && joinInfo.owner) || "the owner"; }
function ownerName() { return el("b", {}, ownerWord()); }

function fillJoinInfo() {
  if (!joinInfo) return;
  for (const e of document.querySelectorAll(".owner-name")) e.textContent = ownerWord();
  if (view === "servers") renderDashboard(serverList, true);
  const name = joinInfo.listName || "Server List";
  const ip = joinInfo.hostIP || "the panel's IP";
  for (const id of ["side-list-name", "dash-list-name", "join-list-name"]) $(id).textContent = name;
  for (const id of ["side-list-ip", "dash-list-ip", "join-list-ip"]) $(id).textContent = ip;
  fillPanelUpdate();
}

// ---- a newer Block Party ----

// The owner hears about a newer release on the Servers page (until they
// say "Not now" for that version) and on Settings.
function fillPanelUpdate() {
  const cur = joinInfo.version || "";
  const next = me.owner ? joinInfo.panelUpdate || "" : "";
  const notes = "https://github.com/ScottSchneck/block-party/blob/main/CHANGELOG.md";
  let dismissed = "";
  try { dismissed = localStorage.getItem("bp-update-dismissed") || ""; } catch (_) { /* private window */ }
  $("panel-update-banner").hidden = !next || dismissed === next;
  $("panel-update-text").textContent = `Block Party ${next} is out`;
  $("panel-update-how").textContent = `(you have ${cur}). Update it from Unraid's Docker tab: Apply update.`;
  $("panel-update-notes").href = notes;
  $("settings-version").replaceChildren(...(next
    ? [el("b", {}, `Block Party ${next} is out.`), ` This panel is ${cur}. `, el("a", { href: notes, target: "_blank", rel: "noopener noreferrer" }, "What's new")]
    : [cur ? `This panel is Block Party ${cur}` + (joinInfo.panelLatest ? ", the newest version." : ".") : ""]));
}

$("panel-update-dismiss").addEventListener("click", () => {
  try { localStorage.setItem("bp-update-dismissed", joinInfo.panelUpdate || ""); } catch (_) { /* private window */ }
  $("panel-update-banner").hidden = true;
});

// ---- appearance ----

let prefs = { style: "control", mode: "auto" };

// applyPrefs sets the look, and remembers it in this browser so the next
// visit starts with it before the account's copy loads.
function applyPrefs(p, save) {
  prefs = { style: p.style === "treehouse" ? "treehouse" : "control", mode: ["light", "dark", "auto"].includes(p.mode) ? p.mode : "auto" };
  const changed = document.documentElement.dataset.style !== prefs.style;
  document.documentElement.dataset.style = prefs.style;
  document.documentElement.dataset.mode = prefs.mode;
  if (changed && signedIn) {
    // Words and layout differ between styles: draw everything again.
    dashKey = detailKey = "";
    if (view === "servers") renderDashboard(serverList, true);
    if (view === "server" && tab === "players") loadPlayers();
    refresh();
  }
  try { localStorage.setItem("bh-prefs", JSON.stringify(prefs)); } catch (_) { /* private window */ }
  for (const r of document.querySelectorAll("input[name=style]")) r.checked = r.value === prefs.style;
  for (const r of document.querySelectorAll("input[name=mode]")) r.checked = r.value === prefs.mode;
  if (save) {
    api("/api/auth/prefs", { method: "PUT", body: JSON.stringify(prefs) })
      .then(() => { $("prefs-error").hidden = true; })
      .catch((err) => { $("prefs-error").textContent = "Couldn't save: " + err.message; $("prefs-error").hidden = false; });
  }
}
for (const r of document.querySelectorAll("input[name=style], input[name=mode]")) {
  r.addEventListener("change", () => {
    const style = document.querySelector("input[name=style]:checked");
    const mode = document.querySelector("input[name=mode]:checked");
    applyPrefs({ style: style ? style.value : "control", mode: mode ? mode.value : "auto" }, true);
  });
}

// signedOut goes back to the sign-in page and forgets what was on screen.
function signedOut() {
  if (!signedIn) return;
  signedIn = false;
  // Nothing of the two-factor setup stays on screen.
  $("two-codes-list").textContent = "";
  $("two-secret").textContent = "";
  $("two-qr").removeAttribute("src");
  $("two-setup").hidden = true;
  $("two-codes").hidden = true;
  loginChallenge = null;
  timers.forEach(clearInterval);
  timers = [];
  closeConsole();
  closeOverviewConsole();
  resetSettings();
  resetBackups();
  resetAddons();
  resetPlugins();
  selected = null;
  settings = {};
  me = { owner: false, canAdd: false, name: "" };
  people = null;
  nightlyOn = null;
  outsideDirty = false;
  dashOutsideAt = 0;
  guideInfo = null;
  joinInfo = null;
  serverList = [];
  gamertagWarned = false;
  view = "servers";
  acceptedHash = "#/";
  clearServerPanes();
  dashKey = "";
  latestVersion = "";
  $("update-banner").hidden = true;
  $("dash-attempts").hidden = true;
  $("dash-backups").textContent = "";
  $("servers-summary").textContent = "";
  $("detail-name").textContent = "";
  $("detail-facts").replaceChildren();
  $("detail-actions").replaceChildren();
  $("password-msg").hidden = true;
  for (const f of ["password-form", "add-form"]) $(f).reset();
  for (const id of ["servers", "online", "allowlist", "operators", "attempts", "console-log", "import-list", "import-results", "guide-checklist", "ov-online", "dash-attempts-list", "backups-overview", "people-list", "people-activity", "ov-activity"]) $(id).replaceChildren();
  $("person-form").reset();
  start();
}

$("setup-form").addEventListener("submit", async (ev) => {
  ev.preventDefault();
  if ($("setup-pass").value !== $("setup-pass2").value) {
    formError("setup-error", new Error("The two passwords aren't the same."));
    return;
  }
  try {
    await post("/api/auth/setup", { code: $("setup-code").value, username: $("setup-user").value, password: $("setup-pass").value });
    $("setup-form").reset();
    formError("setup-error", null);
    start();
  } catch (err) { formError("setup-error", err); }
});

let loginChallenge = null;

$("login-form").addEventListener("submit", async (ev) => {
  ev.preventDefault();
  try {
    const res = await post("/api/auth/login", { username: $("login-user").value, password: $("login-pass").value });
    $("login-pass").value = "";
    formError("login-error", null);
    if (res && res.needCode) {
      loginChallenge = res.challenge;
      $("code-input").value = "";
      formError("code-error", null);
      showOnly("code-form");
      return;
    }
    start();
  } catch (err) {
    $("login-pass").value = "";
    formError("login-error", err);
  }
});

$("code-form").addEventListener("submit", async (ev) => {
  ev.preventDefault();
  try {
    await post("/api/auth/login/code", { challenge: loginChallenge, code: $("code-input").value });
    loginChallenge = null;
    $("code-input").value = "";
    formError("code-error", null);
    start();
  } catch (err) {
    $("code-input").value = "";
    formError("code-error", err);
    if (/password again|sign in again|too many/i.test(err.message)) setTimeout(() => showOnly("login-form"), 1500);
  }
});
$("code-back").addEventListener("click", (ev) => { ev.preventDefault(); loginChallenge = null; showOnly("login-form"); });

$("forgot-link").addEventListener("click", (ev) => { ev.preventDefault(); showOnly("reset-form"); });
$("reset-back").addEventListener("click", () => showOnly("login-form"));
$("reset-form").addEventListener("submit", async (ev) => {
  ev.preventDefault();
  if ($("reset-pass").value !== $("reset-pass2").value) {
    formError("reset-error", new Error("The two passwords aren't the same."));
    return;
  }
  try {
    await post("/api/auth/reset", { code: $("reset-code").value, password: $("reset-pass").value });
    $("reset-form").reset();
    formError("reset-error", null);
    start();
  } catch (err) { formError("reset-error", err); }
});

// ---- account ----

$("logout").addEventListener("click", async () => {
  try { await post("/api/auth/logout"); } catch (_) { /* signed out anyway */ }
  signedOut();
});
$("logout-all").addEventListener("click", async () => {
  if (!confirm("Sign out on every browser and phone, including this one?")) return;
  try { await post("/api/auth/logout-all"); } catch (_) { /* signed out anyway */ }
  signedOut();
});
$("password-form").addEventListener("submit", async (ev) => {
  ev.preventDefault();
  const msg = $("password-msg");
  msg.hidden = false;
  if ($("pw-new").value !== $("pw-new2").value) {
    msg.className = "small error";
    msg.textContent = "The two new passwords aren't the same.";
    return;
  }
  try {
    await post("/api/auth/password", { current: $("pw-current").value, new: $("pw-new").value });
    $("password-form").reset();
    msg.className = "small ok";
    msg.textContent = "Password changed. Other browsers were signed out.";
  } catch (err) {
    msg.className = "small error";
    msg.textContent = err.message;
  }
});

// ---- setup guide ----

let guideStep = 1;
let guideInfo = null;
let gamertagWarned = false;

async function openGuide(step) {
  $("guide").hidden = false;
  try {
    guideInfo = await api("/api/setup-guide");
  } catch (err) { return; }
  $("guide-gamertag").value = guideInfo.ownerGamertag || settings.ownerGamertag || "";
  $("guide-list-name").textContent = guideInfo.listName || "Server List";
  $("guide-list-ip").textContent = guideInfo.listIP || "the panel's IP";
  const n = guideInfo.servers;
  $("guide-everywhere-label").hidden = n === 0;
  $("guide-everywhere-text").textContent = `Also add me to the allowlist and make me an operator on the ${n} server${n === 1 ? "" : "s"} I already have`;
  $("guide-gamertag-msg").hidden = true;
  showGuideStep(step);
}

function showGuideStep(step) {
  guideStep = step;
  for (const p of document.querySelectorAll(".guide-pane")) p.hidden = Number(p.dataset.step) !== step;
  for (const li of document.querySelectorAll("#guide-steps li")) {
    const n = Number(li.dataset.step);
    li.className = n === step ? "current" : (n < step ? "done" : "");
  }
  $("guide-step-label").textContent = `step ${step} of 4`;
  $("guide-back").disabled = step === 1;
  $("guide-next").textContent = step === 4 ? "Finish" : (step === 1 ? "Save and continue" : "Next");
  if (step === 3) renderGuideServers();
  if (step === 4) renderChecklist();
}

function renderGuideServers() {
  const g = guideInfo;
  const parts = [];
  parts.push(g.servers === 0 ? "You don't have any servers yet." : `You have ${g.servers} server${g.servers === 1 ? "" : "s"} in the panel.`);
  if (g.importWaiting > 0) parts.push(` ${g.importWaiting} server${g.importWaiting === 1 ? " is" : "s are"} in the import folder and not imported yet.`);
  else if (!g.importMounted && g.servers === 0) parts.push(" To bring servers over from Crafty, set the container's Crafty servers folder (it's mounted at /import, see the README), or add a new one.");
  $("guide-servers-text").textContent = parts.join("");
  $("guide-import").hidden = !(g.importMounted && g.importWaiting > 0);
  $("guide-add").hidden = g.servers > 0 && g.importWaiting === 0;
}

function checkItem(ok, text) {
  return el("li", { class: ok ? "ok" : "todo" }, el("span", { class: "mark", "aria-hidden": "true" }, ok ? "✓" : "!"), el("span", {}, text));
}

async function renderChecklist() {
  try { guideInfo = await api("/api/setup-guide"); } catch (_) { /* keep the last one */ }
  const g = guideInfo;
  const items = [checkItem(true, "Owner account created.")];
  items.push(g.ownerGamertag
    ? checkItem(true, `Your gamertag is ${g.ownerGamertag}.`)
    : checkItem(false, "Add your gamertag in step 1 so you're never locked out of your own servers."));
  items.push(g.timeZoneIsUTC
    ? checkItem(false, "The panel's clock is on UTC, so a 4 am backup would run at the wrong time. Set TZ to your time zone (for example America/Denver): Unraid does this itself; with docker run, add -e TZ=America/Denver.")
    : checkItem(true, `Time zone: ${g.timeZone}.`));
  if (g.backupsOff.length) items.push(checkItem(false, `Automatic backups are off for: ${g.backupsOff.join(", ")}.`));
  if (g.neverBackedUp.length) items.push(checkItem(false, `Not backed up yet: ${g.neverBackedUp.join(", ")}. The first automatic backup runs within a minute or two; or press Back up now on its Backups tab.`));
  if (!g.backupsOff.length && !g.neverBackedUp.length && g.servers > 0) items.push(checkItem(true, "Every server has a backup and a schedule."));
  if (g.importMounted && g.importWaiting === 0 && g.servers > 0) {
    items.push(checkItem(true, "Everything in the import folder is imported. Once you're happy, stop the servers in Crafty for good and remove the import folder from the container's settings."));
  } else if (g.importWaiting > 0) {
    items.push(checkItem(false, `${g.importWaiting} server${g.importWaiting === 1 ? " is" : "s are"} waiting in the import folder.`));
  }
  if (g.listIP) items.push(checkItem(true, `Consoles and their DNS settings use ${g.listIP}, so keep it from changing: in Unraid, give the container a fixed IP address in its settings.`));
  items.push(checkItem(true, `Bookmark ${location.origin} on your phone; the panel works there too.`));
  $("guide-checklist").replaceChildren(...items);
}

async function finishGuide() {
  try { await post("/api/setup-guide/done", { done: true }); } catch (_) { /* shown again next time */ }
  location.hash = "#/";
}

$("guide-back").addEventListener("click", () => showGuideStep(Math.max(1, guideStep - 1)));
$("guide-skip").addEventListener("click", finishGuide);
$("guide-next").addEventListener("click", async () => {
  if (guideStep === 1) {
    const name = $("guide-gamertag").value.trim();
    if (name) {
      const msg = $("guide-gamertag-msg");
      try {
        const r = await post("/api/setup-guide/gamertag", { gamertag: name, everywhere: $("guide-everywhere").checked });
        settings.ownerGamertag = name;
        guideInfo.ownerGamertag = name;
        if (r.skipped.length && !gamertagWarned) {
          gamertagWarned = true;
          msg.hidden = false;
          msg.className = "small error";
          msg.textContent = "Saved, but some servers were skipped: " + r.skipped.join("; ") + ". Press the button again to carry on.";
          return; // let them read it; Next again moves on
        }
      } catch (err) {
        msg.hidden = false;
        msg.className = "small error";
        msg.textContent = err.message;
        return;
      }
    }
    showGuideStep(2);
    return;
  }
  if (guideStep === 4) { finishGuide(); return; }
  showGuideStep(guideStep + 1);
});

// ---- start ----

start();
