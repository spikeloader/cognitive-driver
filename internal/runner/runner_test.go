package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/spikeloader/cognitive-driver/internal/attribution"
	"github.com/spikeloader/cognitive-driver/internal/collector"
	"github.com/spikeloader/cognitive-driver/internal/config"
	"github.com/spikeloader/cognitive-driver/internal/driver"
	"github.com/spikeloader/cognitive-driver/internal/store"
	"github.com/spikeloader/cognitive-driver/internal/telemetry"
)

// fakeCPU simula node_cpu_seconds_total de um host com 2 núcleos.
type fakeCPU struct {
	ts         time.Time
	busy, idle float64
	rate       func(i int) float64 // núcleos ocupados na rodada i
	i          int
}

func (f *fakeCPU) Host() string { return "node-1" }
func (f *fakeCPU) Job() string  { return "node" }

func (f *fakeCPU) Collect(context.Context) (collector.Snapshot, error) {
	r := f.rate(f.i)
	f.i++
	f.ts = f.ts.Add(15 * time.Second)
	f.busy += r * 15
	f.idle += (2 - r) * 15
	return collector.Snapshot{Host: "node-1", Job: "node", Timestamp: f.ts, Series: []collector.Series{
		{Name: "node_cpu_seconds_total", Labels: map[string]string{"cpu": "0", "mode": "user"}, Value: f.busy},
		{Name: "node_cpu_seconds_total", Labels: map[string]string{"cpu": "0", "mode": "idle"}, Value: f.idle},
	}}, nil
}

func newRunner(t *testing.T, c collector.Collector) (*Runner, *store.Store, *telemetry.Metrics) {
	t.Helper()
	cfg, err := config.Parse([]byte(`
collector:
  targets: [{host: node-1, url: "http://unused:9100/metrics"}]
drivers:
  - name: ai-host-health
    metrics:
      - name: node_cpu_seconds_total
        exclude: {mode: idle}
`))
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	m := telemetry.New()
	return &Runner{
		Interval: 15 * time.Second,
		Timeout:  time.Second,
		Targets:  []collector.Collector{c},
		Drivers:  []driver.Driver{driver.NewStatistical(cfg.Drivers[0], cfg.Collector.Interval)},
		Store:    st,
		Metrics:  m,
		Status:   collector.NewStatusTracker(),
		Log:      slog.New(slog.NewJSONHandler(io.Discard, nil)),
	}, st, m
}

func TestCPUSpikeGeneratesEvent(t *testing.T) {
	spikeAt := 60
	c := &fakeCPU{ts: time.Unix(1_700_000_000, 0), rate: func(i int) float64 {
		if i >= spikeAt && i < spikeAt+3 {
			return 1.9
		}
		return 0.3 + 0.02*math.Sin(float64(i))
	}}
	r, st, m := newRunner(t, c)
	ctx := context.Background()

	for range spikeAt + 2 {
		r.Tick(ctx)
	}
	if evs, _ := st.ListEvents(ctx, store.Filter{}); len(evs) != 0 {
		t.Fatalf("evento antes de 3 pontos consecutivos: %+v", evs)
	}
	r.Tick(ctx)

	evs, err := st.ListEvents(ctx, store.Filter{})
	if err != nil || len(evs) != 1 {
		t.Fatalf("esperado 1 evento, obtido %d (%v)", len(evs), err)
	}
	e := evs[0]
	if e.Driver != "ai-host-health" || e.Host != "node-1" || e.Metric != "node_cpu_seconds_total" ||
		e.Status != store.StatusOpen || e.Mode != config.ModeObserveOnly {
		t.Errorf("metadados do evento incorretos: %+v", e)
	}
	if e.ZScore < 3 || e.Severity == "" || math.Abs(e.Value-1.9) > 1e-9 {
		t.Errorf("evidência numérica incorreta: value=%v z=%v sev=%q", e.Value, e.ZScore, e.Severity)
	}
	if !strings.Contains(e.Explanation, "Uso de CPU") || e.ProbableCause == "" || e.Recommendation == "" {
		t.Errorf("explicação incompleta: %+v", e)
	}
	if got := testutil.ToFloat64(m.ActionsBlocked.WithLabelValues("ai-host-health")); got != 1 {
		t.Errorf("ação deveria ter sido bloqueada pela política, actions_blocked=%v", got)
	}
	if got := testutil.ToFloat64(m.Events.WithLabelValues("ai-host-health", e.Metric, e.Severity)); got != 1 {
		t.Errorf("events_total=%v, esperado 1", got)
	}
}

// Um novo pico depois do cooldown, com o evento anterior ainda aberto,
// entra como ocorrência desse evento em vez de abrir outro.
func TestReemissionUpdatesOpenEvent(t *testing.T) {
	spikes := []int{60, 100} // 10 min entre os picos (cooldown de 5 min)
	c := &fakeCPU{ts: time.Unix(1_700_000_000, 0), rate: func(i int) float64 {
		for _, at := range spikes {
			if i >= at && i < at+3 {
				return 1.9
			}
		}
		return 0.3 + 0.02*math.Sin(float64(i))
	}}
	r, st, m := newRunner(t, c)
	ctx := context.Background()
	for range spikes[1] + 3 {
		r.Tick(ctx)
	}

	evs, err := st.ListEvents(ctx, store.Filter{})
	if err != nil || len(evs) != 1 {
		t.Fatalf("esperado 1 evento, obtido %d (%v)", len(evs), err)
	}
	emitted := 0.0
	for _, sev := range []string{"low", "medium", "high", "critical"} {
		emitted += testutil.ToFloat64(m.Events.WithLabelValues("ai-host-health", evs[0].Metric, sev))
	}
	if emitted != 2 || evs[0].Occurrences != 2 {
		t.Fatalf("ocorrências=%d, emissões=%v: esperado 2 e 2", evs[0].Occurrences, emitted)
	}
	if !evs[0].LastSeen.After(evs[0].Timestamp) {
		t.Errorf("last_seen deveria avançar: %+v", evs[0])
	}
}

func TestStableLoadGeneratesNoEvent(t *testing.T) {
	c := &fakeCPU{ts: time.Unix(0, 0), rate: func(i int) float64 { return 0.5 + 0.05*math.Sin(float64(i)) }}
	r, st, _ := newRunner(t, c)
	for range 200 {
		r.Tick(context.Background())
	}
	if evs, _ := st.ListEvents(context.Background(), store.Filter{}); len(evs) != 0 {
		t.Fatalf("carga estável não deveria gerar eventos: %+v", evs)
	}
}

func TestBaselinesSurviveRestart(t *testing.T) {
	c := &fakeCPU{ts: time.Unix(0, 0), rate: func(int) float64 { return 0.5 }}
	r, st, _ := newRunner(t, c)
	ctx := context.Background()
	for range 30 {
		r.Tick(ctx)
	}
	if err := r.SaveBaselines(ctx); err != nil {
		t.Fatal(err)
	}

	r2, _, _ := newRunner(t, c)
	r2.Store = st
	if err := r2.RestoreBaselines(ctx); err != nil {
		t.Fatal(err)
	}
	p := r2.Drivers[0].(Persistent).Baselines()
	if len(p) != 1 || p[0].State.Baselines[0].N < 20 {
		t.Fatalf("baseline não restaurada: %+v", p)
	}
}

// fakeNginx serve a saída real do exporter (fixture) com nginx_up controlável.
type fakeNginx struct {
	ts   time.Time
	up   func(i int) float64
	i    int
	text string
}

func (f *fakeNginx) Host() string { return "node-1" }
func (f *fakeNginx) Job() string  { return "ai-nginx-health" }

func (f *fakeNginx) Collect(context.Context) (collector.Snapshot, error) {
	series, err := collector.ParseText(strings.NewReader(f.text), nil)
	if err != nil {
		return collector.Snapshot{}, err
	}
	for i := range series {
		switch series[i].Name {
		case "nginx_up":
			series[i].Value = f.up(f.i)
		case "nginx_connections_accepted", "nginx_connections_handled":
			series[i].Value += float64(f.i) * 100
		case "nginx_http_requests_total":
			series[i].Value += float64(f.i) * 300
		}
	}
	f.i++
	f.ts = f.ts.Add(15 * time.Second)
	return collector.Snapshot{Host: "node-1", Job: "ai-nginx-health", Timestamp: f.ts, Series: series}, nil
}

func TestNginxDownGeneratesRuleEvent(t *testing.T) {
	fixture, err := os.ReadFile("../feature/testdata/nginx.prom")
	if err != nil {
		t.Fatal(err)
	}
	tmpl, err := os.ReadFile("../../deploy/drivers/nginx.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse(append([]byte(`
collector:
  targets: [{host: node-1, url: "http://unused:9100/metrics"}]
drivers:
  - name: ai-host-health
    metrics: [node_memory_MemAvailable_bytes]
`), []byte(strings.Replace(strings.Replace(string(tmpl), "drivers:\n", "", 1), "host: local", "host: node-1", 1))...))
	if err != nil {
		t.Fatal(err)
	}
	host := driver.NewStatistical(cfg.Drivers[0], cfg.Collector.Interval)
	nginx := driver.NewStatistical(cfg.Drivers[1], cfg.Collector.Interval)

	c := &fakeNginx{ts: time.Unix(1_700_000_000, 0), text: string(fixture), up: func(i int) float64 {
		if i >= 5 {
			return 0
		}
		return 1
	}}
	r, st, m := newRunner(t, c)
	r.Drivers = []driver.Driver{host, nginx}
	ctx := context.Background()
	for range 7 {
		r.Tick(ctx)
	}
	if evs, _ := st.ListEvents(ctx, store.Filter{}); len(evs) != 0 {
		t.Fatalf("2 pontos com nginx_up=0 não deveriam gerar evento: %+v", evs)
	}
	r.Tick(ctx)

	evs, _ := st.ListEvents(ctx, store.Filter{})
	if len(evs) != 1 {
		t.Fatalf("esperado 1 evento, obtido %d", len(evs))
	}
	e := evs[0]
	if e.Driver != "ai-nginx-health" || e.Metric != "nginx_up" || e.Detector != "rule" || e.Rule != "equals 1" ||
		e.Severity != "critical" || e.MetricLabel != "Disponibilidade do nginx" || e.Unit != "bool" {
		t.Errorf("evento de regra incorreto: %+v", e)
	}
	if !strings.Contains(e.Explanation, "o esperado é ativo (1)") || !strings.Contains(e.Recommendation, "nginx -t") {
		t.Errorf("explicação/recomendação do nginx: %q / %q", e.Explanation, e.Recommendation)
	}
	// O snapshot do nginx não pode alimentar o driver de host (job diferente).
	if got := testutil.ToFloat64(m.Predictions.WithLabelValues("ai-host-health", "node_memory_MemAvailable_bytes")); got != 0 {
		t.Errorf("driver de host recebeu dados do job nginx: %v previsões", got)
	}
	if got := testutil.ToFloat64(m.Predictions.WithLabelValues("ai-nginx-health", "nginx_http_requests_total")); got == 0 {
		t.Error("driver do nginx deveria ter previsões de requisições")
	}
}

// fakeCron publica as métricas do cron-wrap de dois jobs pelo node_exporter.
type fakeCron struct {
	ts       time.Time
	syncExit float64
}

func (f *fakeCron) Host() string { return "node-1" }
func (f *fakeCron) Job() string  { return "node" }

func (f *fakeCron) Collect(context.Context) (collector.Snapshot, error) {
	f.ts = f.ts.Add(15 * time.Second)
	exit := func(job string, v float64) collector.Series {
		return collector.Series{Name: "cron_job_last_exit_code", Labels: map[string]string{"job": job}, Value: v}
	}
	return collector.Snapshot{Host: "node-1", Job: "node", Timestamp: f.ts, Series: []collector.Series{
		exit("app.backup", 0), exit("app.sync", f.syncExit),
	}}, nil
}

func TestCronJobFailureOpensEventPerJob(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "deploy", "drivers", "cron.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse(append([]byte("collector:\n  targets: [{host: node-1, url: \"http://unused:9100/metrics\"}]\n"), data...))
	if err != nil {
		t.Fatal(err)
	}
	c := &fakeCron{ts: time.Unix(1_700_000_000, 0), syncExit: 2}
	r, st, m := newRunner(t, c)
	r.Drivers = []driver.Driver{driver.NewStatistical(cfg.Drivers[0], cfg.Collector.Interval)}
	ctx := context.Background()
	for range 3 { // consecutive_points
		r.Tick(ctx)
	}

	evs, err := st.ListEvents(ctx, store.Filter{})
	if err != nil || len(evs) != 1 {
		t.Fatalf("esperado 1 evento (só o job com falha), obtido %d (%v): %+v", len(evs), err, evs)
	}
	e := evs[0]
	if e.Metric != `cron_job_failed{job="app.sync"}` || e.MetricLabel != "Código de saída do job cron · app.sync" ||
		e.Severity != "high" || e.Value != 2 {
		t.Errorf("evento do job incorreto: %+v", e)
	}
	if !strings.Contains(e.Explanation, "app.sync") || !strings.Contains(e.Recommendation, "MAILTO") {
		t.Errorf("explicação deveria citar o job e usar o describe: %q / %q", e.Explanation, e.Recommendation)
	}
	// Contadores Prometheus usam a métrica base, sem o job.
	if got := testutil.ToFloat64(m.Events.WithLabelValues("ai-cron-health", "cron_job_failed", "high")); got != 1 {
		t.Errorf("events_total{metric=cron_job_failed}=%v, esperado 1", got)
	}

	// O estado por job sobrevive ao restart (cooldown e chave da série).
	if err := r.SaveBaselines(ctx); err != nil {
		t.Fatal(err)
	}
	d2 := driver.NewStatistical(cfg.Drivers[0], cfg.Collector.Interval)
	r2, _, _ := newRunner(t, c)
	r2.Store, r2.Drivers = st, []driver.Driver{d2}
	if err := r2.RestoreBaselines(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(d2.Baselines()); n != 2 {
		t.Fatalf("esperados 2 estados restaurados (um por job), obtido %d", n)
	}
}

// processExporter serve contadores de CPU que avançam com o tempo real:
// stress-ng usa ~1,9 núcleo e sshd quase nada.
func processExporter(t *testing.T) string {
	t.Helper()
	start := time.Now()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		s := time.Since(start).Seconds() + 10
		fmt.Fprintf(w, "namedprocess_namegroup_cpu_seconds_total{groupname=\"stress-ng\",mode=\"user\"} %g\n", 1.9*s)
		fmt.Fprintf(w, "namedprocess_namegroup_cpu_seconds_total{groupname=\"sshd\",mode=\"user\"} %g\n", 0.01*s)
		fmt.Fprintln(w, "namedprocess_namegroup_num_procs{groupname=\"stress-ng\"} 2")
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/metrics"
}

func newAttributedRunner(t *testing.T, c collector.Collector, processURL string) (*Runner, *store.Store) {
	t.Helper()
	cfg, err := config.Parse([]byte(`
collector:
  targets: [{host: node-1, url: "http://unused:9100/metrics"}]
drivers:
  - name: ai-host-health
    metrics:
      - name: node_cpu_seconds_total
        exclude: {mode: idle}
        attribute: cpu
`))
	if err != nil {
		t.Fatal(err)
	}
	r, st, _ := newRunner(t, c)
	r.Drivers = []driver.Driver{driver.NewStatistical(cfg.Drivers[0], cfg.Collector.Interval)}
	r.Attributor = attribution.New(map[string]string{"node-1": processURL}, 50*time.Millisecond, time.Second, 5)
	return r, st
}

func cpuSpike(spikeAt int) *fakeCPU {
	return &fakeCPU{ts: time.Unix(1_700_000_000, 0), rate: func(i int) float64 {
		if i >= spikeAt && i < spikeAt+3 {
			return 1.9
		}
		return 0.3 + 0.02*math.Sin(float64(i))
	}}
}

func TestEventShowsResponsibleProcess(t *testing.T) {
	r, st := newAttributedRunner(t, cpuSpike(60), processExporter(t))
	ctx := context.Background()
	for range 63 {
		r.Tick(ctx)
	}
	evs, err := st.ListEvents(ctx, store.Filter{})
	if err != nil || len(evs) != 1 {
		t.Fatalf("esperado 1 evento, obtido %d (%v)", len(evs), err)
	}
	var res attribution.Result
	if err := json.Unmarshal(evs[0].Attribution, &res); err != nil {
		t.Fatalf("atribuição ausente ou inválida: %q (%v)", evs[0].Attribution, err)
	}
	p := res.Responsible()
	if p == nil || p.Group != "stress-ng" || math.Abs(p.Value-1.9) > 0.3 || p.Procs != 2 || res.Resource != "cpu" {
		t.Fatalf("responsável incorreto: %+v", res)
	}
}

func TestEventWithoutProcessExporterStillOpens(t *testing.T) {
	r, st := newAttributedRunner(t, cpuSpike(60), "http://127.0.0.1:1/metrics")
	ctx := context.Background()
	for range 63 {
		r.Tick(ctx)
	}
	evs, _ := st.ListEvents(ctx, store.Filter{})
	if len(evs) != 1 || len(evs[0].Attribution) != 0 {
		t.Fatalf("evento deveria abrir sem atribuição: %+v", evs)
	}
}
