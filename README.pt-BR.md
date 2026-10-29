# cognitive-driver ~ Micro-IA de observabilidade (MVP)

[![CI](https://github.com/spikeloader/cognitive-driver/actions/workflows/ci.yml/badge.svg)](https://github.com/spikeloader/cognitive-driver/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/spikeloader/cognitive-driver)](https://github.com/spikeloader/cognitive-driver/releases)
[![Go Report Card](https://goreportcard.com/badge/github.com/spikeloader/cognitive-driver)](https://goreportcard.com/report/github.com/spikeloader/cognitive-driver)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue)](LICENSE)

**Português** · [English](README.md)

Daemon leve em Go que observa métricas de hosts Linux via Prometheus
`node_exporter`, mantém uma baseline adaptativa (EWMA + desvio-padrão
exponencial), detecta anomalias, explica a decisão e recomenda uma ação.
Opera apenas em modo **`observe_only`**: nenhum comando é executado no host.

```
node_exporter → Collector → Feature Engine → Anomaly Engine → Explanation Engine → Policy Engine → API / SQLite / logs JSON
```

## Instalação no host

Requisitos: Linux com systemd (testado no Ubuntu 26.04), Go 1.27+ para compilar
e `prometheus-node-exporter`, que o instalador instala via apt quando nada
responde em `127.0.0.1:9100` (desative com `--without-node-exporter`).

A partir de um [release](https://github.com/spikeloader/cognitive-driver/releases)
(linux amd64/arm64, sem precisar de Go):

```bash
curl -LO https://github.com/spikeloader/cognitive-driver/releases/download/v0.1.0/cognitive-driver-0.1.0-linux-amd64.tar.gz
tar -xzf cognitive-driver-0.1.0-linux-amd64.tar.gz && cd cognitive-driver-0.1.0-linux-amd64
sudo scripts/install.sh
```

Ou a partir do código:

```bash
git clone https://github.com/spikeloader/cognitive-driver.git && cd cognitive-driver
make build                 # como seu usuário
sudo scripts/install.sh    # ou: sudo make install
```

O instalador:

| Item | Destino |
|---|---|
| Binário | `/usr/local/bin/cognitive-driver` |
| Configuração | `/etc/cognitive-driver/config.yaml`, gerada de `configs/config.yaml` com o hostname, `data_dir` em `/var/lib/cognitive-driver` e API em `127.0.0.1:8088` |
| Token da API | `/etc/cognitive-driver/env` (`MICRO_AI_TOKEN` aleatório, root 0600) |
| Unit systemd | `/etc/systemd/system/cognitive-driver.service`, habilitada e iniciada |
| Estado | `/var/lib/cognitive-driver` (SQLite com eventos e baselines) |

Ao reinstalar (atualização), a configuração, o token e o estado são mantidos;
apenas o binário e a unit são substituídos e o serviço é reiniciado. Opções:
`--listen :8088` expõe o painel na rede; `--no-start` só copia os arquivos.
Para ver o que seria instalado sem root: `DESTDIR=/tmp/stage scripts/install.sh`.

```bash
sudo cat /etc/cognitive-driver/env             # token para o painel e a API
journalctl -u cognitive-driver -f              # logs JSON
sudo systemctl restart cognitive-driver        # após editar a configuração
cognitive-driver -check -config /etc/cognitive-driver/config.yaml   # valida a configuração
sudo scripts/uninstall.sh [--purge]            # remove (--purge apaga config, token e eventos)
```

A unit roda com usuário dinâmico sem privilégios, sistema de arquivos
somente leitura, filtro de syscalls, sem capabilities e com limites de
25% de CPU e 128 MiB de memória (`systemd-analyze security` 1,4 — OK).
A configuração é validada (`ExecStartPre=... -check`) antes de cada início.

### Validação ponta a ponta

```bash
scripts/validate-e2e.sh            # contra o serviço instalado (pede sudo para ler o token e reiniciar)
scripts/validate-e2e.sh --local    # compila e roda o binário numa porta separada, sem instalar
```

Leva ~6–10 min: espera o aprendizado da baseline, gera carga de CPU e disco
(`stress-ng`, ou laços de shell e `dd`), confere o evento, a API, as métricas,
os logs, o modo seguro e a persistência após restart.

### Desenvolvimento

```bash
make test        # testes unitários com -race
make run         # build + execução local com configs/config.yaml (data em ./data)
make dist        # pacotes de release em dist/ (linux amd64/arm64 + SHA256SUMS)
```

## Serviços monitorados (nginx, Apache, Docker, PHP, cron, Redis, Memcached, PostgreSQL, MySQL, Kubernetes)

Além do host (CPU, memória, disco, rede via node_exporter), cada serviço tem
um driver próprio que lê o exporter do serviço. Ative com um comando; o
script configura o exporter, valida e instala o driver em
`/etc/cognitive-driver/drivers.d/` (sem editar a configuração principal):

```bash
sudo scripts/enable-service.sh nginx     # stub_status em 127.0.0.1:8090 + prometheus-nginx-exporter
sudo scripts/enable-service.sh apache    # mod_status + prometheus-apache-exporter
sudo scripts/enable-service.sh docker    # metrics-addr no daemon.json (reinicia o dockerd; pede confirmação)
sudo scripts/enable-service.sh php-fpm   # exige o php-fpm_exporter em 127.0.0.1:9253
sudo scripts/enable-service.sh swoole --metrics-url http://127.0.0.1:9501/metrics
sudo scripts/enable-service.sh cron      # driver que lê as métricas do cron-wrap via node_exporter
sudo scripts/enable-service.sh processes # process-exporter: eventos indicam o processo responsável
sudo scripts/enable-service.sh redis     # prometheus-redis-exporter (REDIS_PASSWORD=... se houver requirepass)
sudo scripts/enable-service.sh memcached # exige o memcached_exporter em 127.0.0.1:9150
sudo scripts/enable-service.sh postgresql # prometheus-postgres-exporter + role "prometheus" sem senha (pg_monitor, peer)
sudo scripts/enable-service.sh mysql     # prometheus-mysqld-exporter + prometheus@localhost (auth por socket, só leitura)
sudo scripts/enable-service.sh kubernetes # kube-state-metrics descoberto pelo kubectl (ou --ksm-url URL); um nó por cluster
sudo scripts/enable-service.sh nginx --disable   # remove o driver
```

| Driver | Detecção | Tipo |
|---|---|---|
| `ai-nginx-health` | nginx fora do ar (`nginx_up ≠ 1`) | regra · critical |
| | conexões descartadas (aceitas − tratadas > 0): limite de `worker_connections`/descritores | regra · high |
| | requisições/s e conexões ativas fora do padrão | estatística |
| `ai-apache-health` | Apache fora do ar (`apache_up ≠ 1`) | regra · critical |
| | workers ocupados > 90% do scoreboard (saturação) | regra · high |
| | requisições/s, workers ocupados e tempo médio por requisição fora do padrão | estatística |
| `ai-docker-health` | falhas de HEALTHCHECK | regra · high |
| | inícios de container > ~3/min sustentados (crash loop) | regra · high |
| | containers em execução fora do padrão (queda = containers parando) | estatística |
| `ai-php-fpm-health` | pool fora do ar (`phpfpm_up ≠ 1`) | regra · critical |
| (por pool) | `max_children` atingido, fila no listen, processos ocupados > 90% | regra · high |
| | requisições lentas (slowlog) | regra · medium |
| | requisições/s fora do padrão | estatística |
| `ai-swoole-health` | workers ocupados > 90% | regra · high |
| (por servidor) | conexões, requisições/s, corrotinas e fila de tasks fora do padrão | estatística |
| `ai-cron-health` | última execução com código ≠ 0 | regra · high |
| (por job) | job sem iniciar há mais de 2 intervalos (cron parado, linha removida) | regra · high |
| | execução em andamento há mais de 1 intervalo (travado/sobreposição) | regra · high |
| | duração > 80% do intervalo | regra · medium |
| `ai-redis-health` | Redis fora do ar; conexões recusadas (`maxclients`); memória > 90% do `maxmemory`; último snapshot RDB falhou | regra · critical/high |
| | réplica sem link com o primário | regra · critical |
| | despejos, comandos/s, misses no keyspace e clientes fora do padrão | estatística |
| `ai-memcached-health` | Memcached fora do ar; conexões > 90% do limite (`-c`) | regra · critical/high |
| | despejos, misses de GET, comandos/s e conexões fora do padrão | estatística |
| `ai-postgresql-health` | PostgreSQL fora do ar; conexões > 90% do `max_connections` | regra · critical/high |
| (por banco) | deadlocks; atraso da réplica > 5 min | regra · high |
| | rollbacks e arquivos temporários (por banco), transações/s e conexões fora do padrão | estatística |
| `ai-mysql-health` | MySQL/MariaDB fora do ar; thread de IO/SQL da replicação parada | regra · critical |
| | conexões > 90% do `max_connections`; atraso da réplica > 5 min | regra · high |
| | threads em execução, consultas lentas, conexões abortadas, esperas por lock do InnoDB e consultas/s fora do padrão | estatística |
| `ai-kubernetes-health` | nó NotReady; Deployment abaixo do mínimo de réplicas disponíveis; PVC com volume perdido | regra · critical |
| (por nó, namespace ou Deployment) | nó com pressão de memória/disco/PIDs; containers em CrashLoopBackOff, sem baixar a imagem ou sem conseguir ser criados; pods sem nó disponível; rollout travado | regra · high |
| | reinícios de container (por namespace), pods em execução, pendentes e com falha e Jobs com falha fora do padrão | estatística |

**Regras** disparam sem baseline (após `consecutive_points` coletas) e
servem para estados inequívocos; a **detecção estatística** aprende o padrão
de cada métrica. Todo evento traz causa provável e recomendação específicas
do serviço.

### Processo responsável

Quando um evento é emitido para uma métrica com `attribute` (`cpu`, `memory`
ou `io`), o cognitive-driver lê o
[process-exporter](https://github.com/ncabatoff/process-exporter) do host
duas vezes, com 2 s de intervalo, e grava no evento os grupos de processos
(mesmo nome de comando) que mais consumiam o recurso. O painel mostra o
primeiro como **Responsável provável** no topo do evento, com a fatia do
total e os seguintes; a lista de eventos mostra o nome dele.

```bash
sudo scripts/enable-service.sh processes                       # local: 127.0.0.1:9256
sudo scripts/enable-service.sh processes --listen 0.0.0.0:9256 # host coletado por uma instância central
```

- O process-exporter é procurado no mesmo endereço do node_exporter do host,
  na porta `attribution.process_exporter_port` (padrão 9256; 0 desativa).
  Não há configuração por host.
- As métricas de CPU, memória e disco do host têm `attribute` por padrão;
  qualquer métrica de driver pode declarar, e `attribute: none` desativa.
- Indica quem consumia o recurso no momento, não uma causa confirmada.
  Tráfego de rede não é atribuído (o exporter não tem rede por processo).
- Se o process-exporter estiver indisponível, o evento abre mesmo assim,
  sem responsável.
- Eventos de disco precisam de `--with-io`. O `/proc/<pid>/io` só é
  legível pelo dono do processo, então lê-lo para outros usuários exige
  `CAP_DAC_READ_SEARCH` (permissão do arquivo) e `CAP_SYS_PTRACE` (checagem
  de ptrace do kernel). A opção concede as duas ao process-exporter dentro de
  um sandbox (sistema de arquivos somente leitura, sem `/home`, `/root`,
  `/etc/shadow`, `/etc/ssh` nem sudoers). É opcional porque amplia o que o
  exporter pode ler. Sem ela, eventos de CPU e memória têm responsável e os
  de disco não.

### PHP

| Cenário | Cobertura |
|---|---|
| PHP-FPM | `ai-php-fpm-health`, via [php-fpm_exporter](https://github.com/hipages/php-fpm_exporter) lendo o `pm.status_path` de cada pool |
| mod_php | `ai-apache-health` (workers, latência e saturação do Apache) |
| Swoole/OpenSwoole | `ai-swoole-health`: a aplicação expõe `$server->stats()` em `/metrics` com [deploy/swoole/metrics.php](deploy/swoole/metrics.php) (exemplo no arquivo) |
| PHP CLI | pelos jobs de cron (abaixo) |

### Jobs de cron

Cada linha de `/etc/cron.d` passa a rodar pelo [cron-wrap](deploy/cron/cron-wrap),
que grava por job (início, fim, código de saída, duração e intervalo esperado)
no textfile collector do node_exporter. O driver `ai-cron-health` lê essas
métricas pelos alvos de `collector.targets` e abre **um evento por job**.

```bash
sudo scripts/wrap-cron-d.sh            # simulação: mostra o diff, não grava nada
sudo scripts/wrap-cron-d.sh --apply    # instala o cron-wrap, faz backup em /var/backups e reescreve
sudo scripts/wrap-cron-d.sh --unwrap --apply   # desfaz
```

- O job sempre roda e mantém o código de saída. Se o wrapper não conseguir
  gravar as métricas, só avisa em stderr.
- Nome do job: `<arquivo>.<script>` (ex.: `relatorios.backup`). O intervalo
  (`-i`) é deduzido de agendamentos regulares (`*/5`, `0 3 * * *`, `@hourly`,
  dias da semana). Com agendamentos irregulares só existe o alerta de falha.
- Comandos com `&&`, `|`, `;` e afins rodam via `$SHELL -c '...'` para que o
  wrapper meça a linha inteira. Linhas com `%` e `@reboot` ficam como estão.
- Executar de novo é seguro, porque linhas já envolvidas são mantidas. Jobs de
  usuários que não são root recebem ACL de escrita no diretório do textfile
  collector (`/var/lib/prometheus/node-exporter`, ou `CRON_WRAP_DIR`).
- Ao remover um job de propósito, apague o `cron_<job>.prom`. Sem isso, o
  alerta de atraso dispara.
- Arquivos instalados por pacotes (dpkg/rpm, como `anacron`, `e2scrub_all` e
  `php`) ficam de fora por padrão: editá-los gera conflito de conffile nas
  atualizações. Use `--include-packaged` para envolvê-los mesmo assim.

### Outros serviços sem mudar código

Qualquer exporter Prometheus vira um driver com um arquivo em `drivers.d/`.
Os modelos em [deploy/drivers/](deploy/drivers/) mostram todos os recursos;
este é uma versão reduzida do de Redis:

```yaml
drivers:
  - name: ai-redis-health
    targets: [{host: meu-host, url: "http://127.0.0.1:9121/metrics"}]
    metrics:
      - name: redis_up
        rule: {equals: 1, severity: critical}
        describe:
          label: Disponibilidade do Redis
          unit: bool
          down:
            cause: Redis parado ou exporter sem acesso.
            recommendation: Verificar systemctl status redis-server e o log do Redis.
      - name: redis_evicted_keys_total          # contador → taxa por segundo
        describe: {label: Chaves despejadas do Redis, unit: per_second}
      - name: redis_memory_used_bytes
        alias: redis_memory_ratio
        divide_by: {name: redis_memory_max_bytes}
        rule: {above: 0.9}
```

Campos de métrica: `match`/`exclude` (regex de labels), `type`
(counter/gauge/timestamp — timestamp vira idade em segundos), `alias`,
`group_by` (uma feature e um evento por valor dos labels, ex.: `[job]`,
`[pool]`, em vez de somar as séries), `subtract`/`divide_by` (derivadas),
`attribute` (`cpu`/`memory`/`io`/`none`: recurso usado para indicar o
processo responsável), `describe.en` (os mesmos textos `label`/`up`/`down`
em inglês, usados pelo painel em inglês), `rule`
(`above`/`below`/`equals` + `severity`), `min_stddev` (piso absoluto do
desvio, útil em contagens pequenas) e `describe` (`label`, `unit` —
cores, bytes, bytes_per_second, ratio, per_second, count, milliseconds,
seconds, bool — e causa/recomendação para `up`/`down`). Valide com
`cognitive-driver -check -config /etc/cognitive-driver/config.yaml`.

## Painel web

Acesse **http://127.0.0.1:8088/** (redireciona para `/ui/`). O painel vem
embutido no binário, não carrega nada de origens externas e consome apenas
`/health` e `/events` deste serviço. Está em português e inglês (seletor no
topo, lembrado por navegador; o padrão segue o idioma do navegador). Os
eventos gravam explicação, causa e recomendação nos dois idiomas; eventos
anteriores às traduções as recebem quando o serviço inicia, se o driver
deles ainda estiver carregado. Ele mostra:

- estado do serviço, hosts coletados e drivers (habilitados/desabilitados);
- contadores de eventos abertos, high/critical, reconhecidos, resolvidos e falsos positivos;
- lista de eventos com filtros por status, severidade, driver e host;
- detalhe de cada evento: **recomendação**, o que foi observado, causa provável,
  intensidade do desvio (faixas low → critical) e evidências por janela (1m/5m/15m);
- botões para **reconhecer**, **resolver**, marcar **falso positivo** ou reabrir,
  com campo de feedback (`PATCH /events/{id}`).

Na primeira visita, clique em **Token** e informe o valor de `MICRO_AI_TOKEN`
(fica no `sessionStorage`, ou no `localStorage` se marcar "Lembrar").
A página atualiza a cada 15 s enquanto estiver visível. Cada evento tem um
link direto (`/ui/#event=<id>`) para colar em tickets ou no chat do time.

## Como a detecção funciona

| Etapa | Regra |
|---|---|
| Feature | Contadores (`*_total`) viram taxa por segundo; séries filtradas por label são somadas (ou separadas por grupo com `group_by`). CPU = núcleos ocupados; disco = fração do tempo com I/O ativo; rede = bytes/s; memória = bytes disponíveis. |
| Baseline | EWMA de média e variância por host × métrica nas janelas 1m, 5m e 15m (`alpha = 1 - e^(-intervalo/janela)`). |
| Aprendizado | Nenhuma detecção antes de `warmup_points` observações (padrão 20 ≈ 5 min). |
| Z-score | `(valor - média) / desvio` contra a baseline **anterior** ao ponto, na `decision_window` (padrão 5m). As demais janelas aparecem como evidência. O desvio tem um piso de `min_stddev_ratio × |média|` para métricas quase constantes. |
| Anomalia | `|z| ≥ anomaly_threshold` (3,0), em qualquer direção (pico ou queda). |
| Severidade | low 3–4σ · medium 4–6σ · high 6–10σ · critical ≥ 10σ. Score = `1 - e^(-|z|/2)`. |
| Congelamento | Pontos anômalos não entram na baseline até `baseline_freeze_points` (10 ≈ 2,5 min) seguidos; assim um pico não infla o desvio-padrão e mascara a própria detecção. Se a anomalia persistir além disso, o novo patamar passa a ser aprendido. |
| Responsável | Para métricas com `attribute`, os grupos de processos que mais consumiam CPU, memória ou I/O de disco na emissão (process-exporter, 2 amostras). Guardado junto da observação de pico. |
| Evento | Emitido após `consecutive_points` (3) pontos anômalos seguidos, com `cooldown` (5m) por host × métrica. Há no máximo um evento ativo (aberto ou reconhecido) por driver × host × métrica: reemissões viram ocorrências dele. |
| Ação | A recomendação passa pela política; em `observe_only` é sempre bloqueada (`cognitive_driver_actions_blocked_total`). |

Baselines são salvas no SQLite a cada `baseline_save_interval` e no shutdown,
e restauradas no início, de modo que um restart não exige novo aprendizado.

## API

`/health` e `/metrics` são públicos (health check e scrape do Prometheus).
Os demais exigem `Authorization: Bearer $MICRO_AI_TOKEN` quando a variável
está definida (sem ela, o serviço avisa no log e roda sem autenticação).

| Método | Endpoint | Função |
|---|---|---|
| GET | `/health` | Status, versão, drivers (versão do contrato, modo, habilitado) e estado da coleta |
| GET | `/metrics` | Métricas internas no formato Prometheus |
| POST | `/predict` | Avalia um valor contra a baseline atual (sem alterar a baseline nem gerar evento) |
| GET | `/events` | Eventos recentes. Filtros: `host`, `driver`, `metric`, `severity`, `status`, `since` (RFC3339), `limit` (1–500) |
| GET | `/events/{id}` | Detalhe com evidências por janela, causa provável e recomendação |
| PATCH | `/events/{id}` | Feedback do SRE: `status` (`open`, `acknowledged`, `resolved`, `false_positive`) e `feedback`. Reabrir retorna 409 se a mesma métrica já tem outro evento ativo |

```bash
TOKEN="Authorization: Bearer $MICRO_AI_TOKEN"

curl -s localhost:8088/health | jq

# value na unidade da feature: taxa/s para contadores (0.41 = disco 41% ocupado)
curl -s -X POST localhost:8088/predict -H "$TOKEN" -H 'Content-Type: application/json' \
  -d '{"driver":"ai-disk-health","metric":"node_disk_io_time_seconds_total","value":0.41}' | jq

curl -s "localhost:8088/events?severity=high&limit=20" -H "$TOKEN" | jq
curl -s localhost:8088/events/<id> -H "$TOKEN" | jq

curl -s -X PATCH localhost:8088/events/<id> -H "$TOKEN" -H 'Content-Type: application/json' \
  -d '{"status":"false_positive","feedback":"backup noturno agendado"}' | jq

curl -s localhost:8088/metrics | grep ^cognitive_driver_
```

Exemplo de resposta de `/predict` com anomalia:

```json
{
  "driver": "ai-disk-health", "host": "local", "metric": "node_disk_io_time_seconds_total",
  "observed": 0.41, "anomaly": true, "score": 0.992, "severity": "high", "zscore": 9.67,
  "learning": false, "streak": 1, "window": "5m",
  "baseline": {"mean": 0.12, "stddev": 0.03},
  "windows": [{"window": "1m", "mean": 0.13, "stddev": 0.03, "zscore": 9.3}, "..."],
  "explanation": "Utilização de I/O de disco em 41,0%, 9,7 desvios-padrão acima da baseline de 5m (média 12,0%, desvio 3,0%).",
  "probable_cause": "Pressão de I/O: carga de leitura/escrita acima do normal ...",
  "recommendation": "Investigar processos com alta leitura/escrita (iotop, pidstat -d) ...",
  "mode": "observe_only"
}
```

Se ainda não há baseline para o host/métrica, `/predict` retorna `409`.
Durante o aprendizado, retorna `200` com `"learning": true` e `"anomaly": false`.

## Métricas internas

| Métrica | Descrição |
|---|---|
| `cognitive_driver_events_total{driver,metric,severity}` | Eventos gerados |
| `cognitive_driver_false_positives_total{driver,metric}` | Falsos positivos reportados via `PATCH /events/{id}` |
| `cognitive_driver_inference_duration_seconds{driver}` | Latência de inferência |
| `cognitive_driver_predictions_total`, `cognitive_driver_anomalous_points_total` | Volume de previsões e pontos acima do limiar |
| `cognitive_driver_scrapes_total{host,result}`, `cognitive_driver_scrape_duration_seconds` | Saúde da coleta |
| `cognitive_driver_actions_blocked_total{driver}` | Ações bloqueadas pela política |
| `cognitive_driver_driver_info`, `cognitive_driver_store_errors_total` | Drivers carregados e falhas de persistência |
| `go_memstats_*`, `process_*` | Uso de memória e CPU do próprio serviço |

Todas as previsões (`prediction`), eventos (`anomaly_event`), ações bloqueadas
(`action_blocked`) e feedbacks (`event_feedback`) são registrados em JSON no stdout.

## Configuração

Veja [configs/config.yaml](configs/config.yaml), que documenta cada campo.
Cada item de `drivers` é uma micro-IA independente, com métricas e limiares
próprios, e pode ser desativado com `enabled: false`. O único modo aceito no
MVP é `observe_only`; qualquer outro é rejeitado na validação.

## Estrutura

```
cmd/cognitive-driver/     main: configuração, wiring, HTTP server, shutdown
internal/config/          YAML + defaults + validação
internal/collector/       scrape HTTP + parser do formato de texto Prometheus
internal/feature/         contador → taxa, timestamp → idade, filtros de label, agregação e group_by
internal/attribution/     processo responsável a partir do process-exporter
internal/anomaly/         EWMA, z-score, severidade, consecutivos, cooldown
internal/explain/         catálogo (host, nginx, Apache, Docker), explicação e recomendação
internal/policy/          autorização de ações (bloqueia tudo no MVP)
internal/driver/          contrato Driver v1 + implementação estatística
internal/runner/          loop observe → detect → explain → recommend
internal/store/           SQLite (eventos e baselines)
internal/telemetry/       métricas Prometheus do serviço
internal/api/             endpoints HTTP/JSON
internal/api/ui/          painel web (HTML/CSS/JS embutidos via go:embed)
deploy/systemd/           unit com sandbox e limites
deploy/drivers/           modelos de driver por serviço (nginx, apache, docker, php-fpm, swoole, cron,
                          redis, memcached, postgresql, mysql, kubernetes)
deploy/cron/cron-wrap     wrapper dos jobs de cron (métricas no textfile collector)
deploy/swoole/metrics.php métricas do Swoole/OpenSwoole para o /metrics da aplicação
scripts/                  install.sh, uninstall.sh, enable-service.sh, wrap-cron-d.sh, validate-e2e.sh
```

## Contrato do driver (v1)

```go
type Driver interface {
    Info() Info                                       // nome, versão, contrato, modo
    Observe(collector.Snapshot) []feature.Feature     // observe(event) -> features
    Predict(feature.Feature) Anomaly                  // predict(features) -> anomaly (aprende online)
    Evaluate(feature.Feature) (Anomaly, error)        // predict sem efeito colateral (/predict)
    Explain(Anomaly) explain.Explanation              // explain(anomaly) -> explanation
    Recommend(Anomaly) policy.Action                  // recommend(anomaly) -> action
    Act(context.Context, policy.Action) error         // act(action): bloqueado no MVP
}
```

Uma nova implementação (TinyML, modelo local) só precisa satisfazer essa
interface. O runner, a API e o armazenamento não mudam.

## Contribuindo

Veja [CONTRIBUTING.md](CONTRIBUTING.md). Vulnerabilidades: siga o
[SECURITY.md](SECURITY.md) (reporte privado, não por issue). Mudanças por
versão estão no [CHANGELOG.md](CHANGELOG.md).

## Licença

[Apache License 2.0](LICENSE). Copyright 2026 The cognitive-driver authors.
