package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/spikeloader/cognitive-driver/internal/collector"
	"github.com/spikeloader/cognitive-driver/internal/config"
	"github.com/spikeloader/cognitive-driver/internal/driver"
	"github.com/spikeloader/cognitive-driver/internal/feature"
	"github.com/spikeloader/cognitive-driver/internal/store"
	"github.com/spikeloader/cognitive-driver/internal/telemetry"
)

const token = "s3cret"

type fixture struct {
	srv     *httptest.Server
	store   *store.Store
	metrics *telemetry.Metrics
}

func setup(t *testing.T) fixture {
	t.Helper()
	cfg, err := config.Parse([]byte(`
collector:
  targets: [{host: node-1, url: "http://unused:9100/metrics"}]
drivers:
  - name: ai-disk-health
    metrics: [node_disk_io_time_seconds_total]
  - name: ai-off
    enabled: false
    metrics: [node_load1]
`))
	if err != nil {
		t.Fatal(err)
	}
	disk := driver.NewStatistical(cfg.Drivers[0], cfg.Collector.Interval)
	off := driver.NewStatistical(cfg.Drivers[1], cfg.Collector.Interval)
	// Baseline de I/O em torno de 0,12 com desvio ~0,03.
	ts := time.Unix(0, 0)
	for i := range 60 {
		v := 0.12 + 0.03*float64(i%2*2-1)
		disk.Predict(feature.Feature{Host: "node-1", Metric: "node_disk_io_time_seconds_total", Value: v, Timestamp: ts})
		ts = ts.Add(15 * time.Second)
	}

	st, err := store.Open(filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	m := telemetry.New()
	s := &Server{
		Version:     "test",
		Drivers:     []driver.Driver{disk, off},
		DefaultHost: "node-1",
		Store:       st,
		Metrics:     m,
		Status:      collector.NewStatusTracker(),
		Token:       token,
		Log:         slog.New(slog.NewJSONHandler(io.Discard, nil)),
		Started:     time.Now(),
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return fixture{srv: srv, store: st, metrics: m}
}

func (f fixture) do(t *testing.T, method, path, body string, auth bool) (*http.Response, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, f.srv.URL+path, strings.NewReader(body))
	if auth {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("%s %s: JSON inválido: %v\n%s", method, path, err, raw)
		}
	}
	return resp, out
}

func TestHealth(t *testing.T) {
	f := setup(t)
	resp, body := f.do(t, "GET", "/health", "", false)
	if resp.StatusCode != 200 || body["status"] != "ok" || body["version"] != "test" {
		t.Fatalf("health inesperado: %d %v", resp.StatusCode, body)
	}
	drivers := body["drivers"].([]any)
	if len(drivers) != 2 || drivers[1].(map[string]any)["enabled"] != false {
		t.Fatalf("drivers no health incorretos: %v", drivers)
	}
}

func TestMetricsEndpoint(t *testing.T) {
	f := setup(t)
	resp, _ := f.do(t, "GET", "/metrics", "", false)
	raw, _ := http.Get(f.srv.URL + "/metrics")
	b, _ := io.ReadAll(raw.Body)
	raw.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(b), "go_memstats_alloc_bytes") {
		t.Fatalf("/metrics sem métricas de runtime: %d", resp.StatusCode)
	}
}

func TestUIServed(t *testing.T) {
	f := setup(t)
	for path, want := range map[string]string{
		"/ui/":          "<title>Cognitive Driver</title>",
		"/ui/app.js":    "../events?limit=",
		"/ui/style.css": "--sev-critical",
		"/ui/icon.svg":  "<svg",
	} {
		resp, err := http.Get(f.srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || !strings.Contains(string(b), want) {
			t.Errorf("GET %s: status %d, conteúdo sem %q", path, resp.StatusCode, want)
		}
		if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self'") {
			t.Errorf("GET %s sem CSP restritiva: %q", path, csp)
		}
	}

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get(f.srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/ui/" {
		t.Errorf("GET / deveria redirecionar para /ui/: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	for _, path := range []string{"/favicon.ico", "/ui/favicon-32.png", "/ui/apple-touch-icon.png"} {
		resp, err := http.Get(f.srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "image/png" {
			t.Errorf("GET %s: %d %q, esperado PNG", path, resp.StatusCode, resp.Header.Get("Content-Type"))
		}
	}
}

func TestAuthRequired(t *testing.T) {
	f := setup(t)
	for _, p := range []string{"/events", "/events/x"} {
		if resp, _ := f.do(t, "GET", p, "", false); resp.StatusCode != 401 {
			t.Errorf("GET %s sem token: %d, esperado 401", p, resp.StatusCode)
		}
	}
	if resp, _ := f.do(t, "POST", "/predict", `{}`, false); resp.StatusCode != 401 {
		t.Errorf("POST /predict sem token: %d", resp.StatusCode)
	}
}

func TestPredictAnomaly(t *testing.T) {
	f := setup(t)
	resp, body := f.do(t, "POST", "/predict",
		`{"driver":"ai-disk-health","metric":"node_disk_io_time_seconds_total","value":0.41}`, true)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %v", resp.StatusCode, body)
	}
	if body["anomaly"] != true || body["severity"] == "" || body["mode"] != "observe_only" ||
		body["driver"] != "ai-disk-health" || body["host"] != "node-1" {
		t.Fatalf("resposta inesperada: %v", body)
	}
	if b := body["baseline"].(map[string]any); b["mean"].(float64) < 0.1 || b["stddev"].(float64) <= 0 {
		t.Fatalf("baseline inesperada: %v", b)
	}
	if body["explanation"] == nil || body["recommendation"] == nil || body["score"].(float64) <= 0 {
		t.Fatalf("explicação/recomendação/score ausentes: %v", body)
	}
	// /predict não gera evento.
	if evs, _ := f.store.ListEvents(context.Background(), store.Filter{}); len(evs) != 0 {
		t.Fatalf("/predict não deveria persistir evento: %+v", evs)
	}
}

func TestPredictNormalAndErrors(t *testing.T) {
	f := setup(t)
	_, body := f.do(t, "POST", "/predict", `{"metric":"node_disk_io_time_seconds_total","value":0.12}`, true)
	if body["anomaly"] != false || body["explanation"] != nil {
		t.Fatalf("valor normal não deveria ser anômalo: %v", body)
	}
	cases := []struct {
		body string
		code int
	}{
		{`{"metric":"node_disk_io_time_seconds_total"}`, 400},
		{`{"metric":"x","value":1,"extra":true}`, 400},
		{`not json`, 400},
		{`{"metric":"unknown_metric","value":1}`, 404},
		{`{"driver":"nope","metric":"node_disk_io_time_seconds_total","value":1}`, 404},
		{`{"driver":"ai-off","metric":"node_load1","value":1}`, 409},
		{`{"metric":"node_disk_io_time_seconds_total","host":"other","value":1}`, 409},
	}
	for _, c := range cases {
		if resp, body := f.do(t, "POST", "/predict", c.body, true); resp.StatusCode != c.code {
			t.Errorf("%s: status %d, esperado %d (%v)", c.body, resp.StatusCode, c.code, body)
		}
	}
}

func TestEventsLifecycle(t *testing.T) {
	f := setup(t)
	now := time.Now().UTC()
	err := f.store.InsertEvent(context.Background(), store.Event{
		ID: "ev-1", Timestamp: now, Host: "node-1", Driver: "ai-disk-health", DriverVersion: "0.1.0",
		Mode: "observe_only", Metric: "node_disk_io_time_seconds_total", Value: 0.41, Severity: "high",
		Explanation: "e", ProbableCause: "c", Recommendation: "r", Status: store.StatusOpen, UpdatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}

	_, list := f.do(t, "GET", "/events?severity=high&limit=10", "", true)
	if list["count"].(float64) != 1 {
		t.Fatalf("listagem inesperada: %v", list)
	}
	if resp, _ := f.do(t, "GET", "/events?limit=0", "", true); resp.StatusCode != 400 {
		t.Errorf("limit inválido deveria retornar 400")
	}

	resp, ev := f.do(t, "GET", "/events/ev-1", "", true)
	if resp.StatusCode != 200 || ev["recommendation"] != "r" || ev["evidence"] == nil {
		t.Fatalf("detalhe inesperado: %d %v", resp.StatusCode, ev)
	}
	if resp, _ := f.do(t, "GET", "/events/missing", "", true); resp.StatusCode != 404 {
		t.Errorf("evento inexistente: %d, esperado 404", resp.StatusCode)
	}

	resp, upd := f.do(t, "PATCH", "/events/ev-1", `{"status":"false_positive","feedback":"backup noturno"}`, true)
	if resp.StatusCode != 200 || upd["status"] != "false_positive" || upd["feedback"] != "backup noturno" {
		t.Fatalf("atualização inesperada: %d %v", resp.StatusCode, upd)
	}
	// Repetir não conta o falso positivo duas vezes.
	f.do(t, "PATCH", "/events/ev-1", `{"status":"false_positive"}`, true)
	if got := testutil.ToFloat64(f.metrics.FalsePositives.WithLabelValues("ai-disk-health", "node_disk_io_time_seconds_total")); got != 1 {
		t.Errorf("false_positives_total=%v, esperado 1", got)
	}
	if resp, _ := f.do(t, "PATCH", "/events/ev-1", `{"status":"bogus"}`, true); resp.StatusCode != 400 {
		t.Errorf("status inválido deveria retornar 400")
	}
}
