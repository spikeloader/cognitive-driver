package explain

// catalog descreve as métricas conhecidas: rótulo, unidade e, para cada
// direção do desvio, a causa provável e a recomendação ao operador.
// Métricas derivadas usam o alias definido em deploy/drivers/*.yaml.
var catalog = map[string]family{
	// ---------------------------------------------------------------- host (node_exporter)
	"node_cpu_seconds_total": {
		label: "Uso de CPU", unit: "cores",
		up: Guidance{
			Cause:          "Aumento de carga de processamento: processo em loop, job agendado, pico de tráfego ou contenção (steal) no hypervisor.",
			Recommendation: "Identificar os processos com maior consumo de CPU (top/pidstat), verificar jobs agendados recentes e correlacionar com tráfego e deploys.",
		},
		down: Guidance{
			Cause:          "Queda de carga de processamento: serviço parado, tráfego desviado ou falha upstream.",
			Recommendation: "Confirmar que os serviços esperados estão em execução e recebendo tráfego; checar health checks e balanceadores.",
		},
	},
	"node_memory_MemAvailable_bytes": {
		label: "Memória disponível", unit: "bytes",
		up: Guidance{
			Cause:          "Liberação de memória: processo grande encerrado ou reiniciado, ou cache descartado.",
			Recommendation: "Verificar se algum serviço reiniciou ou foi encerrado inesperadamente (journalctl, OOM killer).",
		},
		down: Guidance{
			Cause:          "Pressão de memória: possível vazamento, crescimento de cache da aplicação ou novo processo com alto consumo.",
			Recommendation: "Listar processos por RSS (ps --sort=-rss), checar swap e eventos do OOM killer, e avaliar vazamento de memória na aplicação.",
		},
	},
	"node_disk_io_time_seconds_total": {
		label: "Utilização de I/O de disco", unit: "ratio",
		up: Guidance{
			Cause:          "Pressão de I/O: carga de leitura/escrita acima do normal (backup, compactação, swap ou consultas pesadas) ou degradação do dispositivo.",
			Recommendation: "Investigar processos com alta leitura/escrita (iotop, pidstat -d), verificar latência e fila do dispositivo (iostat -x) e logs de erro do disco (dmesg, SMART).",
		},
		down: Guidance{
			Cause:          "Queda de atividade de disco: serviço que gera I/O parado ou bloqueado.",
			Recommendation: "Confirmar que os serviços de banco/armazenamento estão ativos e processando requisições.",
		},
	},
	"node_network_receive_bytes_total": {
		label: "Tráfego de rede recebido", unit: "bytes_per_second",
		up: Guidance{
			Cause:          "Aumento de tráfego de entrada: pico legítimo de uso, transferência em massa ou possível tráfego malicioso.",
			Recommendation: "Identificar as conexões e origens dominantes (ss, iftop, logs do proxy) e verificar se coincide com evento esperado; considerar rate limiting se for abusivo.",
		},
		down: Guidance{
			Cause:          "Queda de tráfego de entrada: falha upstream, problema de DNS/balanceador ou interface degradada.",
			Recommendation: "Verificar balanceadores, DNS, estado das interfaces (ip -s link) e erros de rede.",
		},
	},
	"node_network_transmit_bytes_total": {
		label: "Tráfego de rede enviado", unit: "bytes_per_second",
		up: Guidance{
			Cause:          "Aumento de tráfego de saída: respostas maiores, replicação/backup ou possível exfiltração de dados.",
			Recommendation: "Identificar processos e destinos com maior envio (ss, nethogs) e confirmar se a transferência é esperada.",
		},
		down: Guidance{
			Cause:          "Queda de tráfego de saída: serviço deixou de responder ou de replicar.",
			Recommendation: "Verificar se os serviços estão respondendo e se replicações/jobs de saída estão ativos.",
		},
	},

	// ---------------------------------------------------------------- nginx (prometheus-nginx-exporter)
	"nginx_up": {
		label: "Disponibilidade do nginx", unit: "bool",
		down: Guidance{
			Cause:          "O exporter não consegue consultar o stub_status: nginx parado, travado, recarregado com erro ou stub_status inacessível.",
			Recommendation: "Verificar `systemctl status nginx` e `journalctl -u nginx -n 50`, validar a configuração com `sudo nginx -t` e testar `curl http://127.0.0.1:8090/stub_status`.",
		},
		up: genericGuidance,
	},
	"nginx_http_requests_total": {
		label: "Requisições HTTP no nginx", unit: "per_second",
		up: Guidance{
			Cause:          "Pico de tráfego: campanha ou evento legítimo, crawler/bot agressivo, ataque (DDoS, força bruta) ou clientes em retry.",
			Recommendation: "Identificar IPs e rotas dominantes no access.log (`awk '{print $1}' /var/log/nginx/access.log | sort | uniq -c | sort -rn | head`), avaliar rate limiting (limit_req) e escalar os upstreams se o tráfego for legítimo.",
		},
		down: Guidance{
			Cause:          "Queda de tráfego: DNS ou balanceador desviando, certificado TLS expirado, bloqueio de rede ou indisponibilidade parcial.",
			Recommendation: "Testar o site de fora da rede, conferir DNS e validade do certificado e procurar erros em /var/log/nginx/error.log.",
		},
	},
	"nginx_connections_active": {
		label: "Conexões ativas no nginx", unit: "count",
		up: Guidance{
			Cause:          "Acúmulo de conexões: upstream lento segurando requisições, keepalive longo ou excesso de clientes simultâneos.",
			Recommendation: "Comparar com worker_connections × worker_processes, medir a latência dos upstreams e procurar 'worker_connections are not enough' no error.log.",
		},
		down: Guidance{
			Cause:          "Queda de conexões ativas: tráfego desviado ou nginx deixando de aceitar conexões.",
			Recommendation: "Verificar balanceador, DNS e o /var/log/nginx/error.log.",
		},
	},
	"nginx_connections_dropped": {
		label: "Conexões descartadas pelo nginx", unit: "per_second",
		up: Guidance{
			Cause:          "O nginx aceitou conexões que não conseguiu tratar: limite de worker_connections ou de descritores de arquivo atingido.",
			Recommendation: "Aumentar worker_connections e worker_rlimit_nofile (e LimitNOFILE na unit do nginx), conferir `ulimit -n` do processo e o error.log.",
		},
		down: genericGuidance,
	},

	// ---------------------------------------------------------------- Apache (prometheus-apache-exporter)
	"apache_up": {
		label: "Disponibilidade do Apache", unit: "bool",
		down: Guidance{
			Cause:          "O exporter não consegue ler o mod_status: Apache parado, travado ou /server-status bloqueado.",
			Recommendation: "Verificar `systemctl status apache2` e `journalctl -u apache2 -n 50`, validar com `sudo apache2ctl configtest` e testar `curl 'http://localhost/server-status?auto'`.",
		},
		up: genericGuidance,
	},
	"apache_accesses_total": {
		label: "Requisições no Apache", unit: "per_second",
		up: Guidance{
			Cause:          "Pico de tráfego: evento legítimo, crawler/bot agressivo, ataque (DDoS, força bruta) ou clientes em retry.",
			Recommendation: "Identificar IPs e URLs dominantes em /var/log/apache2/access.log, avaliar mod_evasive/mod_ratelimit e escalar se o tráfego for legítimo.",
		},
		down: Guidance{
			Cause:          "Queda de tráfego: DNS ou balanceador desviando, certificado TLS expirado ou indisponibilidade parcial.",
			Recommendation: "Testar o site de fora da rede, conferir DNS e certificado e procurar erros em /var/log/apache2/error.log.",
		},
	},
	"apache_workers_busy": {
		label: "Workers ocupados no Apache", unit: "count",
		up: Guidance{
			Cause:          "Mais workers ocupados que o habitual: requisições lentas (backend, PHP, banco) ou aumento de tráfego.",
			Recommendation: "Ver as requisições longas em /server-status, medir o tempo de resposta do backend (PHP-FPM, banco) e comparar com MaxRequestWorkers.",
		},
		down: Guidance{
			Cause:          "Menos workers ocupados que o habitual: queda de tráfego ou Apache recusando requisições.",
			Recommendation: "Conferir o tráfego recebido e o /var/log/apache2/error.log.",
		},
	},
	"apache_workers_busy_ratio": {
		label: "Ocupação dos workers do Apache", unit: "ratio",
		up: Guidance{
			Cause:          "Workers quase esgotados: novas requisições vão enfileirar ou ser recusadas. Causas comuns: backend lento ou MaxRequestWorkers baixo para a demanda.",
			Recommendation: "Ver as requisições longas em /server-status, checar o backend (PHP-FPM, banco) e, se houver memória, aumentar MaxRequestWorkers/ServerLimit no MPM em uso.",
		},
		down: genericGuidance,
	},
	"apache_request_duration_ms": {
		label: "Tempo médio por requisição no Apache", unit: "milliseconds",
		up: Guidance{
			Cause:          "Respostas mais lentas: backend (PHP, banco, API externa) degradado, contenção de CPU/I/O no host ou requisições mais pesadas.",
			Recommendation: "Correlacionar com CPU e I/O do host, verificar o slow log do PHP-FPM/banco e identificar as URLs mais lentas no access.log (%D).",
		},
		down: Guidance{
			Cause:          "Respostas mais rápidas que o habitual: possivelmente erros rápidos (4xx/5xx) substituindo respostas normais.",
			Recommendation: "Conferir a proporção de códigos 5xx/4xx no access.log e o error.log do Apache.",
		},
	},

	// ---------------------------------------------------------------- Docker (métricas do dockerd)
	"docker_containers_running": {
		label: "Containers em execução", unit: "count",
		down: Guidance{
			Cause:          "Containers pararam: crash da aplicação, OOM kill, falha de dependência ou parada manual/deploy.",
			Recommendation: "Listar os parados (`docker ps -a --filter status=exited`), ver o motivo (`docker inspect -f '{{.State.ExitCode}} {{.State.OOMKilled}}' <container>`) e os logs (`docker logs --tail 100 <container>`).",
		},
		up: Guidance{
			Cause:          "Mais containers que o habitual: deploy, escala manual ou containers órfãos.",
			Recommendation: "Conferir `docker ps` e remover containers órfãos ou duplicados.",
		},
	},
	"engine_daemon_health_checks_failed_total": {
		label: "Falhas de health check (Docker)", unit: "per_second",
		up: Guidance{
			Cause:          "Containers estão falhando no HEALTHCHECK: aplicação travada, dependência fora do ar ou checagem mal configurada.",
			Recommendation: "Identificar os containers unhealthy (`docker ps --filter health=unhealthy`) e ver o histórico das checagens (`docker inspect -f '{{json .State.Health}}' <container>`).",
		},
		down: genericGuidance,
	},
	"docker_container_starts": {
		label: "Inícios de container (Docker)", unit: "per_second",
		up: Guidance{
			Cause:          "Containers reiniciando em sequência (crash loop): a aplicação morre logo após iniciar e a política de restart a sobe de novo.",
			Recommendation: "Encontrar o container instável (`docker ps -a --format '{{.Names}} {{.Status}}'` e `docker inspect -f '{{.RestartCount}}' <container>`) e ler os logs da última execução (`docker logs --tail 100 <container>`).",
		},
		down: genericGuidance,
	},
}
