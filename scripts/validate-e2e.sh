#!/usr/bin/env bash
# Validação ponta a ponta do cognitive-driver em um host Linux com node_exporter.
#
# Modos:
#   (padrão)  valida o serviço instalado via systemd (scripts/install.sh)
#   --local   compila e roda o binário em uma porta separada, com estado
#             temporário, sem instalar nada e sem sudo
#
# Fases (~6–10 min no total):
#   1. Verifica /health, autenticação e /metrics.
#   2. Aguarda o fim do aprendizado da baseline (~5 min com warmup_points=20).
#   3. Gera carga real de CPU (e de disco) no host por STRESS_SECONDS.
#   4. Verifica se o evento de CPU foi gerado com evidência, causa e recomendação.
#   5. Valida GET /events/{id}, PATCH (falso positivo) e as métricas internas.
#   6. Verifica os logs JSON e se nenhuma ação foi executada (modo seguro).
#   7. Reinicia o serviço e confirma que a baseline foi restaurada do SQLite.
#
# Uso: scripts/validate-e2e.sh [--local] [--skip-disk] [--no-restart]
#   --skip-disk   não gera carga de disco
#   --no-restart  pula a fase 7 (no modo instalado ela usa sudo systemctl restart)
#
# Requer: curl, jq e node_exporter em 127.0.0.1:9100. Usa stress-ng se
# disponível; caso contrário gera carga com laços de shell e dd.
set -uo pipefail

cd "$(dirname "$0")/.."

SERVICE=cognitive-driver
WARMUP_TIMEOUT=${WARMUP_TIMEOUT:-480}
STRESS_SECONDS=${STRESS_SECONDS:-120}
EVENT_TIMEOUT=${EVENT_TIMEOUT:-180}
CPU_METRIC=node_cpu_seconds_total
DISK_METRIC=node_disk_io_time_seconds_total

LOCAL=0 DISK=1 RESTART=1
for arg in "$@"; do
  case $arg in
    --local) LOCAL=1 ;;
    --skip-disk) DISK=0 ;;
    --no-restart) RESTART=0 ;;
    -h|--help) sed -n '2,24p' "$0"; exit 0 ;;
    *) echo "opção desconhecida: $arg" >&2; exit 2 ;;
  esac
done

if [[ -t 1 ]]; then G=$'\e[32m' R=$'\e[31m' Y=$'\e[33m' B=$'\e[1m' N=$'\e[0m'; else G= R= Y= B= N=; fi
PASS=0 FAIL=0 WARN=0
T0=$(date +%s)
ts()   { printf '[%4ds]' $(( $(date +%s) - T0 )); }
step() { echo; echo "$(ts) ${B}== $* ==${N}"; }
info() { echo "$(ts)    $*"; }
pass() { PASS=$((PASS+1)); echo "$(ts) ${G}PASS${N} $*"; }
fail() { FAIL=$((FAIL+1)); echo "$(ts) ${R}FAIL${N} $*"; }
warn() { WARN=$((WARN+1)); echo "$(ts) ${Y}WARN${N} $*"; }
check() { local desc=$1; shift; if "$@"; then pass "$desc"; else fail "$desc"; fi; }

STRESS_PIDS=() TMP_DIR= RESP_FILE= CODE_FILE= LOCAL_DIR= LOCAL_PID=
cleanup() {
  for p in "${STRESS_PIDS[@]}"; do pkill -P "$p" 2>/dev/null; kill "$p" 2>/dev/null; done
  [[ -n $TMP_DIR ]] && rm -rf "$TMP_DIR"
  rm -f "$RESP_FILE" "$CODE_FILE"
  if [[ -n $LOCAL_PID ]]; then kill "$LOCAL_PID" 2>/dev/null; wait "$LOCAL_PID" 2>/dev/null; fi
  [[ -n $LOCAL_DIR ]] && rm -rf "$LOCAL_DIR"
}
trap cleanup EXIT
trap 'echo; echo interrompido; exit 130' INT TERM

RESP_FILE=$(mktemp) CODE_FILE=$(mktemp)
for bin in curl jq; do
  command -v "$bin" >/dev/null || { echo "dependência ausente: $bin" >&2; exit 2; }
done
curl -sf -m 3 http://127.0.0.1:9100/metrics -o /dev/null ||
  { echo "node_exporter não responde em 127.0.0.1:9100 (sudo apt install prometheus-node-exporter)" >&2; exit 2; }

# ---------------------------------------------------------------- modos
# Cada modo define API, MICRO_AI_TOKEN, service_logs e service_restart.
if (( LOCAL )); then
  API=${API:-http://127.0.0.1:18088}
  LOCAL_DIR=$(mktemp -d)
  export MICRO_AI_TOKEN=validate-$(date +%s)-$RANDOM
  sed -e "s#^\(  data_dir:\) .*#\1 $LOCAL_DIR/data#" \
      -e "s#^\(  listen:\) .*#\1 \"${API#http://}\"#" configs/config.yaml > "$LOCAL_DIR/config.yaml"
  make -s build >/dev/null || { echo "falha ao compilar" >&2; exit 1; }
  local_start() {
    bin/$SERVICE -config "$LOCAL_DIR/config.yaml" >>"$LOCAL_DIR/service.log" 2>&1 &
    LOCAL_PID=$!
  }
  service_logs() { cat "$LOCAL_DIR/service.log"; }
  service_restart() { kill "$LOCAL_PID"; wait "$LOCAL_PID" 2>/dev/null; local_start; }
  local_start
  MODE_DESC="binário local (pid $LOCAL_PID, estado em $LOCAL_DIR)"
else
  systemctl is-active --quiet $SERVICE ||
    { echo "serviço $SERVICE não está ativo. Instale com: make build && sudo make install" >&2; exit 2; }
  listen=$(sed -n 's/^  listen: *"\{0,1\}\([^" ]*\)"\{0,1\}.*/\1/p' /etc/$SERVICE/config.yaml 2>/dev/null)
  host=${listen%:*}; [[ -z $host || $host == 0.0.0.0 ]] && host=127.0.0.1
  API=${API:-http://$host:${listen##*:}}
  if [[ -z ${MICRO_AI_TOKEN:-} ]]; then
    if [[ -r /etc/$SERVICE/env ]]; then
      MICRO_AI_TOKEN=$(sed -n 's/^MICRO_AI_TOKEN=//p' /etc/$SERVICE/env)
    else
      echo "lendo o token em /etc/$SERVICE/env (sudo):"
      MICRO_AI_TOKEN=$(sudo sed -n 's/^MICRO_AI_TOKEN=//p' /etc/$SERVICE/env) || exit 2
    fi
  fi
  SINCE=$(date '+%Y-%m-%d %H:%M:%S')
  service_logs() { journalctl -u $SERVICE --since "$SINCE" -o cat --no-pager 2>/dev/null | grep '^{'; }
  service_restart() { sudo systemctl restart $SERVICE; }
  MODE_DESC="serviço systemd $SERVICE ($(systemctl show -p ActiveEnterTimestamp --value $SERVICE | cut -d' ' -f2-3))"
fi
AUTH="Authorization: Bearer $MICRO_AI_TOKEN"

# Helpers HTTP: imprimem o corpo; o status HTTP fica em $CODE_FILE (lido
# com `code`), pois as chamadas costumam rodar em subshell via $(...).
code() { cat "$CODE_FILE" 2>/dev/null; }
http() { # método caminho [corpo] [auth: 1|0]
  local args=(-s -m 10 -o "$RESP_FILE" -w '%{http_code}' -X "$1" -H 'Content-Type: application/json')
  [[ ${4:-1} == 1 ]] && args+=(-H "$AUTH")
  [[ -n ${3:-} ]] && args+=(--data "$3")
  curl "${args[@]}" "$API$2" >"$CODE_FILE" 2>/dev/null || echo 000 >"$CODE_FILE"
  cat "$RESP_FILE" 2>/dev/null
}
get()   { http GET "$1" "" "${2:-1}"; }
post()  { http POST "$1" "$2"; }
patch() { http PATCH "$1" "$2"; }
jqok()  { jq -e "$2" >/dev/null 2>&1 <<<"$1"; } # json filtro
metric() { # soma (inteira) das séries de uma métrica do /metrics, com filtro opcional por substring
  curl -s -m 10 "$API/metrics" | grep "^$1[{ ]" | grep -F -- "${2:-}" | awk '{s+=$NF} END {printf "%.0f", s}'
}
learning_state() { # imprime true/false/none para o aprendizado de uma métrica
  local body
  body=$(post /predict "{\"metric\":\"$1\",\"value\":0}")
  case $(code) in
    200) jq -r '.learning' <<<"$body" ;;
    *)   echo none ;;
  esac
}

# --------------------------------------------------------------------------
step "1. Verificando o serviço"
info "alvo: $MODE_DESC em $API"

ok=0
for _ in $(seq 60); do
  health=$(get /health 0)
  if [[ $(code) == 200 ]] && jqok "$health" '.targets[0].ok == true'; then ok=1; break; fi
  sleep 2
done
check "/health responde 200 com coleta OK" test $ok = 1
check "/health lista 2 drivers no contrato v1 em observe_only" \
  jqok "$health" '[.drivers[] | select(.contract=="v1" and .mode=="observe_only" and .enabled)] | length == 2'

get /events 0 >/dev/null
check "/events sem token retorna 401" test "$(code)" = 401
get /events >/dev/null
check "/events com token retorna 200" test "$(code)" = 200
post /predict '{"metric":"x","value":1,"campo_extra":true}' >/dev/null
check "/predict rejeita JSON inválido com 400" test "$(code)" = 400
metrics_body=$(curl -s -m 10 "$API/metrics")
check "/metrics expõe métricas internas e de runtime" \
  bash -c 'grep -q "^cognitive_driver_scrapes_total" <<<"$0" && grep -q "^go_memstats_alloc_bytes" <<<"$0"' "$metrics_body"

# --------------------------------------------------------------------------
step "2. Aguardando o aprendizado da baseline (até ${WARMUP_TIMEOUT}s)"
deadline=$(( $(date +%s) + WARMUP_TIMEOUT ))
last=
while (( $(date +%s) < deadline )); do
  state=$(learning_state $CPU_METRIC)
  n=$(metric cognitive_driver_predictions_total "metric=\"$CPU_METRIC\"")
  if [[ "$state/$n" != "$last" ]]; then info "CPU: learning=$state, previsões=$n/20"; last="$state/$n"; fi
  [[ $state == false ]] && break
  sleep 15
done
check "baseline de CPU pronta (learning=false)" test "$state" = false
(( DISK )) && { [[ $(learning_state $DISK_METRIC) == false ]] && pass "baseline de disco pronta" || warn "baseline de disco ainda em aprendizado"; }

pre=$(post /predict "{\"metric\":\"$CPU_METRIC\",\"value\":0}")
info "baseline CPU (5m): média=$(jq -r '.baseline.mean' <<<"$pre") núcleos, desvio=$(jq -r '.baseline.stddev' <<<"$pre")"
events_before=$(get "/events?limit=500" | jq '.count')
info "eventos existentes antes da carga: $events_before"

# --------------------------------------------------------------------------
step "3. Gerando carga por ${STRESS_SECONDS}s"
STRESS_START=$(date -u +%Y-%m-%dT%H:%M:%SZ)
CORES=$(nproc)
if (( DISK )); then TMP_DIR=$(mktemp -d "${TMPDIR:-/var/tmp}/cd-validate.XXXXXX"); fi
if command -v stress-ng >/dev/null; then
  info "stress-ng: CPU em $CORES núcleos"
  stress-ng --cpu 0 --timeout "${STRESS_SECONDS}s" --quiet & STRESS_PIDS+=($!)
  if (( DISK )); then
    info "stress-ng: I/O de disco em $TMP_DIR"
    stress-ng --hdd 2 --hdd-bytes 256M --hdd-opts direct --temp-path "$TMP_DIR" \
      --timeout "${STRESS_SECONDS}s" --quiet & STRESS_PIDS+=($!)
  fi
else
  info "stress-ng ausente; laços de shell em $CORES núcleos"
  end=$(( $(date +%s) + STRESS_SECONDS ))
  for _ in $(seq "$CORES"); do
    ( while (( $(date +%s) < end )); do :; done ) & STRESS_PIDS+=($!)
  done
  if (( DISK )); then
    info "dd com escrita direta em $TMP_DIR"
    ( while (( $(date +%s) < end )); do
        dd if=/dev/zero of="$TMP_DIR/f" bs=1M count=256 oflag=direct status=none
      done ) & STRESS_PIDS+=($!)
  fi
fi

# --------------------------------------------------------------------------
step "4. Aguardando eventos (até ${EVENT_TIMEOUT}s)"
deadline=$(( $(date +%s) + EVENT_TIMEOUT ))
cpu_event= disk_event=
while (( $(date +%s) < deadline )); do
  cpu_event=$(get "/events?metric=$CPU_METRIC&since=$STRESS_START" | jq -c '.events[0] // empty')
  (( DISK )) && [[ -z $disk_event ]] && disk_event=$(get "/events?metric=$DISK_METRIC&since=$STRESS_START" | jq -c '.events[0] // empty')
  if [[ -n $cpu_event ]] && { (( !DISK )) || [[ -n $disk_event ]]; }; then break; fi
  info "aguardando... pontos anômalos de CPU até agora: $(metric cognitive_driver_anomalous_points_total "metric=\"$CPU_METRIC\"")"
  sleep 10
done

if [[ -n $cpu_event ]]; then
  pass "evento de CPU gerado após pico de carga"
  info "$(jq -r '"severidade=\(.severity) z=\(.zscore|.*10|round/10) score=\(.score)"' <<<"$cpu_event")"
  info "explicação: $(jq -r .explanation <<<"$cpu_event")"
  check "evento tem z-score >= 3" jqok "$cpu_event" '.zscore >= 3'
  check "evento tem severidade válida" jqok "$cpu_event" '.severity | IN("low","medium","high","critical")'
  check "evento tem evidência numérica (3 janelas)" jqok "$cpu_event" '(.evidence | length) == 3 and .baseline_stddev > 0'
  check "evento tem explicação, causa provável e recomendação" \
    jqok "$cpu_event" '(.explanation|length)>0 and (.probable_cause|length)>0 and (.recommendation|length)>0'
  check "evento em observe_only com status open" jqok "$cpu_event" '.mode=="observe_only" and .status=="open"'
  check "evento tem id UUID e timestamp RFC3339" \
    jqok "$cpu_event" '(.id|test("^[0-9a-f-]{36}$")) and (.timestamp|test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T"))'
else
  fail "nenhum evento de CPU em ${EVENT_TIMEOUT}s (veja os logs do serviço: grep $CPU_METRIC)"
fi
if (( DISK )); then
  if [[ -n $disk_event ]]; then
    pass "evento de disco gerado: $(jq -r '"\(.severity), z=\(.zscore|.*10|round/10)"' <<<"$disk_event")"
  else
    warn "nenhum evento de disco (depende da baseline de I/O do host; não é falha do serviço)"
  fi
fi

# --------------------------------------------------------------------------
step "5. API de eventos e métricas internas"
if [[ -n $cpu_event ]]; then
  id=$(jq -r .id <<<"$cpu_event")
  detail=$(get "/events/$id")
  if [[ $(code) == 200 ]] && jqok "$detail" ".id == \"$id\""; then pass "GET /events/{id} retorna o evento"; else fail "GET /events/{id} retorna o evento ($(code))"; fi
  get /events/00000000-0000-0000-0000-000000000000 >/dev/null
  check "GET /events/{id} inexistente retorna 404" test "$(code)" = 404

  fp_before=$(metric cognitive_driver_false_positives_total)
  upd=$(patch "/events/$id" '{"status":"false_positive","feedback":"teste de carga do validate-e2e.sh"}')
  if [[ $(code) == 200 ]] && jqok "$upd" '.status=="false_positive" and (.feedback|length)>0'; then
    pass "PATCH /events/{id} registra falso positivo"
  else
    fail "PATCH /events/{id} registra falso positivo ($(code))"
  fi
  fp_after=$(metric cognitive_driver_false_positives_total)
  check "false_positives_total incrementou ($fp_before → $fp_after)" test "$fp_after" -gt "$fp_before"
fi
check "events_total > 0"                 test "$(metric cognitive_driver_events_total)" -gt 0
check "inference_duration_seconds registrado" test "$(metric cognitive_driver_inference_duration_seconds_count)" -gt 0
check "nenhuma falha de persistência"    test "$(metric cognitive_driver_store_errors_total)" -eq 0
info "memória do processo: $(( $(metric process_resident_memory_bytes) / 1024 / 1024 )) MiB"

# --------------------------------------------------------------------------
step "6. Logs estruturados e modo seguro"
logs=$(service_logs)
check "todas as linhas de log são JSON válido" jqok "$logs" .
check "log registra previsões (prediction)"    grep -q '"msg":"prediction"' <<<"$logs"
check "log registra o evento (anomaly_event)"  grep -q '"msg":"anomaly_event"' <<<"$logs"
check "toda ação foi bloqueada pela política (action_blocked)" grep -q '"msg":"action_blocked"' <<<"$logs"
events_total=$(metric cognitive_driver_events_total)
blocked=$(metric cognitive_driver_actions_blocked_total)
check "actions_blocked_total ($blocked) == events_total ($events_total)" test "$blocked" = "$events_total"

# --------------------------------------------------------------------------
step "7. Persistência da baseline após restart"
if (( RESTART )); then
  service_restart
  sleep 1
  for _ in $(seq 30); do get /health 0 >/dev/null; [[ $(code) == 200 ]] && break; sleep 1; done
  check "serviço volta após restart" test "$(code)" = 200
  check "baseline de CPU restaurada (sem novo aprendizado)" test "$(learning_state $CPU_METRIC)" = false
  check "eventos preservados no SQLite" test "$(get "/events?limit=500" | jq '.count')" -gt "$events_before"
else
  info "pulado (--no-restart)"
fi

# --------------------------------------------------------------------------
echo
echo "${B}Resumo:${N} ${G}$PASS passaram${N}, ${R}$FAIL falharam${N}, ${Y}$WARN avisos${N} em $(( $(date +%s) - T0 ))s"
(( FAIL == 0 ))
