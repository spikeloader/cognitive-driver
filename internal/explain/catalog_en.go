package explain

// catalogEN traduz o catálogo para inglês; a unidade vem de catalog.
var catalogEN = map[string]text{
	// ---------------------------------------------------------------- host (node_exporter)
	"node_cpu_seconds_total": {
		label: "CPU usage",
		up: Guidance{
			Cause:          "Higher processing load: a process in a loop, a scheduled job, a traffic spike or contention (steal) on the hypervisor.",
			Recommendation: "Find the processes using the most CPU (top/pidstat), check recently scheduled jobs and correlate with traffic and deploys.",
		},
		down: Guidance{
			Cause:          "Lower processing load: a stopped service, diverted traffic or an upstream failure.",
			Recommendation: "Confirm the expected services are running and receiving traffic; check health checks and load balancers.",
		},
	},
	"node_memory_MemAvailable_bytes": {
		label: "Available memory",
		up: Guidance{
			Cause:          "Memory released: a large process exited or restarted, or the cache was dropped.",
			Recommendation: "Check whether a service restarted or was killed unexpectedly (journalctl, OOM killer).",
		},
		down: Guidance{
			Cause:          "Memory pressure: a possible leak, a growing application cache or a new memory-hungry process.",
			Recommendation: "List processes by RSS (ps --sort=-rss), check swap and OOM killer events, and look for a memory leak in the application.",
		},
	},
	"node_disk_io_time_seconds_total": {
		label: "Disk I/O utilization",
		up: Guidance{
			Cause:          "I/O pressure: more reads/writes than usual (backup, compaction, swap or heavy queries) or a degrading device.",
			Recommendation: "Find processes with heavy reads/writes (iotop, pidstat -d), check device latency and queue (iostat -x) and disk error logs (dmesg, SMART).",
		},
		down: Guidance{
			Cause:          "Less disk activity: a service that generates I/O stopped or is blocked.",
			Recommendation: "Confirm the database/storage services are up and processing requests.",
		},
	},
	"node_network_receive_bytes_total": {
		label: "Network traffic received",
		up: Guidance{
			Cause:          "More inbound traffic: a legitimate usage spike, a bulk transfer or possibly malicious traffic.",
			Recommendation: "Find the dominant connections and sources (ss, iftop, proxy logs) and check whether it matches an expected event; consider rate limiting if abusive.",
		},
		down: Guidance{
			Cause:          "Less inbound traffic: an upstream failure, a DNS/load balancer issue or a degraded interface.",
			Recommendation: "Check load balancers, DNS, interface state (ip -s link) and network errors.",
		},
	},
	"node_network_transmit_bytes_total": {
		label: "Network traffic sent",
		up: Guidance{
			Cause:          "More outbound traffic: larger responses, replication/backup or possible data exfiltration.",
			Recommendation: "Find the processes and destinations sending the most (ss, nethogs) and confirm the transfer is expected.",
		},
		down: Guidance{
			Cause:          "Less outbound traffic: a service stopped responding or replicating.",
			Recommendation: "Check that services are responding and that replication/outbound jobs are running.",
		},
	},

	// ---------------------------------------------------------------- nginx (prometheus-nginx-exporter)
	"nginx_up": {
		label: "nginx availability",
		down: Guidance{
			Cause:          "The exporter cannot query stub_status: nginx stopped, hung, reloaded with an error, or stub_status is unreachable.",
			Recommendation: "Check `systemctl status nginx` and `journalctl -u nginx -n 50`, validate the configuration with `sudo nginx -t` and test `curl http://127.0.0.1:8090/stub_status`.",
		},
		up: genericGuidanceEN,
	},
	"nginx_http_requests_total": {
		label: "nginx HTTP requests",
		up: Guidance{
			Cause:          "Traffic spike: a legitimate campaign or event, an aggressive crawler/bot, an attack (DDoS, brute force) or clients retrying.",
			Recommendation: "Find the dominant IPs and routes in access.log (`awk '{print $1}' /var/log/nginx/access.log | sort | uniq -c | sort -rn | head`), consider rate limiting (limit_req) and scale the upstreams if the traffic is legitimate.",
		},
		down: Guidance{
			Cause:          "Traffic drop: DNS or load balancer diverting, an expired TLS certificate, a network block or partial unavailability.",
			Recommendation: "Test the site from outside the network, check DNS and certificate validity and look for errors in /var/log/nginx/error.log.",
		},
	},
	"nginx_connections_active": {
		label: "nginx active connections",
		up: Guidance{
			Cause:          "Connections piling up: a slow upstream holding requests, long keepalives or too many concurrent clients.",
			Recommendation: "Compare with worker_connections × worker_processes, measure upstream latency and look for 'worker_connections are not enough' in error.log.",
		},
		down: Guidance{
			Cause:          "Fewer active connections: traffic diverted or nginx no longer accepting connections.",
			Recommendation: "Check the load balancer, DNS and /var/log/nginx/error.log.",
		},
	},
	"nginx_connections_dropped": {
		label: "Connections dropped by nginx",
		up: Guidance{
			Cause:          "nginx accepted connections it could not handle: the worker_connections or file descriptor limit was reached.",
			Recommendation: "Raise worker_connections and worker_rlimit_nofile (and LimitNOFILE in the nginx unit), check the process `ulimit -n` and error.log.",
		},
		down: genericGuidanceEN,
	},

	// ---------------------------------------------------------------- Apache (prometheus-apache-exporter)
	"apache_up": {
		label: "Apache availability",
		down: Guidance{
			Cause:          "The exporter cannot read mod_status: Apache stopped, hung or /server-status is blocked.",
			Recommendation: "Check `systemctl status apache2` and `journalctl -u apache2 -n 50`, validate with `sudo apache2ctl configtest` and test `curl 'http://localhost/server-status?auto'`.",
		},
		up: genericGuidanceEN,
	},
	"apache_accesses_total": {
		label: "Apache requests",
		up: Guidance{
			Cause:          "Traffic spike: a legitimate event, an aggressive crawler/bot, an attack (DDoS, brute force) or clients retrying.",
			Recommendation: "Find the dominant IPs and URLs in /var/log/apache2/access.log, consider mod_evasive/mod_ratelimit and scale if the traffic is legitimate.",
		},
		down: Guidance{
			Cause:          "Traffic drop: DNS or load balancer diverting, an expired TLS certificate or partial unavailability.",
			Recommendation: "Test the site from outside the network, check DNS and the certificate and look for errors in /var/log/apache2/error.log.",
		},
	},
	"apache_workers_busy": {
		label: "Apache busy workers",
		up: Guidance{
			Cause:          "More busy workers than usual: slow requests (backend, PHP, database) or more traffic.",
			Recommendation: "Look at long requests in /server-status, measure backend response time (PHP-FPM, database) and compare with MaxRequestWorkers.",
		},
		down: Guidance{
			Cause:          "Fewer busy workers than usual: less traffic or Apache refusing requests.",
			Recommendation: "Check incoming traffic and /var/log/apache2/error.log.",
		},
	},
	"apache_workers_busy_ratio": {
		label: "Apache worker utilization",
		up: Guidance{
			Cause:          "Workers almost exhausted: new requests will queue or be refused. Common causes: a slow backend or MaxRequestWorkers too low for the demand.",
			Recommendation: "Look at long requests in /server-status, check the backend (PHP-FPM, database) and, if memory allows, raise MaxRequestWorkers/ServerLimit for the MPM in use.",
		},
		down: genericGuidanceEN,
	},
	"apache_request_duration_ms": {
		label: "Apache mean request time",
		up: Guidance{
			Cause:          "Slower responses: a degraded backend (PHP, database, external API), CPU/I/O contention on the host or heavier requests.",
			Recommendation: "Correlate with host CPU and I/O, check the PHP-FPM/database slow log and find the slowest URLs in access.log (%D).",
		},
		down: Guidance{
			Cause:          "Faster responses than usual: possibly fast errors (4xx/5xx) replacing normal responses.",
			Recommendation: "Check the share of 5xx/4xx codes in access.log and the Apache error.log.",
		},
	},

	// ---------------------------------------------------------------- Docker (métricas do dockerd)
	"docker_containers_running": {
		label: "Running containers",
		down: Guidance{
			Cause:          "Containers stopped: an application crash, an OOM kill, a failed dependency or a manual stop/deploy.",
			Recommendation: "List the stopped ones (`docker ps -a --filter status=exited`), see why (`docker inspect -f '{{.State.ExitCode}} {{.State.OOMKilled}}' <container>`) and the logs (`docker logs --tail 100 <container>`).",
		},
		up: Guidance{
			Cause:          "More containers than usual: a deploy, manual scaling or orphan containers.",
			Recommendation: "Check `docker ps` and remove orphan or duplicate containers.",
		},
	},
	"engine_daemon_health_checks_failed_total": {
		label: "Health check failures (Docker)",
		up: Guidance{
			Cause:          "Containers are failing their HEALTHCHECK: a hung application, a dependency down or a misconfigured check.",
			Recommendation: "Find the unhealthy containers (`docker ps --filter health=unhealthy`) and their check history (`docker inspect -f '{{json .State.Health}}' <container>`).",
		},
		down: genericGuidanceEN,
	},
	"docker_container_starts": {
		label: "Container starts (Docker)",
		up: Guidance{
			Cause:          "Containers restarting in a row (crash loop): the application dies right after starting and the restart policy brings it back.",
			Recommendation: "Find the unstable container (`docker ps -a --format '{{.Names}} {{.Status}}'` and `docker inspect -f '{{.RestartCount}}' <container>`) and read the logs of the last run (`docker logs --tail 100 <container>`).",
		},
		down: genericGuidanceEN,
	},
}

var genericGuidanceEN = Guidance{
	Cause:          "Value outside the metric's usual behavior.",
	Recommendation: "Correlate with deploys, configuration changes and other host metrics in the same period.",
}
