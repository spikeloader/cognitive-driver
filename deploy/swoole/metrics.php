<?php
// Métricas do Swoole/OpenSwoole no formato do Prometheus, lidas pelo
// driver ai-swoole-health (deploy/drivers/swoole.yaml).
//
// Exemplo em um servidor HTTP:
//
//   require __DIR__ . '/metrics.php';
//   $server->on('request', function ($request, $response) use ($server) {
//       if ($request->server['request_uri'] === '/metrics'
//           && $request->server['remote_addr'] === '127.0.0.1') {
//           $response->header('Content-Type', 'text/plain; version=0.0.4');
//           $response->end(swoole_metrics($server, 'api'));
//           return;
//       }
//       // ... rotas da aplicação
//   });
//
// Servidores TCP/WebSocket sem rota HTTP: abra uma porta extra só para as
// métricas com $server->listen('127.0.0.1', 9502, SWOOLE_SOCK_TCP) e
// configure ['open_http_protocol' => true] nela.

/**
 * @param object $server \Swoole\Server ou \OpenSwoole\Server
 * @param string $name   identifica o servidor quando há mais de um no host
 */
function swoole_metrics(object $server, string $name = 'default'): string
{
    $s = $server->stats();
    if (!is_array($s)) {
        return '';
    }
    $workers = (int) ($s['worker_num'] ?? 0);
    $idle = (int) ($s['idle_worker_num'] ?? 0);
    $metrics = [
        'swoole_connections' => ['gauge', $s['connection_num'] ?? 0],
        'swoole_requests_total' => ['counter', $s['request_count'] ?? 0],
        'swoole_workers' => ['gauge', $workers],
        'swoole_workers_idle' => ['gauge', $idle],
        'swoole_workers_busy_ratio' => ['gauge', $workers > 0 ? ($workers - $idle) / $workers : 0],
        'swoole_coroutines' => ['gauge', $s['coroutine_num'] ?? 0],
        'swoole_task_queue' => ['gauge', $s['task_queue_num'] ?? 0],
        'swoole_tasking_workers' => ['gauge', $s['tasking_num'] ?? 0],
    ];
    $label = '{server="' . addcslashes($name, "\\\"\n") . '"}';
    $out = '';
    foreach ($metrics as $metric => [$type, $value]) {
        $out .= "# TYPE $metric $type\n$metric$label " . (0 + $value) . "\n";
    }
    return $out;
}
