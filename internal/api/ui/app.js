// Painel do cognitive-driver: consome a API local (/health e /events).
// Todo conteúdo vindo da API é inserido via textContent.
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
const LEGACY_LABELS = {
  node_cpu_seconds_total: "CPU",
  node_memory_MemAvailable_bytes: "Memória disponível",
  node_disk_io_time_seconds_total: "Disco (I/O)",
  node_network_receive_bytes_total: "Rede (entrada)",
  node_network_transmit_bytes_total: "Rede (saída)",
};
const STATUS_LABEL = {
  open: "Aberto", acknowledged: "Reconhecido", resolved: "Resolvido", false_positive: "Falso positivo",
};

const state = {
  events: [],
  health: null,
  selectedId: null,
  page: 1,
  filters: { status: "open", severity: "", driver: "", host: "", q: "" },
};
const $ = (sel) => document.querySelector(sel);

// ---------------------------------------------------------------- formatação
function num(v, digits) {
  return Number(v).toLocaleString("pt-BR", { minimumFractionDigits: digits, maximumFractionDigits: digits });
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
  return `${num(v / 86400, 1)} dias`;
}
// Mesmas unidades do pacote explain (Go).
function fmtUnit(unit, v) {
  switch (unit) {
    case "cores": return `${num(v, 2)} núcleos`;
    case "bytes": return bytes(v);
    case "bytes_per_second": return `${bytes(v)}/s`;
    case "ratio": return `${num(v * 100, 1)}%`;
    case "per_second": return `${num(v, 2)}/s`;
    case "count": return num(v, 1);
    case "milliseconds": return `${num(v, 1)} ms`;
    case "seconds": return duration(v);
    case "bool": return v >= 0.5 ? "ativo (1)" : "inativo (0)";
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

// metricInfo aceita um evento (usa metric_label/unit gravados) ou um nome.
function metricInfo(x) {
  const name = typeof x === "string" ? x : x.metric;
  const h = healthMetric(name.replace(/\{.*\}$/, "")); // séries agrupadas: m{job="x"}
  const label = (typeof x === "object" && x.metric_label) || (h && h.label) || LEGACY_LABELS[name] || name;
  const unit = (typeof x === "object" && x.unit) || (h && h.unit) || LEGACY_UNITS[name] || "";
  return { label, unit, fmt: (v) => fmtUnit(unit, v) };
}

// Texto curto da regra de um evento: "esperado ativo (1)", "limite 90,0%".
function ruleText(e) {
  const [op, raw] = (e.rule || "").split(" ");
  const v = metricInfo(e).fmt(Number(raw));
  return { above: `limite máximo ${v}`, below: `mínimo ${v}`, equals: `esperado ${v}` }[op] || e.rule;
}

function jobLabel(job) {
  return job === "node" ? "host" : job.replace(/^ai-/, "").replace(/-health$/, "");
}

function relTime(iso) {
  const s = Math.round((Date.now() - new Date(iso)) / 1000);
  if (s < 60) return "agora";
  if (s < 3600) return `há ${Math.floor(s / 60)} min`;
  if (s < 86400) return `há ${Math.floor(s / 3600)} h`;
  return `há ${Math.floor(s / 86400)} d`;
}
function absTime(iso) {
  return new Date(iso).toLocaleString("pt-BR", { dateStyle: "short", timeStyle: "medium" });
}
function uptime(sec) {
  if (sec >= 86400) return `${Math.floor(sec / 86400)} d ${Math.floor((sec % 86400) / 3600)} h`;
  if (sec >= 3600) return `${Math.floor(sec / 3600)} h ${Math.floor((sec % 3600) / 60)} min`;
  return `${Math.floor(sec / 60)} min`;
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
  if (resp.status === 401) throw new AuthError("token ausente ou inválido");
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
    renderAll();
    $("#last-refresh").textContent = `Atualizado às ${new Date().toLocaleTimeString("pt-BR")}`;
  } catch (e) {
    if (e instanceof AuthError) openTokenDialog(getToken() ? "Token recusado pelo serviço." : "");
    else $("#last-refresh").textContent = `Falha ao carregar eventos: ${e.message}`;
  } finally {
    buttons.forEach((b) => { b.disabled = false; });
  }
}

// ---------------------------------------------------------------- render: serviço
function renderOffline() {
  $("#collect-alerts").replaceChildren();
  const pill = $("#service-status");
  pill.className = "status-pill offline";
  pill.textContent = "Sem conexão";
  $("#hero-text").textContent = "Serviço indisponível";
  $("#hero-pill .dot").className = "dot bad";
  $("#acct-sub").textContent = "desconectado";
}

function renderHealth() {
  const h = state.health;
  const label = { ok: "Operacional", degraded: "Coleta com falha", unhealthy: "Indisponível" }[h.status] || h.status;
  const pill = $("#service-status");
  pill.className = `status-pill ${h.status}`;
  pill.textContent = label;
  $("#hero-pill .dot").className = `dot ${h.status === "ok" ? "ok" : "bad"}`;
  $("#hero-text").textContent = `${label} · v${h.version} · no ar há ${uptime(h.uptime_seconds)}`;
  const targets = h.targets || [];
  const hosts = new Set(targets.map((t) => t.host)).size;
  $("#acct-sub").textContent = `v${h.version} · ${hosts} host${hosts === 1 ? "" : "s"} · ${targets.length} alvo${targets.length === 1 ? "" : "s"}`;
  renderCollectAlerts(targets);
}

// Diagnóstico de falhas de coleta a partir da mensagem de erro do serviço.
function diagnose(error, job) {
  const url = (error.match(/https?:\/\/[^\s"]+/) || [""])[0].replace(/[:.,]+$/, "");
  const local = /\/\/(localhost|127\.0\.0\.1|\[::1\])(:|\/)/.test(url);
  if (/connection refused/i.test(error) && job && job !== "node") {
    const svc = jobLabel(job);
    return {
      title: `O exporter do ${svc} não está respondendo`,
      text: local
        ? `O driver ${job} está ativo, mas nada responde em ${url}. Rode de novo a configuração do serviço (no diretório do projeto) ou desative o driver:`
        : `Nada está escutando em ${url}. Verifique o exporter do ${svc} nesse host.`,
      cmd: local ? `sudo scripts/enable-service.sh ${svc}` : "",
    };
  }
  if (/connection refused/i.test(error)) {
    return {
      title: "O node_exporter não está respondendo",
      text: local
        ? "Nada está escutando no endereço configurado. Instale o node_exporter (ou inicie o serviço, se já estiver instalado):"
        : `Nada está escutando em ${url}. Verifique se o node_exporter está rodando nesse host e se a porta está liberada.`,
      cmd: local ? "sudo apt install prometheus-node-exporter && sudo systemctl enable --now prometheus-node-exporter" : "",
    };
  }
  if (/no such host|server misbehaving/i.test(error)) {
    return { title: "Host do alvo não encontrado", text: "O nome do host não resolve via DNS. Corrija collector.targets[].url na configuração:", cmd: "sudoedit /etc/cognitive-driver/config.yaml && sudo systemctl restart cognitive-driver" };
  }
  if (/timeout|deadline exceeded/i.test(error)) {
    return { title: "Coleta excedeu o tempo limite", text: "O node_exporter demorou demais para responder ou há perda de pacotes na rede. Verifique a carga do host e collector.timeout.", cmd: "" };
  }
  if (/status \d{3}/.test(error)) {
    return { title: "O endpoint de métricas respondeu com erro", text: "O endereço responde, mas não é um /metrics válido. Confira a URL do alvo na configuração:", cmd: "sudoedit /etc/cognitive-driver/config.yaml && sudo systemctl restart cognitive-driver" };
  }
  return { title: "Falha na coleta de métricas", text: "Verifique os logs do serviço para mais detalhes:", cmd: "journalctl -u cognitive-driver -n 50" };
}

function renderCollectAlerts(targets) {
  const box = $("#collect-alerts");
  box.replaceChildren();
  for (const t of targets) {
    if (t.ok || !t.last_attempt) continue;
    const d = diagnose(t.last_error || "", t.job);
    const alert = el("div", "alert");
    alert.setAttribute("role", "alert");
    const since = t.last_success
      ? `Última coleta bem-sucedida ${relTime(t.last_success)} (${absTime(t.last_success)}).`
      : "Nenhuma coleta bem-sucedida desde que o serviço iniciou; a baseline não está sendo aprendida.";
    alert.append(icon("alert"), el("h3", "", `Coleta falhando em ${t.host}${t.job && t.job !== "node" ? ` (${jobLabel(t.job)})` : ""}: ${d.title.toLowerCase()}`));
    const body = el("div");
    body.append(el("p", "", since), el("p", "err", t.last_error || ""));
    const fix = el("div", "fix");
    fix.append(el("p", "", d.text));
    if (d.cmd) {
      const cmd = el("div", "cmd");
      const btn = el("button", "btn");
      const label = el("span", "", "Copiar");
      btn.type = "button";
      btn.append(icon("copy"), label);
      btn.addEventListener("click", async () => {
        try {
          await navigator.clipboard.writeText(d.cmd);
          label.textContent = "Copiado";
        } catch {
          label.textContent = "Selecione e copie";
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
  $("#summary-scope").textContent = `Últimos ${state.events.length} eventos`;
}

function renderSidebar() {
  $("#nav-open-count").textContent = openCount(() => true) || "";

  const recent = $("#nav-recent");
  recent.replaceChildren();
  const latest = state.events.slice(0, 5);
  if (latest.length === 0) recent.append(el("div", "sub-empty", "Nenhum evento ainda"));
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
    a.title = `${d.enabled ? "Habilitado" : "Desabilitado"} · v${d.version} · contrato ${d.contract}\n${(d.metric_details || []).map((m) => m.label).join(", ")}`;
    drivers.append(a);
  }
  const hosts = $("#nav-hosts");
  hosts.replaceChildren();
  for (const t of h.targets || []) {
    // Cada alvo conta os eventos dos drivers que o observam.
    const owners = new Set((h.drivers || []).filter((d) => d.job === t.job).map((d) => d.name));
    const label = t.job === "node" ? t.host : `${t.host} · ${jobLabel(t.job)}`;
    const a = navFilterItem(label, t.ok ? "ok" : "bad", openCount((e) => e.host === t.host && owners.has(e.driver)), "host", t.host);
    a.title = t.ok ? `Última coleta: ${absTime(t.last_success)}` : (t.last_error || "Sem coleta ainda");
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
  if (/docker|container/.test(name)) return "box";
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
  if (drivers.length === 0) drv.append(el("div", "row-empty", "Nenhum driver carregado"));
  for (const d of drivers) {
    const label = el("span", "row-label", d.name);
    if (!d.enabled) label.append(el("span", "muted", " (desabilitado)"));
    drv.append(rowButton([icon(driverIcon(d.name)), label,
      el("span", "row-count", `${openCount((e) => e.driver === d.name)} abertos`)], () => {
      setFilter("driver", d.name);
      scrollToEvents();
    }));
  }

  const rec = $("#col-recent");
  rec.replaceChildren();
  const latest = state.events.slice(0, 5);
  if (latest.length === 0) {
    rec.append(el("div", "row-empty", "Sem anomalias. Durante o aprendizado da baseline (~5 min) nenhum evento é gerado."));
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
  const labels = { driver: "Driver", host: "Host", q: "Busca" };
  for (const key of ["driver", "host", "q"]) {
    const v = state.filters[key];
    if (!v) continue;
    const chip = el("span", "chip", `${labels[key]}: ${v}`);
    const x = el("button");
    x.type = "button";
    x.setAttribute("aria-label", `Remover filtro ${labels[key]}`);
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
    const hay = [metricInfo(e).label, e.metric, e.host, e.driver, e.severity,
      e.recommendation, e.explanation, e.probable_cause, e.feedback].join(" ").toLowerCase();
    return f.q.toLowerCase().split(/\s+/).every((t) => hay.includes(t));
  }
  return true;
}

// Reemissões da mesma métrica com o evento ainda ativo viram ocorrências.
function occurrencesText(e) {
  const n = e.occurrences || 1;
  return n > 1 ? ` · ${n} ocorrências, última ${relTime(e.last_seen)}` : "";
}

function renderList() {
  const list = $("#event-list");
  const items = state.events.filter(matches);
  list.replaceChildren();
  renderPager(items.length);
  if (items.length === 0) {
    const msg = state.events.length === 0
      ? "Nenhuma anomalia registrada até agora."
      : "Nenhum evento corresponde aos filtros.";
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
    main.append(title, el("div", "event-reco", e.recommendation),
      el("div", "event-meta", `${e.driver} · ${e.detector === "rule" ? `regra: ${ruleText(e)}` : `z ${num(e.zscore, 1)}`} · observado ${m.fmt(e.value)}${occurrencesText(e)}`));
    const side = el("div", "event-side");
    const time = el("span", "", relTime(e.timestamp));
    time.title = absTime(e.timestamp);
    side.append(el("span", `status ${e.status}`, STATUS_LABEL[e.status] || e.status), time, icon("chev"));

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
  $("#pager-info").textContent =
    `${start + 1}–${Math.min(start + PAGE_SIZE, total)} de ${total} · página ${state.page} de ${pages}`;
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
  const q = (s) => node.querySelector(s);

  $("#drawer-crumbs").textContent = `Eventos / ${m.label} · ${e.host}`;
  q(".sev").className = `sev ${e.severity}`;
  q(".sev").textContent = e.severity;
  q(".status").className = `status ${e.status}`;
  q(".status").textContent = STATUS_LABEL[e.status] || e.status;
  q(".when").textContent = (e.occurrences || 1) > 1
    ? `Aberto ${absTime(e.timestamp)} · última ocorrência ${relTime(e.last_seen)} (${e.occurrences}×)`
    : `${absTime(e.timestamp)} (${relTime(e.timestamp)})`;
  q(".detail-title").textContent = `${m.label} em ${e.host}`;
  q(".detail-meta").textContent = `${e.driver} v${e.driver_version} · ${e.metric} · ${e.id}`;
  q(".recommendation").textContent = e.recommendation;
  q(".explanation").textContent = e.explanation;
  q(".cause").textContent = e.probable_cause;

  const isRule = e.detector === "rule";
  q(".z-card").hidden = isRule;
  q(".evidence-card").hidden = isRule;
  q(".rule-card").hidden = !isRule;
  if (isRule) {
    q(".rule-text").textContent = `Observado ${m.fmt(e.value)} · ${ruleText(e)}. Detecção por regra fixa: não depende de baseline.`;
  }
  const absZ = Math.abs(e.zscore);
  q(".zmarker").style.left = `${(Math.min(absZ, 15) / 15) * 100}%`;
  q(".zbar").setAttribute("aria-label", `${num(absZ, 1)} desvios-padrão (${e.severity})`);
  q(".zbar").title = `|z| = ${num(absZ, 1)}`;

  const tbody = q(".evidence tbody");
  const decision = closestWindow(e);
  for (const w of e.evidence || []) {
    const tr = el("tr", w.window === decision ? "decision" : "");
    tr.append(el("td", "", w.window + (w.window === decision ? " (decisão)" : "")),
      el("td", "", m.fmt(w.mean)), el("td", "", m.fmt(w.stddev)),
      el("td", Math.abs(w.zscore) >= 3 ? "hot" : "", num(w.zscore, 1)));
    tbody.append(tr);
  }
  if (!isRule) q(".numbers").textContent =
    `Observado ${m.fmt(e.value)} · baseline ${m.fmt(e.baseline_mean)} ± ${m.fmt(e.baseline_stddev)} · score ${num(e.score, 3)}`;

  const ta = q("textarea");
  ta.value = e.feedback || "";
  q(".saved-feedback").textContent = e.feedback
    ? `Último feedback (${absTime(e.updated_at)}): “${e.feedback}”`
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
    if (e instanceof AuthError) openTokenDialog("Token recusado pelo serviço.");
    else alert(`Falha ao atualizar o evento: ${e.message}`);
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

refresh().then(openFromHash);
setInterval(() => { if (!document.hidden) refresh(); }, REFRESH_MS);
