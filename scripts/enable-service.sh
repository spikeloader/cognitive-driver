#!/usr/bin/env bash
# Ativa o monitoramento de um serviço: configura o exporter, valida e
# instala o driver correspondente em /etc/cognitive-driver/drivers.d.
#
# Uso: sudo scripts/enable-service.sh <serviço> [opções]
#   serviços: nginx apache docker php-fpm swoole cron processes
#             redis memcached postgresql mysql
#   --disable           remove o driver (o exporter e as configurações do
#                       serviço são mantidos)
#   --status-url URL    (apache) URL do mod_status; padrão http://127.0.0.1/server-status?auto
#   --metrics-url URL   (swoole) URL do /metrics da aplicação; padrão http://127.0.0.1:9501/metrics
#   --listen ADDR       (processes) endereço do process-exporter; padrão 127.0.0.1:9256
#                       (use 0.0.0.0:9256 quando o cognitive-driver central coleta este host)
#   --with-io           (processes) concede CAP_SYS_PTRACE e CAP_DAC_READ_SEARCH ao
#                       exporter (com sandbox) para ler o I/O de disco de processos de
#                       outros usuários (eventos de disco)
#   --redis-addr URL    (redis) padrão redis://127.0.0.1:6379; com requirepass, passe a
#                       senha em REDIS_PASSWORD (sudo REDIS_PASSWORD=... scripts/...)
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
#   processes  prometheus-process-exporter agrupando processos por nome: os
#            eventos de CPU, memória e disco passam a indicar o processo
#            responsável (não instala driver)
#   redis      prometheus-redis-exporter em 127.0.0.1:9121
#   memcached  só o driver: exige o memcached_exporter já respondendo em 127.0.0.1:9150
#   postgresql prometheus-postgres-exporter em 127.0.0.1:9187 + role "prometheus"
#            (pg_monitor, autenticação peer pelo socket, sem senha)
#   mysql      prometheus-mysqld-exporter em 127.0.0.1:9104 + usuário prometheus@localhost
#            (auth_socket/unix_socket, sem senha; PROCESS, REPLICATION CLIENT, SELECT)
#
# Todos os endpoints ficam apenas em loopback.
set -euo pipefail
cd "$(dirname "$0")/.."

NAME=cognitive-driver
ETC=/etc/$NAME
DROPIN=$ETC/drivers.d
SERVICE=${1:-}
shift || true
DISABLE=0 YES=0 STATUS_URL="http://127.0.0.1/server-status?auto" METRICS_URL=http://127.0.0.1:9501/metrics LISTEN=127.0.0.1:9256 WITH_IO=0 REDIS_ADDR=redis://127.0.0.1:6379
while (( $# )); do
  case $1 in
    --disable) DISABLE=1 ;;
    --yes) YES=1 ;;
    --status-url) STATUS_URL=${2:?--status-url requer uma URL}; shift ;;
    --metrics-url) METRICS_URL=${2:?--metrics-url requer uma URL}; shift ;;
    --listen) LISTEN=${2:?--listen requer um endereço}; shift ;;
    --with-io) WITH_IO=1 ;;
    --redis-addr) REDIS_ADDR=${2:?--redis-addr requer uma URL}; shift ;;
    *) echo "opção desconhecida: $1" >&2; exit 2 ;;
  esac
  shift
done

say()  { printf '\033[1m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[33mAVISO:\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[31mERRO:\033[0m %s\n' "$*" >&2; exit 1; }

case $SERVICE in
  nginx|apache|docker|php-fpm|swoole|cron|processes|redis|memcached|postgresql|mysql) ;;
  -h|--help|"") sed -n '2,38p' "$0"; exit 0 ;;
  *) die "serviço desconhecido: $SERVICE (veja --help)" ;;
esac
[[ $EUID -eq 0 ]] || die "execute com sudo: sudo $0 $SERVICE"
[[ -f $ETC/config.yaml ]] || die "cognitive-driver não está instalado (sudo scripts/install.sh)"
TEMPLATE=deploy/drivers/$SERVICE.yaml
[[ $SERVICE == processes || -f $TEMPLATE ]] || die "modelo $TEMPLATE não encontrado"

wait_for() { # url padrão tentativas
  for _ in $(seq "${3:-20}"); do
    # Sem pipe: com pipefail, o grep -q fecharia o pipe no 1º match e o curl
    # (saídas grandes, como a do process-exporter) sairia com erro de SIGPIPE.
    local out
    out=$(curl -sf -m 3 "$1" 2>/dev/null) && grep -q "$2" <<<"$out" && return 0
    sleep 1
  done
  return 1
}

restart_driver_service() {
  /usr/local/bin/$NAME -check -config $ETC/config.yaml >/dev/null || return 1
  systemctl restart $NAME
}

PROCESS_DROPIN=/etc/systemd/system/prometheus-process-exporter.service.d/cognitive-driver.conf

# ---------------------------------------------------------------- desativar
if (( DISABLE )) && [[ $SERVICE == processes ]]; then
  systemctl disable --now prometheus-process-exporter 2>/dev/null || true
  rm -f "$PROCESS_DROPIN" && systemctl daemon-reload
  say "process-exporter parado; os próximos eventos saem sem o processo responsável"
  exit 0
fi
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
# O 3º argumento são linhas extras (variáveis de ambiente do exporter); com
# segredo, o arquivo fica legível só para root e o grupo prometheus.
set_exporter_args() { # pacote args [linhas-extras]
  local f=/etc/default/$1
  [[ -f $f && ! -f $f.cognitive-driver.bak ]] && cp -p "$f" "$f.cognitive-driver.bak"
  printf '# Gerado por cognitive-driver (scripts/enable-service.sh). Original: %s.cognitive-driver.bak\nARGS="%s"\n%s' "$f" "$2" "${3:+$3$'\n'}" > "$f"
  if [[ -n ${3:-} ]]; then chown root:prometheus "$f" && chmod 0640 "$f"; fi
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

# ---------------------------------------------------------------- processes
# Um grupo por nome de comando (comm): "php-fpm8.3", "postgres", "stress-ng".
PROCESS_CONFIG=/etc/prometheus/process-exporter-cognitive-driver.yml
setup_processes() {
  # /proc/<pid>/io é 0400 do dono do processo: ler o de outros usuários exige
  # CAP_DAC_READ_SEARCH (permissão do arquivo) e CAP_SYS_PTRACE (checagem de
  # ptrace do kernel). Sem isso o I/O vem zerado e eventos de disco ficam sem
  # responsável. Opt-in, e com sandbox: a 1ª capability permite ler qualquer
  # arquivo, então o exporter só enxerga o necessário (/proc).
  if (( WITH_IO )); then
    install -d -m 0755 "${PROCESS_DROPIN%/*}"
    cat > "$PROCESS_DROPIN" <<'UNIT'
# Gerado por cognitive-driver (enable-service.sh processes --with-io).
[Service]
AmbientCapabilities=CAP_SYS_PTRACE CAP_DAC_READ_SEARCH
CapabilityBoundingSet=CAP_SYS_PTRACE CAP_DAC_READ_SEARCH
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
InaccessiblePaths=-/etc/shadow -/etc/gshadow -/etc/sudoers -/etc/sudoers.d -/etc/ssh -/etc/cognitive-driver
UNIT
    say "process-exporter com CAP_SYS_PTRACE e CAP_DAC_READ_SEARCH em sandbox (I/O de disco por processo)"
  else
    rm -f "$PROCESS_DROPIN"
  fi
  systemctl daemon-reload
  apt_install prometheus-process-exporter
  install -d -m 0755 /etc/prometheus
  cat > "$PROCESS_CONFIG" <<'CONF'
# Gerado por cognitive-driver (scripts/enable-service.sh processes).
process_names:
  - name: "{{.Comm}}"
    cmdline:
      - '.+'
CONF
  chmod 0644 "$PROCESS_CONFIG"
  # Sem métricas por thread: a saída cai de ~10 mil para ~2 mil linhas.
  set_exporter_args prometheus-process-exporter "-config.path=$PROCESS_CONFIG -web.listen-address=$LISTEN -threads=false"
  probe=$LISTEN # global: usado na checagem de I/O após o setup
  [[ ${probe%:*} == 0.0.0.0 ]] && probe=127.0.0.1:${LISTEN##*:}
  wait_for "http://$probe/metrics" '^namedprocess_namegroup_cpu_seconds_total' ||
    die "process-exporter não publicou métricas em $LISTEN (journalctl -u prometheus-process-exporter)"
}

# ---------------------------------------------------------------- redis
setup_redis() {
  command -v redis-server >/dev/null || die "Redis não está instalado neste host"
  local env=""
  if [[ -n ${REDIS_PASSWORD:-} ]]; then
    [[ $REDIS_PASSWORD != *[\"\$\`\\]* ]] || die "REDIS_PASSWORD não pode conter aspas, \$, \` ou \\"
    env="REDIS_PASSWORD=\"$REDIS_PASSWORD\""
  fi
  apt_install prometheus-redis-exporter
  set_exporter_args prometheus-redis-exporter "-redis.addr=$REDIS_ADDR -web.listen-address=127.0.0.1:9121" "$env"
  wait_for http://127.0.0.1:9121/metrics '^redis_up 1' ||
    die "prometheus-redis-exporter não reportou redis_up 1 (Redis com senha? use REDIS_PASSWORD=...; veja journalctl -u prometheus-redis-exporter)"
}

# ---------------------------------------------------------------- memcached
setup_memcached() {
  wait_for http://127.0.0.1:9150/metrics '^memcached_up 1' 5 ||
    die "memcached_exporter não reportou memcached_up 1 em 127.0.0.1:9150 (instale-o: github.com/prometheus/memcached_exporter)"
}

# ---------------------------------------------------------------- postgresql
# Role sem senha: o exporter roda como o usuário prometheus e entra pelo
# socket local com autenticação peer (padrão do pg_hba.conf no Debian/Ubuntu).
setup_postgresql() {
  command -v psql >/dev/null || die "PostgreSQL não está instalado neste host"
  runuser -u postgres -- psql -qtAc 'SELECT 1' >/dev/null 2>&1 || die "não foi possível conectar como postgres pelo socket local"
  apt_install prometheus-postgres-exporter
  if ! runuser -u postgres -- psql -qtAc "SELECT 1 FROM pg_roles WHERE rolname = 'prometheus'" | grep -q 1; then
    say "criando a role prometheus (pg_monitor)"
    runuser -u postgres -- psql -qc 'CREATE ROLE prometheus LOGIN' >/dev/null
  fi
  runuser -u postgres -- psql -qc 'GRANT pg_monitor TO prometheus' >/dev/null
  set_exporter_args prometheus-postgres-exporter "--web.listen-address=127.0.0.1:9187" \
    'DATA_SOURCE_NAME="user=prometheus host=/var/run/postgresql dbname=postgres sslmode=disable"'
  wait_for http://127.0.0.1:9187/metrics '^pg_up 1' ||
    die "prometheus-postgres-exporter não reportou pg_up 1 (pg_hba.conf permite peer para prometheus? journalctl -u prometheus-postgres-exporter)"
}

# ---------------------------------------------------------------- mysql
MYSQL_CNF=/etc/prometheus/mysqld-exporter-cognitive-driver.cnf
setup_mysql() {
  command -v mysql >/dev/null || die "MySQL/MariaDB não está instalado neste host"
  mysql -e 'SELECT 1' >/dev/null 2>&1 || die "não foi possível conectar como root pelo socket local (mysql -e 'SELECT 1')"
  apt_install prometheus-mysqld-exporter
  local auth="IDENTIFIED WITH auth_socket"
  mysql --version | grep -qi mariadb && auth="IDENTIFIED VIA unix_socket"
  say "garantindo o usuário prometheus@localhost (somente leitura de status)"
  mysql -e "CREATE USER IF NOT EXISTS 'prometheus'@'localhost' $auth WITH MAX_USER_CONNECTIONS 3;
            GRANT PROCESS, REPLICATION CLIENT, SELECT ON *.* TO 'prometheus'@'localhost';" ||
    die "falha ao criar o usuário de monitoramento no MySQL"
  local sock
  sock=$(mysql -NBe "SELECT @@socket")
  install -d -m 0755 /etc/prometheus
  printf '# Gerado por cognitive-driver (scripts/enable-service.sh mysql).\n[client]\nuser=prometheus\nsocket=%s\n' "$sock" > "$MYSQL_CNF"
  chown root:prometheus "$MYSQL_CNF" && chmod 0640 "$MYSQL_CNF"
  set_exporter_args prometheus-mysqld-exporter "--config.my-cnf=$MYSQL_CNF --web.listen-address=127.0.0.1:9104"
  wait_for http://127.0.0.1:9104/metrics '^mysql_up 1' ||
    die "prometheus-mysqld-exporter não reportou mysql_up 1 (journalctl -u prometheus-mysqld-exporter)"
}

# ---------------------------------------------------------------- driver
say "configurando $SERVICE"
"setup_${SERVICE//-/_}"

if [[ $SERVICE == processes ]]; then
  port=$(sed -n 's/^  process_exporter_port: *\([0-9]*\).*/\1/p' $ETC/config.yaml)
  if [[ ${port:-9256} == 0 ]]; then
    warn "attribution.process_exporter_port está 0 em $ETC/config.yaml: defina 9256 para usar o process-exporter"
  elif [[ ${port:-9256} != "${LISTEN##*:}" ]]; then
    warn "o process-exporter escuta na porta ${LISTEN##*:}, mas attribution.process_exporter_port é ${port:-9256}"
  fi
  if (( WITH_IO )); then
    io=$(curl -sf -m 5 "http://$probe/metrics" 2>/dev/null | awk '/^namedprocess_namegroup_(read|write)_bytes_total/ && $NF > 0 {n++} END {print n + 0}')
    (( io > 0 )) || warn "o I/O por processo ainda está zerado; veja grep Cap /proc/\$(pidof prometheus-process-exporter)/status"
    say "process-exporter ativo em $LISTEN; novos eventos de CPU, memória e disco indicam o processo responsável"
  else
    say "process-exporter ativo em $LISTEN; novos eventos de CPU e memória indicam o processo responsável"
    echo "    Eventos de disco precisam de --with-io (concede CAP_SYS_PTRACE ao exporter para ler o I/O de outros usuários)."
  fi
  exit 0
fi

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
