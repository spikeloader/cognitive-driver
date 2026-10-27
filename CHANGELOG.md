# Changelog

Formato baseado em [Keep a Changelog](https://keepachangelog.com/pt-BR/1.1.0/);
o projeto segue [Versionamento Semântico](https://semver.org/lang/pt-BR/).

## [Não lançado]

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
