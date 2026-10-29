# Changelog

Formato baseado em [Keep a Changelog](https://keepachangelog.com/pt-BR/1.1.0/);
o projeto segue [Versionamento Semântico](https://semver.org/lang/pt-BR/).

## [Não lançado]

### Adicionado
- Processo responsável: eventos de CPU, memória e disco trazem os grupos de
  processos que mais consumiam o recurso no momento (process-exporter), em
  destaque no detalhe do evento e na lista. `attribute` por métrica no YAML,
  seção `attribution` na configuração e `enable-service.sh processes`.
- Drivers de Redis, Memcached, PostgreSQL e MySQL/MariaDB, com
  `enable-service.sh redis|memcached|postgresql|mysql`. PostgreSQL e MySQL
  ganham um usuário de monitoramento sem senha (autenticação pelo socket
  local) e com permissões só de leitura.
- Driver de Kubernetes (`ai-kubernetes-health`) sobre o kube-state-metrics:
  nós NotReady ou sob pressão, CrashLoopBackOff, erros de imagem e de criação
  de container, pods sem nó, Deployments indisponíveis ou com rollout travado,
  PVCs perdidos e, por baseline, reinícios, pods por fase e Jobs com falha.
  `enable-service.sh kubernetes` descobre o Service pelo kubectl ou usa `--ksm-url`.

- Painel e textos dos eventos em português e inglês: seletor de idioma no
  topo (padrão pelo idioma do navegador), números e datas no formato do
  idioma. Eventos gravam `translations.en` (rótulo, explicação, causa e
  recomendação); catálogo de métricas em inglês e `describe.en` nos drivers.
  Ao iniciar, o serviço gera as traduções dos eventos gravados antes delas.

### Corrigido
- `enable-service.sh` dava a coleta como falha com exporters de saída grande
  (process-exporter): com `pipefail`, o `grep -q` encerrava o pipe e o `curl`
  saía por SIGPIPE. O process-exporter passa a rodar com `-threads=false`.

## [0.1.0] - 2026-10-27

Primeira versão pública.

### Adicionado
- Daemon `observe_only`: coleta do `node_exporter` e de exporters de serviço,
  baseline EWMA com z-score por janela (1m/5m/15m), regras fixas, explicação,
  causa provável e recomendação por métrica.
- Drivers: host (CPU, memória, disco, rede), nginx, Apache, Docker, PHP-FPM
  (por pool), Swoole/OpenSwoole (por servidor) e jobs de cron (por job).
- `group_by` (uma feature e um evento por valor de label) e `type: timestamp`
  (idade em segundos) nos drivers YAML.
- `cron-wrap` e `scripts/wrap-cron-d.sh` para instrumentar `/etc/cron.d`.
- Deduplicação: no máximo um evento ativo por driver, host e métrica;
  reemissões viram ocorrências do evento aberto.
- API HTTP (`/health`, `/predict`, `/events`, `/metrics`), painel web
  embutido com paginação de eventos e persistência em SQLite.
- Instalador com unit systemd isolada e validação ponta a ponta.

[Não lançado]: https://github.com/spikeloader/cognitive-driver/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/spikeloader/cognitive-driver/releases/tag/v0.1.0
