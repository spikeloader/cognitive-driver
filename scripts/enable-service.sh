#!/usr/bin/env bash
# Ativa o monitoramento de um serviço: configura o exporter, valida e
# instala o driver correspondente em /etc/cognitive-driver/drivers.d.
#
# Uso: sudo scripts/enable-service.sh <nginx|apache|docker|php-fpm|swoole|cron> [opções]
#   --disable           remove o driver (o exporter e as configurações do
#                       serviço são mantidos)
#   --status-url URL    (apache) URL do mod_status; padrão http://127.0.0.1/server-status?auto
#   --metrics-url URL   (swoole) URL do /metrics da aplicação; padrão http://127.0.0.1:9501/metrics
#   --yes               (docker) não pede confirmação para reiniciar o dockerd
#
# O que cada serviço recebe:
#   nginx    stub_status em 127.0.0.1:8090 (conf.d) + prometheus-nginx-exporter em 127.0.0.1:9113
#   apache   mod_status (a2enmod status) + prometheus-apache-exporter em 127.0.0.1:9117
#   docker   "metrics-addr": "127.0.0.1:9323" em /etc/docker/daemon.json (reinicia o dockerd)
#   php-fpm  só o driver: exige o php-fpm_exporter já respondendo em 127.0.0.1:9253
#   swoole   só o driver: exige a aplicação expondo deploy/swoole/metrics.php
#   cron     só o driver (lê o node_exporter); os jobs são envolvidos com
#            scripts/wrap-cron-d.sh em cada host
#
# Todos os endpoints ficam apenas em loopback.
set -euo pipefail
cd "$(dirname "$0")/.."

NAME=cognitive-driver
ETC=/etc/$NAME
DROPIN=$ETC/drivers.d
SERVICE=${1:-}
shift || true
DISABLE=0 YES=0 STATUS_URL="http://127.0.0.1/server-status?auto" METRICS_URL=http://127.0.0.1:9501/metrics
while (( $# )); do
  case $1 in
    --disable) DISABLE=1 ;;
    --yes) YES=1 ;;
    --status-url) STATUS_URL=${2:?--status-url requer uma URL}; shift ;;
    --metrics-url) METRICS_URL=${2:?--metrics-url requer uma URL}; shift ;;
    *) echo "opção desconhecida: $1" >&2; exit 2 ;;
  esac
  shift
done

say()  { printf '\033[1m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[33mAVISO:\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[31mERRO:\033[0m %s\n' "$*" >&2; exit 1; }

case $SERVICE in
  nginx|apache|docker|php-fpm|swoole|cron) ;;
  -h|--help|"") sed -n '2,21p' "$0"; exit 0 ;;
  *) die "serviço desconhecido: $SERVICE (use nginx, apache, docker, php-fpm, swoole ou cron)" ;;
esac
[[ $EUID -eq 0 ]] || die "execute com sudo: sudo $0 $SERVICE"
[[ -f $ETC/config.yaml ]] || die "cognitive-driver não está instalado (sudo scripts/install.sh)"
TEMPLATE=deploy/drivers/$SERVICE.yaml
[[ -f $TEMPLATE ]] || die "modelo $TEMPLATE não encontrado"

wait_for() { # url padrão tentativas
  for _ in $(seq "${3:-20}"); do
    curl -sf -m 3 "$1" 2>/dev/null | grep -q "$2" && return 0
    sleep 1
  done
  return 1
}

restart_driver_service() {
  /usr/local/bin/$NAME -check -config $ETC/config.yaml >/dev/null || return 1
  systemctl restart $NAME
}

# ---------------------------------------------------------------- desativar
if (( DISABLE )); then
  if [[ -f $DROPIN/$SERVICE.yaml ]]; then
    rm -f "$DROPIN/$SERVICE.yaml"
    restart_driver_service
    say "driver de $SERVICE removido; o exporter continua instalado"
  else
    say "driver de $SERVICE não estava ativo"
  fi
  exit 0
fi

apt_install() {
  dpkg -s "$1" >/dev/null 2>&1 && return 0
  say "instalando $1"
  DEBIAN_FRONTEND=noninteractive apt-get install -y "$1" >/dev/null || die "falha ao instalar $1 (tente: sudo apt update)"
}

# Define ARGS do exporter em /etc/default, guardando o original uma vez.
set_exporter_args() { # pacote args
  local f=/etc/default/$1
  [[ -f $f && ! -f $f.cognitive-driver.bak ]] && cp -p "$f" "$f.cognitive-driver.bak"
  printf '# Gerado por cognitive-driver (scripts/enable-service.sh). Original: %s.cognitive-driver.bak\nARGS="%s"\n' "$f" "$2" > "$f"
  systemctl enable "$1" >/dev/null 2>&1
  systemctl restart "$1"
}

# ---------------------------------------------------------------- nginx
setup_nginx() {
  command -v nginx >/dev/null || die "nginx não está instalado neste host"
  nginx -V 2>&1 | grep -q http_stub_status_module || die "este nginx foi compilado sem o módulo stub_status"
  grep -rqs 'include /etc/nginx/conf.d/\*.conf' /etc/nginx/nginx.conf ||
    die "/etc/nginx/nginx.conf não inclui conf.d/*.conf; adicione o bloco do stub_status manualmente"

  local conf=/etc/nginx/conf.d/cognitive-driver-stub-status.conf
  if [[ ! -f $conf ]]; then
    if ss -ltn 2>/dev/null | grep -q '127.0.0.1:8090 '; then
      die "a porta 127.0.0.1:8090 já está em uso; libere-a ou configure o stub_status manualmente"
    fi
    say "habilitando stub_status do nginx em 127.0.0.1:8090"
    cat > "$conf" <<'CONF'
# Gerado por cognitive-driver: stub_status apenas em loopback para o exporter.
server {
    listen 127.0.0.1:8090;
    server_name localhost;
    access_log off;
    location = /stub_status {
        stub_status;
        allow 127.0.0.1;
        deny all;
    }
}
CONF
    if ! nginx -t >/dev/null 2>&1; then
      rm -f "$conf"
      die "nginx -t falhou com o stub_status; nada foi alterado (veja: sudo nginx -t)"
    fi
    systemctl reload nginx
  fi
  wait_for http://127.0.0.1:8090/stub_status "Active connections" 10 || die "stub_status não respondeu em 127.0.0.1:8090"

  apt_install prometheus-nginx-exporter
  set_exporter_args prometheus-nginx-exporter \
    "--nginx.scrape-uri=http://127.0.0.1:8090/stub_status --web.listen-address=127.0.0.1:9113"
  wait_for http://127.0.0.1:9113/metrics '^nginx_up 1' || die "prometheus-nginx-exporter não reportou nginx_up 1 (journalctl -u prometheus-nginx-exporter)"
}

# ---------------------------------------------------------------- apache
setup_apache() {
  command -v apache2ctl >/dev/null || die "Apache (apache2) não está instalado neste host"
  if ! apache2ctl -M 2>/dev/null | grep -q status_module; then
    say "habilitando mod_status"
    a2enmod status >/dev/null
    apache2ctl configtest >/dev/null 2>&1 || die "apache2ctl configtest falhou após habilitar mod_status"
    systemctl reload apache2
  fi
  wait_for "$STATUS_URL" "Total Accesses" 10 ||
    die "mod_status não respondeu em $STATUS_URL (o Apache escuta em outra porta? use --status-url)"

  apt_install prometheus-apache-exporter
  set_exporter_args prometheus-apache-exporter \
    "--scrape_uri=$STATUS_URL --web.listen-address=127.0.0.1:9117"
  wait_for http://127.0.0.1:9117/metrics '^apache_up 1' || die "prometheus-apache-exporter não reportou apache_up 1 (journalctl -u prometheus-apache-exporter)"
}

# ---------------------------------------------------------------- docker
DOCKER_METRICS=127.0.0.1:9323
setup_docker() {
  command -v docker >/dev/null || die "Docker não está instalado neste host"
  local f=/etc/docker/daemon.json current=""
  if [[ -f $f ]]; then
    command -v jq >/dev/null || die "jq é necessário para editar $f (sudo apt install jq)"
    current=$(jq -r '."metrics-addr" // empty' "$f") || die "$f não é um JSON válido"
  fi
  if [[ -n $current ]]; then
    DOCKER_METRICS=$current
    say "dockerd já expõe métricas em $current"
  else
    if (( !YES )); then
      warn "o dockerd será reiniciado para ativar as métricas."
      warn "containers sem restart policy (e sem live-restore) param e não voltam sozinhos."
      read -r -p "Continuar? [s/N] " ans
      [[ $ans == [sSyY]* ]] || die "cancelado"
    fi
    install -d -m 0755 /etc/docker
    local tmp
    tmp=$(mktemp)
    if [[ -f $f ]]; then
      cp -p "$f" "$f.cognitive-driver.bak"
      jq --arg a "$DOCKER_METRICS" '. + {"metrics-addr": $a}' "$f" > "$tmp"
    else
      printf '{\n  "metrics-addr": "%s"\n}\n' "$DOCKER_METRICS" > "$tmp"
    fi
    if dockerd --help 2>/dev/null | grep -q -- '--validate'; then
      dockerd --validate --config-file "$tmp" >/dev/null 2>&1 || { rm -f "$tmp"; die "configuração do dockerd inválida; nada foi alterado"; }
    fi
    install -m 0644 "$tmp" "$f"
    rm -f "$tmp"
    say "reiniciando o dockerd"
    systemctl restart docker
  fi
  wait_for "http://$DOCKER_METRICS/metrics" '^engine_daemon_' 30 ||
    die "dockerd não expôs métricas em $DOCKER_METRICS (journalctl -u docker)"
}

# ---------------------------------------------------------------- php-fpm
setup_php_fpm() {
  wait_for http://127.0.0.1:9253/metrics '^phpfpm_up{.*} 1' 5 ||
    die "php-fpm_exporter não reportou phpfpm_up 1 em 127.0.0.1:9253 (instale-o e configure pm.status_path no pool; veja deploy/drivers/php-fpm.yaml)"
}

# ---------------------------------------------------------------- swoole
setup_swoole() {
  wait_for "$METRICS_URL" '^swoole_workers_busy_ratio' 5 ||
    die "a aplicação não expôs as métricas do Swoole em $METRICS_URL (veja deploy/swoole/metrics.php ou use --metrics-url)"
}

# ---------------------------------------------------------------- cron
TEXTFILE_DIR=/var/lib/prometheus/node-exporter
setup_cron() {
  wait_for http://127.0.0.1:9100/metrics '^node_textfile_scrape_error' 5 ||
    warn "o textfile collector do node_exporter não parece ativo neste host (--collector.textfile.directory)"
  [[ -d $TEXTFILE_DIR ]] || warn "$TEXTFILE_DIR não existe; o cron-wrap grava as métricas lá (CRON_WRAP_DIR muda o destino)"
}

# ---------------------------------------------------------------- driver
say "configurando $SERVICE"
"setup_${SERVICE//-/_}"

host=$(sed -n 's/^    - host: *\([^ #]*\).*/\1/p' $ETC/config.yaml | head -1)
[[ -n $host ]] || host=$(hostname -s)
install -d -m 0755 "$DROPIN"
tmp=$(mktemp)
sed -e "s#host: local#host: $host#" "$TEMPLATE" > "$tmp"
[[ $SERVICE == docker ]] && sed -i "s#127.0.0.1:9323#$DOCKER_METRICS#" "$tmp"
[[ $SERVICE == swoole ]] && sed -i "s#http://127.0.0.1:9501/metrics#$METRICS_URL#" "$tmp"
install -m 0644 "$tmp" "$DROPIN/$SERVICE.yaml"
rm -f "$tmp"
if ! restart_driver_service; then
  rm -f "$DROPIN/$SERVICE.yaml"
  die "configuração do driver inválida; driver removido (o serviço continua com a configuração anterior)"
fi

if [[ $SERVICE == cron ]]; then
  # O driver de cron usa os alvos do node_exporter: não há coleta própria a confirmar.
  say "driver ai-cron-health ativo (arquivo: $DROPIN/$SERVICE.yaml)"
  echo "    Envolva os jobs em cada host: sudo scripts/wrap-cron-d.sh (simulação) e --apply."
  exit 0
fi

listen=$(sed -n 's/^  listen: *"\{0,1\}\([^" ]*\)"\{0,1\}.*/\1/p' $ETC/config.yaml)
api_host=${listen%:*}; [[ -z $api_host || $api_host == 0.0.0.0 ]] && api_host=127.0.0.1
health=""
for _ in $(seq 30); do
  health=$(curl -sf -m 3 "http://$api_host:${listen##*:}/health" 2>/dev/null || true)
  grep -q "\"job\":\"ai-$SERVICE-health\",\"url\":[^}]*\"ok\":true" <<<"$health" && break
  sleep 1
done
if grep -q "\"job\":\"ai-$SERVICE-health\",\"url\":[^}]*\"ok\":true" <<<"$health"; then
  say "driver ai-$SERVICE-health ativo e coletando (arquivo: $DROPIN/$SERVICE.yaml)"
  echo "    Regras (ex.: serviço fora do ar) valem de imediato; a detecção estatística aprende a baseline em ~5 min."
else
  warn "driver instalado, mas a primeira coleta ainda não confirmou sucesso; veja o painel ou: journalctl -u $NAME -n 20"
fi
