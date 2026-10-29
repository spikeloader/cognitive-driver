// Painel do cognitive-driver: consome a API local (/health e /events).
// Todo conteúdo vindo da API é inserido via textContent. Textos da
// interface vêm de i18n.js (t, LANG, locale).
"use strict";

const REFRESH_MS = 15000;
const EVENTS_LIMIT = 500;
const PAGE_SIZE = 10;
const TOKEN_KEY = "cognitive-driver-token";
const SEVERITIES = ["critical", "high", "medium", "low"];

// Unidade das métricas de eventos antigos (gravados antes de metric_label/unit).
const LEGACY_UNITS = {
  node_cpu_seconds_total: "cores",
  node_memory_MemAvailable_bytes: "bytes",
  node_disk_io_time_seconds_total: "ratio",
  node_network_receive_bytes_total: "bytes_per_second",
  node_network_transmit_bytes_total: "bytes_per_second",
};
const statusLabel = (s) => (["open", "acknowledged", "resolved", "false_positive"].includes(s) ? t(`status.${s}`) : s);

const state = {
  events: [],
  health: null,
  selectedId: null,
  page: 1,
  filters: { status: "open", severity: "", driver: "", host: "", q: "" },
  lastRefresh: null,
};
const $ = (sel) => document.querySelector(sel);

// ---------------------------------------------------------------- formatação
function num(v, digits) {
  return Number(v).toLocaleString(locale(), { minimumFractionDigits: digits, maximumFractionDigits: digits });
}
function bytes(v) {
  const units = ["B", "KiB", "MiB", "GiB", "TiB"];
  let i = 0;
  while (Math.abs(v) >= 1024 && i < units.length - 1) { v /= 1024; i++; }
  return `${num(v, 1)} ${units[i]}`;
}
function duration(v) {
  const a = Math.abs(v);
  if (a < 60) return `${num(v, 0)} s`;
  if (a < 3600) return `${num(v / 60, 1)} min`;
  if (a < 86400) return `${num(v / 3600, 1)} h`;
  return t("unit.days", { v: num(v / 86400, 1) });
}
// Mesmas unidades do pacote explain (Go).
function fmtUnit(unit, v) {
  switch (unit) {
    case "cores": return t("unit.cores", { v: num(v, 2) });
    case "bytes": return bytes(v);
    case "bytes_per_second": return `${bytes(v)}/s`;
    case "ratio": return `${num(v * 100, 1)}%`;
    case "per_second": return `${num(v, 2)}/s`;
    case "count": return num(v, 1);
    case "milliseconds": return `${num(v, 1)} ms`;
    case "seconds": return duration(v);
    case "bool": return v >= 0.5 ? t("unit.active") : t("unit.inactive");
    default: return num(v, 3);
  }
}

// Rótulos e unidades das métricas declaradas pelos drivers (/health).
function healthMetric(name) {
  for (const d of (state.health && state.health.drivers) || []) {
    for (const m of d.metric_details || []) if (m.name === name) return m;
  }
  return null;
}

// loc devolve os textos do evento no idioma do painel, ou null (português,
// que está nos campos principais, ou evento gravado antes das traduções).
function loc(e) {
  return (LANG !== "pt-BR" && e.translations && e.translations[LANG]) || null;
}
const evText = (e, field) => (loc(e) && loc(e)[field]) || e[field];

// metricInfo aceita um evento (usa metric_label/unit gravados) ou um nome.
function metricInfo(x) {
  const name = typeof x === "string" ? x : x.metric;
  const h = healthMetric(name.replace(/\{.*\}$/, "")); // séries agrupadas: m{job="x"}
  const hLabel = h && ((h.labels && h.labels[LANG]) || h.label);
  const legacy = I18N["pt-BR"][`legacy.${name}`] ? t(`legacy.${name}`) : "";
  const label = (typeof x === "object" && evText(x, "metric_label")) || hLabel || legacy || name;
  const unit = (typeof x === "object" && x.unit) || (h && h.unit) || LEGACY_UNITS[name] || "";
  return { label, unit, fmt: (v) => fmtUnit(unit, v) };
}

// Texto curto da regra de um evento: "esperado ativo (1)", "limite 90,0%".
function ruleText(e) {
  const [op, raw] = (e.rule || "").split(" ");
  const v = metricInfo(e).fmt(Number(raw));
  return ["above", "below", "equals"].includes(op) ? t(`rule.${op}`, { v }) : e.rule;
}

function jobLabel(job) {
  return job === "node" ? "host" : job.replace(/^ai-/, "").replace(/-health$/, "");
}

function relTime(iso) {
  const s = Math.round((Date.now() - new Date(iso)) / 1000);
  if (s < 60) return t("time.now");
  if (s < 3600) return t("time.min", { n: Math.floor(s / 60) });
  if (s < 86400) return t("time.h", { n: Math.floor(s / 3600) });
  return t("time.d", { n: Math.floor(s / 86400) });
}
function absTime(iso) {
  return new Date(iso).toLocaleString(locale(), { dateStyle: "short", timeStyle: "medium" });
}
function uptime(sec) {
  if (sec >= 86400) return t("uptime.dh", { d: Math.floor(sec / 86400), h: Math.floor((sec % 86400) / 3600) });
  if (sec >= 3600) return t("uptime.hm", { h: Math.floor(sec / 3600), m: Math.floor((sec % 3600) / 60) });
  return t("uptime.m", { m: Math.floor(sec / 60) });
}

// ---------------------------------------------------------------- DOM helpers
function el(tag, cls, text) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text !== undefined) e.textContent = text;
  return e;
}
function icon(name, cls = "") {
  const svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
  svg.setAttribute("class", `ic ${cls}`.trim());
  const use = document.createElementNS("http://www.w3.org/2000/svg", "use");
  use.setAttribute("href", `#i-${name}`);
  svg.append(use);
  return svg;
}
function rowButton(children, onClick) {
  const b = el("button", "row");
  b.type = "button";
  b.append(...children, icon("chev"));
  b.addEventListener("click", onClick);
  return b;
}

// ---------------------------------------------------------------- token / API
function getToken() {
  try { return sessionStorage.getItem(TOKEN_KEY) || localStorage.getItem(TOKEN_KEY) || ""; } catch { return ""; }
}
function setToken(token, remember) {
  try {
    sessionStorage.setItem(TOKEN_KEY, token);
    if (remember) localStorage.setItem(TOKEN_KEY, token); else localStorage.removeItem(TOKEN_KEY);
  } catch { /* armazenamento indisponível: token vale só nesta sessão */ }
}

class AuthError extends Error {}

async function api(path, opts = {}) {
  const headers = { "Content-Type": "application/json" };
  const token = getToken();
  if (token) headers.Authorization = `Bearer ${token}`;
  const resp = await fetch(path, { ...opts, headers });
  if (resp.status === 401) throw new AuthError(t("token.refused"));
  const body = await resp.json().catch(() => ({}));
  if (!resp.ok) throw new Error(body.error || `HTTP ${resp.status}`);
  return body;
}

function openTokenDialog(message) {
  const dlg = $("#token-dialog");
  const err = $("#token-error");
  err.hidden = !message;
  err.textContent = message || "";
  $("#token-input").value = getToken();
  try { $("#token-remember").checked = !!localStorage.getItem(TOKEN_KEY); } catch { /* ignore */ }
  if (!dlg.open) dlg.showModal();
}

// ---------------------------------------------------------------- carregamento
async function refresh() {
  const buttons = [$("#btn-refresh"), $("#btn-refresh-2")];
  buttons.forEach((b) => { b.disabled = true; });
  try {
    state.health = await fetch("../health").then((r) => r.json());
    renderHealth();
  } catch {
    state.health = null;
    renderOffline();
  }
  try {
    const data = await api(`../events?limit=${EVENTS_LIMIT}`);
    state.events = data.events || [];
    state.lastRefresh = new Date();
    renderAll();
    renderLastRefresh();
  } catch (e) {
    if (e instanceof AuthError) openTokenDialog(getToken() ? t("token.refused") : "");
    else $("#last-refresh").textContent = t("top.loadFailed", { msg: e.message });
  } finally {
    buttons.forEach((b) => { b.disabled = false; });
  }
}

function renderLastRefresh() {
  if (state.lastRefresh) $("#last-refresh").textContent = t("top.updated", { time: state.lastRefresh.toLocaleTimeString(locale()) });
}

// ---------------------------------------------------------------- render: serviço
function renderOffline() {
  $("#collect-alerts").replaceChildren();
  const pill = $("#service-status");
  pill.className = "status-pill offline";
  pill.textContent = t("service.offline");
  $("#hero-text").textContent = t("service.down");
  $("#hero-pill .dot").className = "dot bad";
  $("#acct-sub").textContent = t("acct.offline");
}

function renderHealth() {
  const h = state.health;
  const label = ["ok", "degraded", "unhealthy"].includes(h.status) ? t(`service.${h.status}`) : h.status;
  const pill = $("#service-status");
  pill.className = `status-pill ${h.status}`;
  pill.textContent = label;
  $("#hero-pill .dot").className = `dot ${h.status === "ok" ? "ok" : "bad"}`;
  $("#hero-text").textContent = t("hero.status", { label, v: h.version, uptime: uptime(h.uptime_seconds) });
  const targets = h.targets || [];
  const hosts = new Set(targets.map((x) => x.host)).size;
  $("#acct-sub").textContent = t("acct.summary", { v: h.version, hosts, targets: targets.length });
  renderCollectAlerts(targets);
}

// Diagnóstico de falhas de coleta a partir da mensagem de erro do serviço.
function diagnose(error, job) {
  const url = (error.match(/https?:\/\/[^\s"]+/) || [""])[0].replace(/[:.,]+$/, "");
  const local = /\/\/(localhost|127\.0\.0\.1|\[::1\])(:|\/)/.test(url);
  if (/connection refused/i.test(error) && job && job !== "node") {
    const svc = jobLabel(job);
    return {
      title: t("diag.exporterDown", { svc }),
      text: local ? t("diag.exporterLocal", { job, url }) : t("diag.exporterRemote", { url, svc }),
      cmd: local ? `sudo scripts/enable-service.sh ${svc}` : "",
    };
  }
  if (/connection refused/i.test(error)) {
    return {
      title: t("diag.nodeDown"),
      text: local ? t("diag.nodeLocal") : t("diag.nodeRemote", { url }),
      cmd: local ? "sudo apt install prometheus-node-exporter && sudo systemctl enable --now prometheus-node-exporter" : "",
    };
  }
  if (/no such host|server misbehaving/i.test(error)) {
    return { title: t("diag.dnsTitle"), text: t("diag.dnsText"), cmd: "sudoedit /etc/cognitive-driver/config.yaml && sudo systemctl restart cognitive-driver" };
  }
  if (/timeout|deadline exceeded/i.test(error)) {
    return { title: t("diag.timeoutTitle"), text: t("diag.timeoutText"), cmd: "" };
  }
  if (/status \d{3}/.test(error)) {
    return { title: t("diag.statusTitle"), text: t("diag.statusText"), cmd: "sudoedit /etc/cognitive-driver/config.yaml && sudo systemctl restart cognitive-driver" };
  }
  return { title: t("diag.otherTitle"), text: t("diag.otherText"), cmd: "journalctl -u cognitive-driver -n 50" };
}

function renderCollectAlerts(targets) {
  const box = $("#collect-alerts");
  box.replaceChildren();
  for (const tg of targets) {
    if (tg.ok || !tg.last_attempt) continue;
    const d = diagnose(tg.last_error || "", tg.job);
    const alert = el("div", "alert");
    alert.setAttribute("role", "alert");
    const since = tg.last_success
      ? t("alert.lastOk", { rel: relTime(tg.last_success), abs: absTime(tg.last_success) })
      : t("alert.neverOk");
    alert.append(icon("alert"), el("h3", "", t("alert.title", {
      host: tg.host, job: tg.job && tg.job !== "node" ? ` (${jobLabel(tg.job)})` : "", title: d.title.toLowerCase(),
    })));
    const body = el("div");
    body.append(el("p", "", since), el("p", "err", tg.last_error || ""));
    const fix = el("div", "fix");
    fix.append(el("p", "", d.text));
    if (d.cmd) {
      const cmd = el("div", "cmd");
      const btn = el("button", "btn");
      const label = el("span", "", t("alert.copy"));
      btn.type = "button";
      btn.append(icon("copy"), label);
      btn.addEventListener("click", async () => {
        try {
          await navigator.clipboard.writeText(d.cmd);
          label.textContent = t("alert.copied");
        } catch {
          label.textContent = t("alert.copyManual");
        }
      });
      cmd.append(el("code", "", d.cmd), btn);
      fix.append(cmd);
    }
    body.append(fix);
    alert.append(body);
    box.append(alert);
  }
}

// ---------------------------------------------------------------- render: dados
function renderAll() {
  renderKPIs();
  renderSidebar();
  renderColumns();
  renderChips();
  renderList();
  if (state.selectedId && $("#drawer").classList.contains("open")) renderDetail();
}

function openCount(fn) {
  return state.events.filter((e) => e.status === "open" && fn(e)).length;
}

function renderKPIs() {
  const count = (fn) => state.events.filter(fn).length;
  $("#kpi-open").textContent = count((e) => e.status === "open");
  const severe = openCount((e) => e.severity === "high" || e.severity === "critical");
  $("#kpi-severe").textContent = severe;
  $("#kpi-severe").classList.toggle("hot", severe > 0);
  $("#kpi-ack").textContent = count((e) => e.status === "acknowledged");
  $("#kpi-resolved").textContent = count((e) => e.status === "resolved");
  $("#kpi-fp").textContent = count((e) => e.status === "false_positive");
  $("#summary-scope").textContent = t("summary.scope", { n: state.events.length });
}

function renderSidebar() {
  $("#nav-open-count").textContent = openCount(() => true) || "";

  const recent = $("#nav-recent");
  recent.replaceChildren();
  const latest = state.events.slice(0, 5);
  if (latest.length === 0) recent.append(el("div", "sub-empty", t("nav.noEvents")));
  for (const e of latest) {
    const b = el("button", "sub-item");
    b.type = "button";
    b.append(el("strong", "", metricInfo(e).label), el("span", "", `${e.host} / ${e.severity} · ${relTime(e.timestamp)}`));
    b.addEventListener("click", () => { closeSidebar(); openDrawer(e.id); });
    recent.append(b);
  }

  const h = state.health || {};
  const drivers = $("#nav-drivers");
  drivers.replaceChildren();
  for (const d of h.drivers || []) {
    const a = navFilterItem(d.name, d.enabled ? "ok" : "off", openCount((e) => e.driver === d.name), "driver", d.name);
    a.title = `${t("driver.tooltip", { state: t(d.enabled ? "driver.enabled" : "driver.disabled"), v: d.version, contract: d.contract })}\n` +
      (d.metric_details || []).map((m) => (m.labels && m.labels[LANG]) || m.label).join(", ");
    drivers.append(a);
  }
  const hosts = $("#nav-hosts");
  hosts.replaceChildren();
  for (const tg of h.targets || []) {
    // Cada alvo conta os eventos dos drivers que o observam.
    const owners = new Set((h.drivers || []).filter((d) => d.job === tg.job).map((d) => d.name));
    const label = tg.job === "node" ? tg.host : `${tg.host} · ${jobLabel(tg.job)}`;
    const a = navFilterItem(label, tg.ok ? "ok" : "bad", openCount((e) => e.host === tg.host && owners.has(e.driver)), "host", tg.host);
    a.title = tg.ok ? t("target.last", { when: absTime(tg.last_success) }) : (tg.last_error || t("target.never"));
    hosts.append(a);
  }
}

function navFilterItem(label, dot, count, key, value) {
  const a = el("button", `nav-item${state.filters[key] === value ? " active" : ""}`);
  a.type = "button";
  a.append(el("span", `dot ${dot}`), el("span", "", label), el("span", "badge", count ? String(count) : ""));
  a.addEventListener("click", () => {
    setFilter(key, state.filters[key] === value ? "" : value);
    closeSidebar();
    scrollToEvents();
  });
  return a;
}

function driverIcon(name) {
  if (/nginx|apache|http|web/.test(name)) return "globe";
  if (/docker|container|kubernetes/.test(name)) return "box";
  if (/disk/.test(name)) return "server";
  return "cpu";
}

function renderColumns() {
  const sev = $("#col-severity");
  sev.replaceChildren();
  for (const s of SEVERITIES) {
    const n = openCount((e) => e.severity === s);
    const label = el("span", "row-label", s[0].toUpperCase() + s.slice(1));
    sev.append(rowButton([el("span", `sev-dot ${s}`), label, el("span", "row-count", String(n))], () => {
      state.filters.status = "open";
      setFilter("severity", s);
      scrollToEvents();
    }));
  }

  const drv = $("#col-drivers");
  drv.replaceChildren();
  const drivers = (state.health && state.health.drivers) || [];
  if (drivers.length === 0) drv.append(el("div", "row-empty", t("col.noDrivers")));
  for (const d of drivers) {
    const label = el("span", "row-label", d.name);
    if (!d.enabled) label.append(el("span", "muted", t("col.disabled")));
    drv.append(rowButton([icon(driverIcon(d.name)), label,
      el("span", "row-count", t("col.openCount", { n: openCount((e) => e.driver === d.name) }))], () => {
      setFilter("driver", d.name);
      scrollToEvents();
    }));
  }

  const rec = $("#col-recent");
  rec.replaceChildren();
  const latest = state.events.slice(0, 5);
  if (latest.length === 0) {
    rec.append(el("div", "row-empty", t("col.noRecent")));
  }
  for (const e of latest) {
    const label = el("span", "row-label");
    label.append(el("span", "muted", `${e.host} / `), document.createTextNode(metricInfo(e).label));
    label.title = `${e.severity} · ${absTime(e.timestamp)}`;
    rec.append(rowButton([icon("clock"), label], () => openDrawer(e.id)));
  }
}

function renderChips() {
  const box = $("#active-filters");
  box.replaceChildren();
  const labels = { driver: t("chip.driver"), host: t("chip.host"), q: t("chip.q") };
  for (const key of ["driver", "host", "q"]) {
    const v = state.filters[key];
    if (!v) continue;
    const chip = el("span", "chip", `${labels[key]}: ${v}`);
    const x = el("button");
    x.type = "button";
    x.setAttribute("aria-label", t("filter.remove", { name: labels[key] }));
    x.append(icon("x"));
    x.addEventListener("click", () => {
      if (key === "q") $("#q").value = "";
      setFilter(key, "");
    });
    chip.append(x);
    box.append(chip);
  }
}

function matches(e) {
  const f = state.filters;
  if (f.status && e.status !== f.status) return false;
  if (f.severity && e.severity !== f.severity) return false;
  if (f.driver && e.driver !== f.driver) return false;
  if (f.host && e.host !== f.host) return false;
  if (f.q) {
    const tr = Object.values(e.translations || {}).flatMap((x) => [x.metric_label, x.explanation, x.probable_cause, x.recommendation]);
    const hay = [metricInfo(e).label, e.metric, e.host, e.driver, e.severity,
      e.recommendation, e.explanation, e.probable_cause, e.feedback, ...tr].join(" ").toLowerCase();
    return f.q.toLowerCase().split(/\s+/).every((t) => hay.includes(t));
  }
  return true;
}


// Grupo de processos que mais consumia o recurso quando o evento foi registrado.
function responsible(e) {
  const p = e.attribution && e.attribution.processes && e.attribution.processes[0];
  return p || null;
}

// A métrica pede atribuição (attribute no YAML), mesmo que o evento não a tenha.
function wantsAttribution(e) {
  const h = healthMetric(e.metric.replace(/\{.*\}$/, ""));
  return Boolean(e.attribution || (h && h.attribute));
}

function wantsIO(e) {
  const h = healthMetric(e.metric.replace(/\{.*\}$/, ""));
  return Boolean(h && h.attribute === "io");
}

// Reemissões da mesma métrica com o evento ainda ativo viram ocorrências.
function occurrencesText(e) {
  const n = e.occurrences || 1;
  return n > 1 ? ` · ${t("list.occurrences", { n, when: relTime(e.last_seen) })}` : "";
}

function renderList() {
  const list = $("#event-list");
  const items = state.events.filter(matches);
  list.replaceChildren();
  renderPager(items.length);
  if (items.length === 0) {
    const msg = state.events.length === 0 ? t("list.none") : t("list.noMatch");
    list.append(el("div", "list-empty", msg));
    return;
  }
  const start = (state.page - 1) * PAGE_SIZE;
  for (const e of items.slice(start, start + PAGE_SIZE)) {
    const m = metricInfo(e);
    const b = el("button", `event${e.id === state.selectedId ? " selected" : ""}`);
    b.type = "button";
    b.setAttribute("role", "listitem");
    b.dataset.id = e.id;

    const title = el("div", "event-title");
    title.append(el("span", `sev ${e.severity}`, e.severity), el("span", "", `${m.label} · ${e.host}`));
    const main = el("div", "event-main");
    const meta = [e.driver, e.detector === "rule" ? t("list.rule", { rule: ruleText(e) }) : `z ${num(e.zscore, 1)}`,
      t("list.observed", { value: m.fmt(e.value) })];
    if (responsible(e)) meta.push(t("list.responsible", { name: responsible(e).group }));
    main.append(title, el("div", "event-reco", evText(e, "recommendation")),
      el("div", "event-meta", meta.join(" · ") + occurrencesText(e)));
    const side = el("div", "event-side");
    const time = el("span", "", relTime(e.timestamp));
    time.title = absTime(e.timestamp);
    side.append(el("span", `status ${e.status}`, statusLabel(e.status)), time, icon("chev"));

    b.append(el("span", `sev-dot ${e.severity}`), main, side);
    b.addEventListener("click", () => openDrawer(e.id));
    list.append(b);
  }
}

// Ajusta a página ao total (o auto-refresh pode encolher a lista) e desenha os controles.
function renderPager(total) {
  const pages = Math.max(1, Math.ceil(total / PAGE_SIZE));
  state.page = Math.min(Math.max(1, state.page), pages);
  const pager = $("#event-pager");
  pager.hidden = pages <= 1;
  if (pager.hidden) return;
  const start = (state.page - 1) * PAGE_SIZE;
  $("#pager-info").textContent = t("pager.info", {
    from: start + 1, to: Math.min(start + PAGE_SIZE, total), total, page: state.page, pages,
  });
  $("#pager-prev").disabled = state.page <= 1;
  $("#pager-next").disabled = state.page >= pages;
}

function goToPage(delta) {
  state.page += delta;
  renderList();
  scrollToEvents();
}

// ---------------------------------------------------------------- filtros / navegação
function setFilter(key, value) {
  state.filters[key] = value;
  state.page = 1;
  $("#f-status").value = state.filters.status;
  $("#f-severity").value = state.filters.severity;
  renderSidebar();
  renderChips();
  renderList();
}

function scrollToEvents() {
  $("#events-anchor").scrollIntoView({ behavior: "smooth", block: "start" });
}

function setActiveNav(name) {
  for (const a of document.querySelectorAll(".nav-item[data-nav]")) {
    a.classList.toggle("active", a.dataset.nav === name);
  }
}

function navigate(name) {
  const f = state.filters;
  if (name === "home") {
    Object.assign(f, { severity: "", driver: "", host: "", q: "" });
    $("#q").value = "";
    setFilter("status", "open");
    window.scrollTo({ top: 0, behavior: "smooth" });
  } else {
    setFilter("status", name === "open" ? "open" : "");
    scrollToEvents();
  }
  setActiveNav(name);
  closeSidebar();
}

// ---------------------------------------------------------------- drawer
function openDrawer(id) {
  state.selectedId = id;
  $("#detail").dataset.id = "";
  renderDetail();
  for (const n of document.querySelectorAll(".event")) n.classList.toggle("selected", n.dataset.id === id);
  const drawer = $("#drawer");
  drawer.classList.add("open");
  drawer.setAttribute("aria-hidden", "false");
  $("#drawer-scrim").classList.add("show");
  $("#drawer-close").focus();
  // Link direto para o evento (ex.: colar em um ticket ou no Slack).
  history.replaceState(null, "", `#event=${encodeURIComponent(id)}`);
}

function closeDrawer() {
  const drawer = $("#drawer");
  drawer.classList.remove("open");
  drawer.setAttribute("aria-hidden", "true");
  $("#drawer-scrim").classList.remove("show");
  state.selectedId = null;
  for (const n of document.querySelectorAll(".event.selected")) n.classList.remove("selected");
  history.replaceState(null, "", location.pathname + location.search);
}

function openFromHash() {
  const m = location.hash.match(/^#event=(.+)$/);
  if (!m) return;
  const id = decodeURIComponent(m[1]);
  if (state.events.some((e) => e.id === id)) openDrawer(id);
}

function renderDetail() {
  const panel = $("#detail");
  const e = state.events.find((x) => x.id === state.selectedId);
  if (!e) return;
  const m = metricInfo(e);
  const node = $("#tpl-detail").content.cloneNode(true);
  applyI18n(node);
  const q = (s) => node.querySelector(s);

  $("#drawer-crumbs").textContent = t("drawer.crumbs", { label: m.label, host: e.host });
  q(".sev").className = `sev ${e.severity}`;
  q(".sev").textContent = e.severity;
  q(".status").className = `status ${e.status}`;
  q(".status").textContent = statusLabel(e.status);
  q(".when").textContent = (e.occurrences || 1) > 1
    ? t("detail.whenRepeated", { abs: absTime(e.timestamp), rel: relTime(e.last_seen), n: e.occurrences })
    : t("detail.when", { abs: absTime(e.timestamp), rel: relTime(e.timestamp) });
  q(".detail-title").textContent = t("detail.title", { label: m.label, host: e.host });
  q(".detail-meta").textContent = `${e.driver} v${e.driver_version} · ${e.metric} · ${e.id}`;
  q(".recommendation").textContent = evText(e, "recommendation");
  q(".explanation").textContent = evText(e, "explanation");
  q(".cause").textContent = evText(e, "probable_cause");

  const isRule = e.detector === "rule";
  q(".z-card").hidden = isRule;
  q(".evidence-card").hidden = isRule;
  q(".rule-card").hidden = !isRule;
  if (isRule) {
    q(".rule-text").textContent = t("detail.ruleText", { value: m.fmt(e.value), rule: ruleText(e) });
  }
  const absZ = Math.abs(e.zscore);
  q(".zmarker").style.left = `${(Math.min(absZ, 15) / 15) * 100}%`;
  q(".zbar").setAttribute("aria-label", t("detail.zAria", { z: num(absZ, 1), sev: e.severity }));
  q(".zbar").title = `|z| = ${num(absZ, 1)}`;

  renderOwner(q, e);

  const tbody = q(".evidence-card tbody");
  const decision = closestWindow(e);
  for (const w of e.evidence || []) {
    const tr = el("tr", w.window === decision ? "decision" : "");
    tr.append(el("td", "", w.window + (w.window === decision ? t("detail.decision") : "")),
      el("td", "", m.fmt(w.mean)), el("td", "", m.fmt(w.stddev)),
      el("td", Math.abs(w.zscore) >= 3 ? "hot" : "", num(w.zscore, 1)));
    tbody.append(tr);
  }
  if (!isRule) q(".numbers").textContent = t("detail.numbers", {
    value: m.fmt(e.value), mean: m.fmt(e.baseline_mean), stddev: m.fmt(e.baseline_stddev), score: num(e.score, 3),
  });

  const ta = q("textarea");
  ta.value = e.feedback || "";
  q(".saved-feedback").textContent = e.feedback
    ? t("detail.lastFeedback", { when: absTime(e.updated_at), text: e.feedback })
    : "";
  for (const btn of node.querySelectorAll(".actions .btn")) {
    if (btn.dataset.status === e.status) btn.classList.add("active");
    btn.hidden = btn.dataset.status === "open" && e.status === "open";
    btn.addEventListener("click", () => updateStatus(e.id, btn.dataset.status, ta.value, panel));
  }

  // Preserva o rascunho de feedback durante o auto-refresh.
  const draft = panel.querySelector("textarea");
  if (draft && panel.dataset.id === e.id && draft.value !== (e.feedback || "")) ta.value = draft.value;
  panel.dataset.id = e.id;
  panel.replaceChildren(node);
}

function renderOwner(q, e) {
  const card = q(".owner-card");
  card.hidden = !wantsAttribution(e);
  if (card.hidden) return;
  const a = e.attribution;
  const top = responsible(e);
  if (!top) {
    card.classList.add("empty");
    q(".owner-note").textContent = t(wantsIO(e) ? "owner.noIO" : "owner.none");
    return;
  }
  const fmt = (v) => fmtUnit(a.unit, v);
  const share = (p) => `${num(p.share * 100, 0)}%`;
  q(".owner-name").textContent = top.group;
  const resource = ["cpu", "memory", "io"].includes(a.resource) ? t(`resource.${a.resource}`) : a.resource;
  q(".owner-value").textContent = t("owner.value", { value: fmt(top.value), share: share(top), resource });
  q(".owner-bar span").style.width = `${Math.min(100, top.share * 100)}%`;
  q(".owner-bar").setAttribute("aria-label", t("owner.aria", { name: top.group, share: share(top) }));
  const tbody = q(".owners tbody");
  a.processes.forEach((p, i) => {
    const tr = el("tr", i === 0 ? "top" : "");
    tr.append(el("td", "", p.group), el("td", "", fmt(p.value)), el("td", "", share(p)), el("td", "", p.procs ? String(p.procs) : "—"));
    tbody.append(tr);
  });
  q(".owner-note").textContent = t("owner.note", { source: a.source, when: absTime(a.at) });
}

// A janela de decisão é a que tem as mesmas média/desvio do evento.
function closestWindow(e) {
  let best = null, diff = Infinity;
  for (const w of e.evidence || []) {
    const d = Math.abs(w.mean - e.baseline_mean) + Math.abs(w.stddev - e.baseline_stddev);
    if (d < diff) { diff = d; best = w.window; }
  }
  return best;
}

async function updateStatus(id, status, feedback, panel) {
  const buttons = panel.querySelectorAll(".actions .btn");
  buttons.forEach((b) => { b.disabled = true; });
  try {
    const updated = await api(`../events/${encodeURIComponent(id)}`, {
      method: "PATCH",
      body: JSON.stringify({ status, feedback }),
    });
    const i = state.events.findIndex((e) => e.id === id);
    if (i >= 0) state.events[i] = updated;
    panel.dataset.id = "";
    renderAll();
  } catch (e) {
    if (e instanceof AuthError) openTokenDialog(t("token.refused"));
    else alert(t("action.failed", { msg: e.message }));
    buttons.forEach((b) => { b.disabled = false; });
  }
}

// ---------------------------------------------------------------- sidebar mobile
function openSidebar() {
  $("#sidebar").classList.add("open");
  $("#sidebar-scrim").classList.add("show");
}
function closeSidebar() {
  $("#sidebar").classList.remove("open");
  $("#sidebar-scrim").classList.remove("show");
}

// ---------------------------------------------------------------- eventos de UI
$("#btn-refresh").addEventListener("click", refresh);
$("#btn-refresh-2").addEventListener("click", refresh);
$("#btn-token").addEventListener("click", () => openTokenDialog(""));
$("#btn-menu").addEventListener("click", openSidebar);
$("#sidebar-scrim").addEventListener("click", closeSidebar);
$("#drawer-close").addEventListener("click", closeDrawer);
$("#drawer-scrim").addEventListener("click", closeDrawer);
$("#quick-search").addEventListener("click", () => { closeSidebar(); $("#q").focus(); });
$("#toggle-recent").addEventListener("click", (ev) => {
  const btn = ev.currentTarget;
  const open = btn.getAttribute("aria-expanded") !== "true";
  btn.setAttribute("aria-expanded", String(open));
  $("#nav-recent").hidden = !open;
});
for (const a of document.querySelectorAll("[data-nav]")) {
  a.addEventListener("click", (ev) => { ev.preventDefault(); navigate(a.dataset.nav); });
}
$("#token-form").addEventListener("submit", (ev) => {
  if (ev.submitter && ev.submitter.value === "save") {
    setToken($("#token-input").value.trim(), $("#token-remember").checked);
    refresh();
  }
});
$("#pager-prev").addEventListener("click", () => goToPage(-1));
$("#pager-next").addEventListener("click", () => goToPage(1));
$("#f-status").addEventListener("change", (ev) => setFilter("status", ev.target.value));
$("#f-severity").addEventListener("change", (ev) => setFilter("severity", ev.target.value));
let searchTimer;
$("#q").addEventListener("input", (ev) => {
  clearTimeout(searchTimer);
  searchTimer = setTimeout(() => setFilter("q", ev.target.value.trim()), 150);
});
$("#q").addEventListener("keydown", (ev) => { if (ev.key === "Enter") scrollToEvents(); });
document.addEventListener("keydown", (ev) => {
  if ((ev.ctrlKey || ev.metaKey) && ev.key.toLowerCase() === "k") {
    ev.preventDefault();
    $("#q").focus();
    $("#q").select();
  } else if (ev.key === "Escape") {
    if ($("#drawer").classList.contains("open")) closeDrawer();
    closeSidebar();
  }
});
document.addEventListener("visibilitychange", () => { if (!document.hidden) refresh(); });

window.addEventListener("hashchange", openFromHash);

// Troca de idioma: textos estáticos via applyI18n, o resto é redesenhado.
$("#lang").value = LANG;
$("#lang").addEventListener("change", (ev) => {
  setLang(ev.target.value);
  if (state.health) renderHealth(); else if (state.lastRefresh) renderOffline();
  renderAll();
  renderLastRefresh();
});
setLang(LANG);

refresh().then(openFromHash);
setInterval(() => { if (!document.hidden) refresh(); }, REFRESH_MS);
