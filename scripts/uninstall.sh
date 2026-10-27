#!/usr/bin/env bash
# Remove o serviço cognitive-driver deste host.
#
# Uso: sudo scripts/uninstall.sh [--purge]
#   --purge  também apaga configuração/token (/etc/cognitive-driver) e o
#            estado (eventos e baselines em /var/lib/cognitive-driver)
#
# O pacote prometheus-node-exporter não é removido.
set -euo pipefail

NAME=cognitive-driver
PURGE=0
case ${1:-} in
  --purge) PURGE=1 ;;
  "") ;;
  -h|--help) sed -n '2,9p' "$0"; exit 0 ;;
  *) echo "opção desconhecida: $1" >&2; exit 2 ;;
esac
[[ $EUID -eq 0 ]] || { echo "execute com sudo: sudo $0 $*" >&2; exit 1; }

say() { printf '\033[1m==>\033[0m %s\n' "$*"; }

if systemctl list-unit-files "$NAME.service" >/dev/null 2>&1; then
  say "parando e desabilitando o serviço"
  systemctl disable --now "$NAME" >/dev/null 2>&1 || true
fi
rm -f "/etc/systemd/system/$NAME.service" "/usr/local/bin/$NAME"
systemctl daemon-reload
say "binário e unit removidos"

if (( PURGE )); then
  # Com DynamicUser, o estado fica em /var/lib/private e /var/lib/<nome> é um link.
  rm -rf "/etc/$NAME" "/var/lib/$NAME" "/var/lib/private/$NAME"
  say "configuração, token e estado removidos"
else
  say "mantidos: /etc/$NAME (config e token) e /var/lib/$NAME (eventos e baselines)"
fi
