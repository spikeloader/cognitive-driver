#!/usr/bin/env bash
# Instala (ou atualiza) o cognitive-driver como serviço systemd neste host.
#
# Uso:
#   make build                                  # como seu usuário (requer Go)
#   sudo scripts/install.sh [opções]
#
# Opções:
#   --without-node-exporter  não instala o prometheus-node-exporter (use quando ele já
#                            roda em outro lugar ou foi instalado de outra forma)
#   --listen ADDR          endereço da API/painel (padrão 127.0.0.1:8088; use :8088 para expor na rede)
#   --no-start             instala os arquivos sem iniciar o serviço
#
# Para inspecionar o que seria instalado, sem root e sem tocar no sistema:
#   DESTDIR=/tmp/stage scripts/install.sh
#
# Reinstalar preserva /etc/cognitive-driver (config e token) e o estado em
# /var/lib/cognitive-driver (eventos e baselines).
set -euo pipefail
cd "$(dirname "$0")/.."

NAME=cognitive-driver
DESTDIR=${DESTDIR:-}
BIN_SRC=bin/$NAME
BIN_DST=$DESTDIR/usr/local/bin/$NAME
ETC_DIR=$DESTDIR/etc/$NAME
UNIT_DST=$DESTDIR/etc/systemd/system/$NAME.service
LISTEN=127.0.0.1:8088
WITH_NODE_EXPORTER=1
START=1

while (( $# )); do
  case $1 in
    --with-node-exporter) WITH_NODE_EXPORTER=1 ;;   # padrão; mantido por compatibilidade
    --without-node-exporter) WITH_NODE_EXPORTER=0 ;;
    --listen) LISTEN=${2:?--listen requer um endereço}; shift ;;
    --no-start) START=0 ;;
    -h|--help) sed -n '2,20p' "$0"; exit 0 ;;
    *) echo "opção desconhecida: $1" >&2; exit 2 ;;
  esac
  shift
done

say()  { printf '\033[1m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[33mAVISO:\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[31mERRO:\033[0m %s\n' "$*" >&2; exit 1; }

STAGING=0
[[ -n $DESTDIR ]] && STAGING=1
if (( !STAGING )); then
  [[ $EUID -eq 0 ]] || die "execute com sudo: sudo $0 $*"
  command -v systemctl >/dev/null || die "systemd é necessário"
fi

# ---------------------------------------------------------------- binário
if [[ ! -x $BIN_SRC ]]; then
  if (( EUID != 0 )); then
    say "compilando $BIN_SRC"
    make build
  else
    die "$BIN_SRC não encontrado. Rode 'make build' como seu usuário antes de instalar."
  fi
fi
"$BIN_SRC" -version >/dev/null || die "$BIN_SRC não executa neste host"
VERSION=$("$BIN_SRC" -version)

# ---------------------------------------------------------------- node_exporter
node_exporter_up() { curl -sf -m 3 http://127.0.0.1:9100/metrics -o /dev/null 2>/dev/null; }
if (( !STAGING )); then
  if (( WITH_NODE_EXPORTER )) && ! node_exporter_up; then
    if command -v apt-get >/dev/null; then
      say "instalando prometheus-node-exporter (dependência de coleta)"
      DEBIAN_FRONTEND=noninteractive apt-get install -y prometheus-node-exporter >/dev/null ||
        die "falha ao instalar prometheus-node-exporter (tente: sudo apt update)"
      systemctl enable --now prometheus-node-exporter >/dev/null
      for _ in $(seq 20); do node_exporter_up && break; sleep 0.5; done
    else
      warn "apt não disponível: instale o node_exporter manualmente (https://prometheus.io/download/#node_exporter)"
    fi
  fi
  if node_exporter_up; then
    say "node_exporter respondendo em 127.0.0.1:9100"
  else
    warn "node_exporter não responde em 127.0.0.1:9100; o serviço vai subir, mas sem coletar."
    warn "verifique com: systemctl status prometheus-node-exporter"
  fi
fi

# ---------------------------------------------------------------- arquivos
say "instalando binário $VERSION em ${BIN_DST#"$DESTDIR"}"
install -d -m 0755 "$(dirname "$BIN_DST")"
install -m 0755 "$BIN_SRC" "$BIN_DST.new"
mv -f "$BIN_DST.new" "$BIN_DST"

install -d -m 0755 "$ETC_DIR" "$ETC_DIR/drivers.d"
if [[ -f $ETC_DIR/config.yaml ]]; then
  say "mantendo configuração existente em ${ETC_DIR#"$DESTDIR"}/config.yaml"
else
  say "gerando ${ETC_DIR#"$DESTDIR"}/config.yaml (host=$(hostname -s), listen=$LISTEN)"
  tmp=$(mktemp)
  sed -e 's#^\(  data_dir:\) .*#\1 /var/lib/cognitive-driver   \# estado persistente (systemd StateDirectory)#' \
      -e "s#^\(    - host:\) local\$#\1 $(hostname -s)#" \
      -e "s#^\(  listen:\) .*#\1 \"$LISTEN\"   \# use \":8088\" para expor o painel na rede#" \
      configs/config.yaml > "$tmp"
  grep -q '/var/lib/cognitive-driver' "$tmp" && grep -q "\"$LISTEN\"" "$tmp" ||
    die "não foi possível adaptar configs/config.yaml (formato inesperado)"
  "$BIN_SRC" -check -config "$tmp" >/dev/null || die "configuração gerada é inválida"
  install -m 0644 "$tmp" "$ETC_DIR/config.yaml"
  rm -f "$tmp"
fi

if [[ -f $ETC_DIR/env ]]; then
  say "mantendo token existente em ${ETC_DIR#"$DESTDIR"}/env"
else
  say "gerando token de acesso em ${ETC_DIR#"$DESTDIR"}/env (0600)"
  token=$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n')
  ( umask 077; printf 'MICRO_AI_TOKEN=%s\n' "$token" > "$ETC_DIR/env" )
  chmod 0600 "$ETC_DIR/env"
fi

say "instalando unit ${UNIT_DST#"$DESTDIR"}"
install -d -m 0755 "$(dirname "$UNIT_DST")"
install -m 0644 deploy/systemd/$NAME.service "$UNIT_DST"

if (( STAGING )); then
  say "árvore montada em $DESTDIR (nenhum serviço foi alterado)"
  exit 0
fi

# ---------------------------------------------------------------- serviço
systemctl daemon-reload
"$BIN_DST" -check -config "$ETC_DIR/config.yaml" >/dev/null || die "configuração inválida em $ETC_DIR/config.yaml"
if (( !START )); then
  say "instalado. Inicie com: sudo systemctl enable --now $NAME"
  exit 0
fi
systemctl enable $NAME >/dev/null 2>&1
if systemctl is-active --quiet $NAME; then
  say "reiniciando serviço"
  systemctl restart $NAME
else
  say "iniciando serviço"
  systemctl start $NAME
fi

listen=$(sed -n 's/^  listen: *"\{0,1\}\([^" ]*\)"\{0,1\}.*/\1/p' "$ETC_DIR/config.yaml")
port=${listen##*:}
host=${listen%:*}
[[ -z $host || $host == 0.0.0.0 || $host == "[::]" ]] && host=127.0.0.1
url="http://$host:$port"

ok=0
for _ in $(seq 30); do
  "$BIN_DST" -healthcheck "$url/health" 2>/dev/null && { ok=1; break; }
  sleep 1
done
if (( !ok )); then
  systemctl --no-pager status $NAME | tail -n 15 >&2 || true
  die "serviço não respondeu em $url/health (veja: journalctl -u $NAME -n 50)"
fi

cat <<EOF

$(printf '\033[32m')cognitive-driver $VERSION instalado e em execução.$(printf '\033[0m')

  Painel:        $url/
  Token:         sudo cat $ETC_DIR/env
  Configuração:  $ETC_DIR/config.yaml   (depois de editar: sudo systemctl restart $NAME)
  Logs:          journalctl -u $NAME -f
  Estado:        /var/lib/cognitive-driver (eventos e baselines)

A baseline leva ~5 min para ser aprendida; antes disso nenhum evento é gerado.
Monitorar serviços: sudo scripts/enable-service.sh nginx|apache|docker
Validação ponta a ponta: scripts/validate-e2e.sh
EOF
