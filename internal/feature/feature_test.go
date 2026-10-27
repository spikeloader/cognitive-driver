package feature

import (
	"math"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/spikeloader/cognitive-driver/internal/collector"
	"github.com/spikeloader/cognitive-driver/internal/config"
)

func cpu(cpuID, mode string, v float64) collector.Series {
	return collector.Series{Name: "node_cpu_seconds_total", Labels: map[string]string{"cpu": cpuID, "mode": mode}, Value: v}
}

func TestCounterRateWithExclude(t *testing.T) {
	e := NewEngine([]config.Metric{{Name: "node_cpu_seconds_total", Exclude: map[string]string{"mode": "idle"}}})
	t0 := time.Unix(1000, 0)
	snap := func(ts time.Time, user0, user1, idle float64) collector.Snapshot {
		return collector.Snapshot{Host: "h", Timestamp: ts, Series: []collector.Series{
			cpu("0", "user", user0), cpu("1", "user", user1), cpu("0", "idle", idle),
		}}
	}
	if f := e.Compute(snap(t0, 10, 20, 100)); len(f) != 0 {
		t.Fatalf("primeira coleta não deveria gerar feature de contador: %+v", f)
	}
	f := e.Compute(snap(t0.Add(10*time.Second), 15, 22, 200))
	if len(f) != 1 {
		t.Fatalf("esperada 1 feature, obtido %+v", f)
	}
	// (5 + 2) / 10s = 0.7 núcleos; idle excluído.
	if math.Abs(f[0].Value-0.7) > 1e-9 || f[0].Series != 2 {
		t.Fatalf("taxa incorreta: %+v", f[0])
	}
}

func TestCounterResetSkipsRound(t *testing.T) {
	e := NewEngine([]config.Metric{{Name: "node_network_receive_bytes_total"}})
	t0 := time.Unix(0, 0)
	s := func(ts time.Time, v float64) collector.Snapshot {
		return collector.Snapshot{Host: "h", Timestamp: ts, Series: []collector.Series{
			{Name: "node_network_receive_bytes_total", Labels: map[string]string{"device": "eth0"}, Value: v},
		}}
	}
	e.Compute(s(t0, 1000))
	if f := e.Compute(s(t0.Add(time.Second), 10)); len(f) != 0 {
		t.Fatalf("reset de contador não deveria gerar feature: %+v", f)
	}
	f := e.Compute(s(t0.Add(2*time.Second), 110))
	if len(f) != 1 || f[0].Value != 100 {
		t.Fatalf("após reset esperado 100 B/s, obtido %+v", f)
	}
}

func TestGaugeAndHostsAreIndependent(t *testing.T) {
	e := NewEngine([]config.Metric{{Name: "node_memory_MemAvailable_bytes"}, {Name: "node_disk_io_time_seconds_total", Match: map[string]string{"device": "sd.*"}}})
	ts := time.Unix(0, 0)
	mk := func(host string, ts time.Time, io float64) collector.Snapshot {
		return collector.Snapshot{Host: host, Timestamp: ts, Series: []collector.Series{
			{Name: "node_memory_MemAvailable_bytes", Labels: map[string]string{}, Value: 4096},
			{Name: "node_disk_io_time_seconds_total", Labels: map[string]string{"device": "sda"}, Value: io},
			{Name: "node_disk_io_time_seconds_total", Labels: map[string]string{"device": "loop0"}, Value: io * 100},
		}}
	}
	if f := e.Compute(mk("a", ts, 1)); len(f) != 1 || f[0].Value != 4096 {
		t.Fatalf("gauge deveria sair na 1ª coleta: %+v", f)
	}
	// Host b nunca foi visto: contador sem histórico não deve usar o estado de a.
	if f := e.Compute(mk("b", ts.Add(time.Second), 2)); len(f) != 1 {
		t.Fatalf("host b não deveria ter taxa ainda: %+v", f)
	}
	f := e.Compute(mk("a", ts.Add(2*time.Second), 2))
	if len(f) != 2 || f[1].Value != 0.5 {
		t.Fatalf("taxa de disco de a com match=sd.* esperada 0.5: %+v", f)
	}
}

func TestMissingMetricProducesNothing(t *testing.T) {
	e := NewEngine([]config.Metric{{Name: "node_memory_MemAvailable_bytes"}})
	if f := e.Compute(collector.Snapshot{Host: "h", Timestamp: time.Now()}); len(f) != 0 {
		t.Fatalf("esperado nenhuma feature: %+v", f)
	}
}

// snapshotFromFixture lê uma saída real de exporter e soma delta aos contadores.
func snapshotFromFixture(t *testing.T, file string, ts time.Time, delta map[string]float64) collector.Snapshot {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", file))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	series, err := collector.ParseText(f, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := range series {
		series[i].Value += delta[series[i].Name]
	}
	return collector.Snapshot{Host: "web-1", Job: "x", Timestamp: ts, Series: series}
}

func loadDriver(t *testing.T, file string) config.Driver {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "deploy", "drivers", file))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse(append([]byte("collector:\n  targets: [{host: h, url: \"http://h:9100/metrics\"}]\n"), data...))
	if err != nil {
		t.Fatal(err)
	}
	return cfg.Drivers[0]
}

func byMetric(fs []Feature) map[string]float64 {
	out := map[string]float64{}
	for _, f := range fs {
		out[f.Metric] = f.Value
	}
	return out
}

func TestNginxDriverFeatures(t *testing.T) {
	e := NewEngine(loadDriver(t, "nginx.yaml").Metrics)
	t0 := time.Unix(1000, 0)
	first := byMetric(e.Compute(snapshotFromFixture(t, "nginx.prom", t0, nil)))
	if first["nginx_up"] != 1 || first["nginx_connections_active"] != 291 || len(first) != 2 {
		t.Fatalf("1ª coleta deveria trazer só os gauges: %v", first)
	}
	// Em 10 s: +500 aceitas, +480 tratadas (20 descartadas), +1500 requisições.
	got := byMetric(e.Compute(snapshotFromFixture(t, "nginx.prom", t0.Add(10*time.Second), map[string]float64{
		"nginx_connections_accepted": 500, "nginx_connections_handled": 480, "nginx_http_requests_total": 1500,
	})))
	if got["nginx_http_requests_total"] != 150 || got["nginx_connections_dropped"] != 2 {
		t.Fatalf("taxas derivadas incorretas: %v", got)
	}
}

func TestApacheDriverFeatures(t *testing.T) {
	e := NewEngine(loadDriver(t, "apache.yaml").Metrics)
	t0 := time.Unix(1000, 0)
	e.Compute(snapshotFromFixture(t, "apache.prom", t0, nil))
	// apache_accesses_total alimenta duas features (taxa e divisor da latência).
	got := byMetric(e.Compute(snapshotFromFixture(t, "apache.prom", t0.Add(10*time.Second), map[string]float64{
		"apache_accesses_total": 200, "apache_duration_ms_total": 9000,
	})))
	// Fixture: 3 workers ocupados, scoreboard com 60 slots no total.
	want := map[string]float64{
		"apache_up": 1, "apache_accesses_total": 20, "apache_workers_busy": 3,
		"apache_workers_busy_ratio": 3.0 / 60, "apache_request_duration_ms": 45,
	}
	for k, v := range want {
		if math.Abs(got[k]-v) > 1e-9 {
			t.Errorf("%s = %v, esperado %v", k, got[k], v)
		}
	}
}

func TestDivideByZeroSkipsFeature(t *testing.T) {
	e := NewEngine(loadDriver(t, "apache.yaml").Metrics)
	t0 := time.Unix(1000, 0)
	e.Compute(snapshotFromFixture(t, "apache.prom", t0, nil))
	got := byMetric(e.Compute(snapshotFromFixture(t, "apache.prom", t0.Add(10*time.Second), nil)))
	if _, ok := got["apache_request_duration_ms"]; ok {
		t.Fatalf("sem requisições no intervalo a latência não deveria ser calculada: %v", got)
	}
}

func TestSourceMetricsIncludeOperands(t *testing.T) {
	e := NewEngine(loadDriver(t, "nginx.yaml").Metrics)
	src := e.SourceMetrics()
	for _, want := range []string{"nginx_up", "nginx_connections_accepted", "nginx_connections_handled"} {
		if !slices.Contains(src, want) {
			t.Errorf("SourceMetrics sem %s: %v", want, src)
		}
	}
}

func TestCronDriverFeatures(t *testing.T) {
	e := NewEngine(loadDriver(t, "cron.yaml").Metrics)
	got := byMetric(e.Compute(snapshotFromFixture(t, "cron.prom", time.Unix(1000, 0), nil)))
	want := map[string]float64{
		`cron_job_failed{job="app.backup"}`:    0,
		`cron_job_failed{job="app.sync"}`:      2,
		`cron_job_failed{job="app.irregular"}`: 0,
		// (1000 - 400) / 300: duas janelas sem iniciar.
		`cron_job_overdue{job="app.backup"}`: 2,
		`cron_job_overdue{job="app.sync"}`:   100.0 / 60,
		// Rodando há 100 s num job de 60 s.
		`cron_job_stuck{job="app.sync"}`:            100.0 / 60,
		`cron_job_duration_ratio{job="app.backup"}`: 0.8,
		`cron_job_duration_ratio{job="app.sync"}`:   0.5,
	}
	if len(got) != len(want) {
		t.Fatalf("features: obtido %v, esperado %v", got, want)
	}
	for k, v := range want {
		if g, ok := got[k]; !ok || math.Abs(g-v) > 1e-9 {
			t.Errorf("%s = %v (presente: %v), esperado %v", k, g, ok, v)
		}
	}
}

func TestPHPFPMDriverFeatures(t *testing.T) {
	e := NewEngine(loadDriver(t, "php-fpm.yaml").Metrics)
	t0 := time.Unix(1000, 0)
	first := byMetric(e.Compute(snapshotFromFixture(t, "phpfpm.prom", t0, nil)))
	if first[`phpfpm_busy_ratio{pool="www"}`] != 0.95 || first[`phpfpm_busy_ratio{pool="api"}`] != 0.2 ||
		first[`phpfpm_listen_queue{pool="www"}`] != 4 || first[`phpfpm_up{pool="api"}`] != 1 || len(first) != 6 {
		t.Fatalf("1ª coleta deveria trazer só os gauges, por pool: %v", first)
	}
	// Em 10 s, somado nos dois pools: o delta vale para cada série.
	got := byMetric(e.Compute(snapshotFromFixture(t, "phpfpm.prom", t0.Add(10*time.Second), map[string]float64{
		"phpfpm_accepted_connections": 300, "phpfpm_max_children_reached": 2,
	})))
	if got[`phpfpm_requests{pool="www"}`] != 30 || got[`phpfpm_max_children_reached{pool="www"}`] != 0.2 ||
		got[`phpfpm_slow_requests{pool="api"}`] != 0 || len(got) != 12 {
		t.Fatalf("taxas por pool incorretas: %v", got)
	}
}

func TestSwooleDriverFeatures(t *testing.T) {
	e := NewEngine(loadDriver(t, "swoole.yaml").Metrics)
	t0 := time.Unix(1000, 0)
	first := byMetric(e.Compute(snapshotFromFixture(t, "swoole.prom", t0, nil)))
	if first[`swoole_workers_busy_ratio{server="api"}`] != 1 || first[`swoole_coroutines{server="api"}`] != 340 || len(first) != 4 {
		t.Fatalf("1ª coleta: %v", first)
	}
	got := byMetric(e.Compute(snapshotFromFixture(t, "swoole.prom", t0.Add(10*time.Second), map[string]float64{"swoole_requests_total": 2500})))
	if got[`swoole_requests_total{server="api"}`] != 250 {
		t.Fatalf("taxa de requisições incorreta: %v", got)
	}
}

func TestGroupByKeepsGroupsIndependent(t *testing.T) {
	e := NewEngine([]config.Metric{{Operand: config.Operand{Name: "jobs_total"}, GroupBy: []string{"job"}}})
	s := func(ts int64, a, b float64, withB bool) collector.Snapshot {
		series := []collector.Series{{Name: "jobs_total", Labels: map[string]string{"job": "a"}, Value: a}}
		if withB {
			series = append(series, collector.Series{Name: "jobs_total", Labels: map[string]string{"job": "b"}, Value: b})
		}
		series = append(series, collector.Series{Name: "jobs_total", Labels: map[string]string{}, Value: 99}) // sem o label: ignorada
		return collector.Snapshot{Host: "h", Timestamp: time.Unix(ts, 0), Series: series}
	}
	e.Compute(s(0, 10, 0, false))
	// Série nova (b) não tem taxa ainda: só ela é pulada, a já conhecida (a) segue.
	got := byMetric(e.Compute(s(10, 20, 5, true)))
	if len(got) != 1 || got[`jobs_total{job="a"}`] != 1 {
		t.Fatalf("grupos deveriam ser independentes: %v", got)
	}
	got = byMetric(e.Compute(s(20, 30, 25, true)))
	if got[`jobs_total{job="a"}`] != 1 || got[`jobs_total{job="b"}`] != 2 {
		t.Fatalf("taxas por grupo incorretas: %v", got)
	}
}

func TestSeriesHelpers(t *testing.T) {
	id := SeriesID("cron_job_failed", `job="a\"b",pool="www"`)
	if k, g := SplitSeries(id); k != "cron_job_failed" || g != `job="a\"b",pool="www"` {
		t.Fatalf("SplitSeries(%q) = %q, %q", id, k, g)
	}
	if v := GroupValues(`job="a\"b",pool="www"`); v != `a"b / www` {
		t.Fatalf("GroupValues = %q", v)
	}
	if k, g := SplitSeries("node_load1"); k != "node_load1" || g != "" {
		t.Fatalf("métrica sem grupo: %q %q", k, g)
	}
}
