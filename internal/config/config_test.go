package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const minimal = `
collector:
  targets:
    - host: local
      url: http://localhost:9100/metrics
drivers:
  - name: ai-disk-health
    metrics:
      - node_disk_io_time_seconds_total
      - name: node_cpu_seconds_total
        exclude: {mode: "idle|iowait"}
`

func TestParseAppliesDefaults(t *testing.T) {
	cfg, err := Parse([]byte(minimal))
	if err != nil {
		t.Fatal(err)
	}
	d := cfg.Drivers[0]
	if cfg.Collector.Interval != 15*time.Second || cfg.API.Listen != ":8088" || cfg.API.AuthTokenEnv != "MICRO_AI_TOKEN" {
		t.Errorf("defaults globais incorretos: %+v %+v", cfg.Collector, cfg.API)
	}
	if d.Mode != ModeObserveOnly || d.AnomalyThreshold != 3 || d.ConsecutivePoints != 3 ||
		d.Cooldown != 5*time.Minute || len(d.WindowSizes) != 3 || !d.IsEnabled() || d.BaselineFreeze != 10 {
		t.Errorf("defaults do driver incorretos: %+v", d)
	}
	if d.Metrics[0].Name != "node_disk_io_time_seconds_total" || d.Metrics[1].Exclude["mode"] != "idle|iowait" {
		t.Errorf("métricas (forma curta e completa) incorretas: %+v", d.Metrics)
	}
}

func TestExampleConfigIsValid(t *testing.T) {
	if _, err := Load("../../configs/config.yaml"); err != nil {
		t.Fatalf("configs/config.yaml inválido: %v", err)
	}
}

func TestValidationErrors(t *testing.T) {
	cases := map[string]string{
		"modo":       strings.Replace(minimal, "name: ai-disk-health", "name: ai-disk-health\n    mode: act", 1),
		"janela":     strings.Replace(minimal, "name: ai-disk-health", "name: ai-disk-health\n    decision_window: 2m", 1),
		"regex":      strings.Replace(minimal, `"idle|iowait"`, `"(idle"`, 1),
		"url":        strings.Replace(minimal, "http://localhost:9100/metrics", "localhost:9100", 1),
		"campo":      minimal + "unknown_field: 1\n",
		"sem driver": "collector:\n  targets:\n    - host: a\n      url: http://a:9100/metrics\n",
	}
	for name, in := range cases {
		if _, err := Parse([]byte(in)); err == nil {
			t.Errorf("%s: esperado erro de validação", name)
		}
	}
}

func TestDriverCanBeDisabled(t *testing.T) {
	cfg, err := Parse([]byte(strings.Replace(minimal, "name: ai-disk-health", "name: ai-disk-health\n    enabled: false", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Drivers[0].IsEnabled() {
		t.Fatal("driver deveria estar desabilitado")
	}
}

func TestLoadDriversDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(minimal), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "drivers.d"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"nginx.yaml", "apache.yaml", "docker.yaml"} {
		data, err := os.ReadFile(filepath.Join("..", "..", "deploy", "drivers", f))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "drivers.d", f), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := Load(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, d := range cfg.Drivers {
		names = append(names, d.Name+"/"+d.Job())
	}
	want := []string{"ai-disk-health/node", "ai-apache-health/ai-apache-health", "ai-docker-health/ai-docker-health", "ai-nginx-health/ai-nginx-health"}
	if !slices.Equal(names, want) {
		t.Fatalf("drivers carregados: %v, esperado %v", names, want)
	}
	if r := cfg.Drivers[3].Metrics[0].Rule; r == nil || *r.Equals != 1 || r.Severity != "critical" {
		t.Fatalf("regra do nginx_up incorreta: %+v", r)
	}

	// Erros em drivers.d citam o arquivo.
	bad := "drivers:\n  - name: ai-disk-health\n    metrics: [x]\n"
	if err := os.WriteFile(filepath.Join(dir, "drivers.d", "zz.yaml"), []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(filepath.Join(dir, "config.yaml")); err == nil || !strings.Contains(err.Error(), "zz.yaml") {
		t.Fatalf("esperado erro de nome duplicado citando zz.yaml, obtido %v", err)
	}
}

func TestMetricValidation(t *testing.T) {
	base := "collector:\n  targets: [{host: h, url: \"http://h:9100/metrics\"}]\ndrivers:\n  - name: d\n    metrics:\n"
	cases := map[string]string{
		"derivada sem alias":   "      - {name: a, subtract: {name: b}}\n",
		"subtract e divide_by": "      - {name: a, alias: x, subtract: {name: b}, divide_by: {name: c}}\n",
		"regra com dois ops":   "      - {name: a, rule: {above: 1, below: 0}}\n",
		"regra sem op":         "      - {name: a, rule: {severity: high}}\n",
		"severidade inválida":  "      - {name: a, rule: {above: 1, severity: urgente}}\n",
		"unidade inválida":     "      - {name: a, describe: {unit: furlongs}}\n",
		"chave duplicada":      "      - a\n      - a\n",
		"min_stddev negativo":  "      - {name: a, min_stddev: -1}\n",
	}
	for name, m := range cases {
		if _, err := Parse([]byte(base + m)); err == nil {
			t.Errorf("%s: esperado erro de validação", name)
		}
	}
	if _, err := Parse([]byte(strings.Replace(base, "name: d", "name: node", 1) + "      - a\n")); err == nil {
		t.Error("nome de driver 'node' deveria ser reservado")
	}
}
