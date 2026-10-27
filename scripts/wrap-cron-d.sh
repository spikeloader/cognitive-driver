#!/usr/bin/env bash
# Envolve os jobs de /etc/cron.d com o cron-wrap, para o driver
# ai-cron-health receber métricas de cada job (falha, atraso, travamento).
#
# Uso: sudo scripts/wrap-cron-d.sh [--apply] [--unwrap] [--include-packaged] [--dir DIR]
#   (sem opções)        mostra o diff do que seria alterado; não grava nada
#   --apply             grava: instala /usr/local/bin/cron-wrap, faz backup em
#                       /var/backups/cron.d-wrap-<data>/ e reescreve os arquivos
#   --unwrap            remove o cron-wrap das linhas (combine com --apply)
#   --include-packaged  envolve também arquivos de pacotes (dpkg/rpm); por
#                       padrão eles ficam de fora: editá-los gera conflito de
#                       conffile nas atualizações
#   --dir DIR           diretório dos crontabs (padrão /etc/cron.d)
#
# Cada job vira "<arquivo>.<script>" (ex.: relatorios.backup) e recebe -i
# com o intervalo deduzido do agendamento quando ele é regular. Ficam de
# fora: @reboot, linhas com '%' (stdin do cron) e linhas já envolvidas.
# Executar de novo é seguro: linhas já envolvidas são mantidas.
set -euo pipefail
set -f # campos do cron ("*", "*/5") e comandos não podem passar pelo glob
cd "$(dirname "$0")/.."

APPLY=0 UNWRAP=0 PACKAGED=0 DIR=/etc/cron.d
WRAP_BIN=${WRAP_BIN:-/usr/local/bin/cron-wrap}
TEXTFILE_DIR=${CRON_WRAP_DIR:-/var/lib/prometheus/node-exporter}
while (( $# )); do
  case $1 in
    --apply) APPLY=1 ;;
    --unwrap) UNWRAP=1 ;;
    --include-packaged) PACKAGED=1 ;;
    --dir) DIR=${2:?--dir requer um diretório}; shift ;;
    -h|--help) sed -n '2,19p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "opção desconhecida: $1" >&2; exit 2 ;;
  esac
  shift
done

say()  { printf '\033[1m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[33mAVISO:\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[31mERRO:\033[0m %s\n' "$*" >&2; exit 1; }

[[ -d $DIR ]] || die "diretório não encontrado: $DIR"
(( APPLY == 0 )) || [[ $EUID -eq 0 ]] || die "--apply exige root: sudo $0 --apply"

# ---------------------------------------------------------------- intervalo
# expand FIELD MIN MAX: valores de um campo do cron (listas, faixas, passos).
# Falha para nomes (jan, mon) e formatos não numéricos.
expand() {
  local f=$1 lo=$2 hi=$3 part range step a b v
  for part in ${f//,/ }; do
    step=1 range=$part
    if [[ $part == */* ]]; then range=${part%/*} step=${part#*/}; fi
    if [[ $range == '*' ]]; then a=$lo b=$hi
    elif [[ $range == *-* ]]; then a=${range%-*} b=${range#*-}
    else a=$range b=$range; [[ $part == */* ]] && b=$hi
    fi
    [[ $a =~ ^[0-9]+$ && $b =~ ^[0-9]+$ && $step =~ ^[1-9][0-9]*$ ]] || return 1
    for ((v = 10#$a; v <= 10#$b; v += step)); do echo "$v"; done
  done | sort -un
}

# max_gap PERIOD valores...: maior distância entre valores consecutivos, circular.
max_gap() {
  local period=$1; shift
  local -a v=("$@")
  (( ${#v[@]} > 0 )) || return 1
  local gap=$((v[0] + period - v[${#v[@]} - 1])) i
  for ((i = 1; i < ${#v[@]}; i++)); do
    (( v[i] - v[i - 1] > gap )) && gap=$((v[i] - v[i - 1]))
  done
  echo "$gap"
}

# interval M H DOM MON DOW: maior intervalo, em segundos, entre execuções.
# Vazio quando o agendamento não é regular o bastante para deduzir.
interval() {
  local m=$1 h=$2 dom=$3 mon=$4 dow=$5 mins hours days t d
  mins=$(expand "$m" 0 59) || return 0
  hours=$(expand "$h" 0 23) || return 0
  local -a times=()
  for t in $hours; do for d in $mins; do times+=($((t * 60 + d))); done; done
  if [[ $dom == '*' && $mon == '*' && $dow == '*' ]]; then
    echo $(( $(max_gap 1440 "${times[@]}") * 60 ))
  elif [[ $dom == '*' && $mon == '*' ]]; then
    days=$(expand "$dow" 0 7) || return 0
    local -a week=()
    for d in $(for d in $days; do echo $((d % 7)); done | sort -un); do
      for t in "${times[@]}"; do week+=($((d * 1440 + t))); done
    done
    echo $(( $(max_gap 10080 "${week[@]}") * 60 ))
  elif [[ $mon == '*' && $dow == '*' ]]; then
    days=$(expand "$dom" 1 31) || return 0
    # Aproximação pelo mês de 31 dias: o alerta tolera 2 intervalos.
    echo $(( $(max_gap 31 $days) * 86400 ))
  fi
}

special_interval() {
  case $1 in
    @hourly) echo 3600 ;;
    @daily|@midnight) echo 86400 ;;
    @weekly) echo 604800 ;;
    @monthly) echo 2678400 ;;
    @yearly|@annually) echo 31622400 ;;
  esac
}

# ---------------------------------------------------------------- nomes
# job_name ARQUIVO COMANDO: "<arquivo>.<script>", com o script sem extensão;
# sem script reconhecível, usa o último caminho que não é destino de
# redirecionamento (php /app/artisan >> /dev/null -> artisan; em
# "test -e /run/x || /usr/bin/job" -> job) ou, por fim, o primeiro comando.
job_name() {
  local file=$1 cmd=$2 tok stem= redirect=0
  for tok in $cmd; do
    if [[ $tok =~ \.(php|sh|py|pl|rb|js)$ ]]; then stem=${tok##*/}; stem=${stem%.*}; break; fi
  done
  if [[ -z $stem ]]; then
    for tok in $cmd; do
      tok=${tok%%[;&|)]*}
      if [[ $tok == *'>'* || $tok == '<' ]]; then redirect=$([[ $tok == *'>' || $tok == '<' ]] && echo 1 || echo 0); continue; fi
      if (( redirect )); then redirect=0; continue; fi
      [[ $tok == */* ]] && stem=${tok##*/}
    done
  fi
  if [[ -z $stem ]]; then
    for tok in $cmd; do
      [[ $tok =~ ^[A-Za-z_][A-Za-z0-9_]*= ]] && continue
      stem=$tok; break
    done
  fi
  local name="${file}.${stem}"
  echo "${name//[^A-Za-z0-9._-]/_}"
}

# ---------------------------------------------------------------- linhas
SKIPPED=() WRAPPED=0 USERS=()

# wrap_cmd SHELL CMD: comandos com operadores de shell rodam via SHELL -c,
# senão o operador ficaria fora do wrapper (só o 1º comando seria medido).
wrap_cmd() {
  local shell=$1 cmd=$2
  if [[ $cmd =~ [\;\|\&\`\(\)\$] ]]; then
    printf "%s -c '%s'" "$shell" "${cmd//\'/\'\\\'\'}"
  else
    printf '%s' "$cmd"
  fi
}

unwrap_line() {
  local line=$1 re="^(.*)${WRAP_BIN//\//\\/}( -i [0-9]+)? [^ ]+ -- (.*)$"
  if [[ $line =~ $re ]]; then
    local prefix=${BASH_REMATCH[1]} cmd=${BASH_REMATCH[3]}
    if [[ $cmd =~ ^([^ ]+)\ -c\ \'(.*)\'$ ]]; then
      cmd=${BASH_REMATCH[2]}
      cmd=${cmd//\'\\\'\'/\'}
    fi
    line=$prefix$cmd
    WRAPPED=$((WRAPPED + 1))
  fi
  printf '%s\n' "$line"
}

# process FILE: imprime o arquivo com as linhas de job envolvidas.
process() {
  local path=$1 file shell=/bin/sh line
  file=$(basename "$path")
  declare -A seen=()
  while IFS= read -r line || [[ -n $line ]]; do
    if (( UNWRAP )); then unwrap_line "$line"; continue; fi
    local trimmed=${line#"${line%%[![:space:]]*}"}
    if [[ -z $trimmed || $trimmed == \#* ]]; then printf '%s\n' "$line"; continue; fi
    if [[ $trimmed =~ ^SHELL[[:space:]]*=[[:space:]]*(.+)$ ]]; then shell=${BASH_REMATCH[1]}; fi
    if [[ $trimmed =~ ^[A-Za-z_][A-Za-z0-9_]*[[:space:]]*= ]]; then printf '%s\n' "$line"; continue; fi
    if [[ $line == *"$WRAP_BIN"* ]]; then printf '%s\n' "$line"; continue; fi

    local user cmd iv
    if [[ $trimmed =~ ^(@[a-z]+)[[:space:]]+([^[:space:]]+)[[:space:]]+(.+)$ ]]; then
      user=${BASH_REMATCH[2]} cmd=${BASH_REMATCH[3]}
      if [[ ${BASH_REMATCH[1]} == @reboot ]]; then printf '%s\n' "$line"; continue; fi
      iv=$(special_interval "${BASH_REMATCH[1]}")
    elif [[ $trimmed =~ ^([^[:space:]]+)[[:space:]]+([^[:space:]]+)[[:space:]]+([^[:space:]]+)[[:space:]]+([^[:space:]]+)[[:space:]]+([^[:space:]]+)[[:space:]]+([^[:space:]]+)[[:space:]]+(.+)$ ]]; then
      user=${BASH_REMATCH[6]} cmd=${BASH_REMATCH[7]}
      iv=$(interval "${BASH_REMATCH[@]:1:5}")
    else
      SKIPPED+=("$file: linha não reconhecida: $line"); printf '%s\n' "$line"; continue
    fi
    if [[ $cmd =~ (^|[^\\])% ]]; then
      SKIPPED+=("$file: '%' no comando (stdin do cron): $cmd"); printf '%s\n' "$line"; continue
    fi

    local name base n=2
    base=$(job_name "$file" "$cmd") name=$base
    while [[ -n ${seen[$name]:-} ]]; do name="$base-$n"; n=$((n + 1)); done
    seen[$name]=1
    printf '%s%s %s%s -- %s\n' "${line%"$cmd"}" "$WRAP_BIN" "${iv:+-i $iv }" "$name" "$(wrap_cmd "$shell" "$cmd")"
    WRAPPED=$((WRAPPED + 1))
    [[ $user == root ]] || USERS+=("$user")
  done <"$path"
}

# ---------------------------------------------------------------- execução
# O cron ignora arquivos com '.' no nome (backups, .dpkg-old etc.).
mapfile -t FILES < <(find "$DIR" -maxdepth 1 -type f -regex '.*/[A-Za-z0-9_-]+$' | sort)

# packaged ARQUIVO: verdadeiro se o arquivo pertence a um pacote do sistema.
packaged() {
  { command -v dpkg >/dev/null && dpkg -S "$1" >/dev/null 2>&1; } ||
    { command -v rpm >/dev/null && rpm -qf "$1" >/dev/null 2>&1; }
}
if (( ! UNWRAP && ! PACKAGED )); then
  OWN=()
  for f in "${FILES[@]}"; do
    if packaged "$f"; then say "ignorado (arquivo de pacote; use --include-packaged): $f"; else OWN+=("$f"); fi
  done
  FILES=("${OWN[@]}")
fi
(( ${#FILES[@]} )) || { say "nenhum crontab a processar em $DIR"; exit 0; }

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
CHANGED=()
for f in "${FILES[@]}"; do
  process "$f" >"$TMP/$(basename "$f")"
  if ! cmp -s "$f" "$TMP/$(basename "$f")"; then
    CHANGED+=("$f")
    diff -u --label "$f" --label "$f (novo)" "$f" "$TMP/$(basename "$f")" || true
  fi
done

for s in "${SKIPPED[@]}"; do warn "mantido sem wrapper: $s"; done
(( ${#CHANGED[@]} )) || { say "nada a alterar"; exit 0; }
verb=envolvidas; (( UNWRAP )) && verb=restauradas
say "$WRAPPED linha(s) $verb em ${#CHANGED[@]} arquivo(s)"
if (( ! APPLY )); then
  say "simulação: nada foi gravado (use --apply para aplicar)"
  exit 0
fi

if (( ! UNWRAP )); then
  install -m 0755 deploy/cron/cron-wrap "$WRAP_BIN"
  say "cron-wrap instalado em $WRAP_BIN"
fi
BACKUP=/var/backups/cron.d-wrap-$(date +%Y%m%d%H%M%S)
install -d -m 0700 "$BACKUP"
for f in "${CHANGED[@]}"; do
  cp -a "$f" "$BACKUP/"
  # Nome com '.' para o cron não ler o arquivo pela metade.
  tmp="$DIR/.$(basename "$f").wrap"
  cp "$TMP/$(basename "$f")" "$tmp"
  chmod --reference="$f" "$tmp"
  chown --reference="$f" "$tmp"
  mv -f "$tmp" "$f"
done
say "backup dos originais em $BACKUP"

(( UNWRAP )) && exit 0
if [[ ! -d $TEXTFILE_DIR ]]; then
  warn "$TEXTFILE_DIR não existe: confira --collector.textfile.directory do node_exporter"
elif (( ${#USERS[@]} )); then
  # Jobs de outros usuários precisam gravar as métricas no diretório.
  mapfile -t USERS < <(printf '%s\n' "${USERS[@]}" | sort -u)
  if command -v setfacl >/dev/null; then
    for u in "${USERS[@]}"; do setfacl -m "u:$u:rwx" "$TEXTFILE_DIR"; done
    say "escrita em $TEXTFILE_DIR liberada (ACL) para: ${USERS[*]}"
  else
    warn "setfacl ausente (pacote acl): os jobs de ${USERS[*]} rodam, mas sem métricas"
  fi
fi
if ! curl -sf -m 3 http://127.0.0.1:9100/metrics 2>/dev/null | grep -q '^node_textfile_scrape_error'; then
  warn "o textfile collector do node_exporter não parece ativo em 127.0.0.1:9100"
fi
