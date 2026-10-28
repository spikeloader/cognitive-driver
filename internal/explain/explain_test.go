package explain

import (
	"strings"
	"testing"

	"github.com/spikeloader/cognitive-driver/internal/anomaly"
)

func TestExplainDiskSpike(t *testing.T) {
	r := anomaly.Result{Value: 0.41, Mean: 0.12, StdDev: 0.03, ZScore: 9.67, Streak: 3, Window: "5m"}
	ex := Explain("node_disk_io_time_seconds_total", r, nil)
	want := "Utilização de I/O de disco em 41,0%, 9,7 desvios-padrão acima da baseline de 5m (média 12,0%, desvio 3,0%), por 3 coletas consecutivas."
	if ex.Summary != want {
		t.Fatalf("resumo:\n  obtido   %q\n  esperado %q", ex.Summary, want)
	}
	if !strings.Contains(ex.ProbableCause, "I/O") {
		t.Errorf("causa provável inesperada: %q", ex.ProbableCause)
	}
	if !strings.Contains(Recommend("node_disk_io_time_seconds_total", r, nil), "iotop") {
		t.Error("recomendação de disco deveria citar iotop")
	}
}

func TestExplainMemoryDropUsesDownGuidance(t *testing.T) {
	r := anomaly.Result{Value: 512 << 20, Mean: 8 << 30, StdDev: 256 << 20, ZScore: -30, Window: "5m"}
	ex := Explain("node_memory_MemAvailable_bytes", r, nil)
	if !strings.Contains(ex.Summary, "512,0 MiB") || !strings.Contains(ex.Summary, "abaixo") {
		t.Errorf("resumo inesperado: %q", ex.Summary)
	}
	if !strings.Contains(ex.ProbableCause, "Pressão de memória") {
		t.Errorf("causa provável deveria indicar pressão de memória: %q", ex.ProbableCause)
	}
}

func TestExplainUnknownMetric(t *testing.T) {
	ex := Explain("custom_metric", anomaly.Result{Value: 10, Mean: 1, StdDev: 1, ZScore: 9, Window: "5m"}, nil)
	if !strings.HasPrefix(ex.Summary, "Métrica custom_metric em 10,000") || ex.ProbableCause == "" {
		t.Errorf("fallback genérico inesperado: %+v", ex)
	}
}

func TestExplainRules(t *testing.T) {
	down := anomaly.Result{Detector: anomaly.DetectorRule, Rule: "equals 1", Value: 0, Mean: 1, Streak: 3}
	ex := Explain("nginx_up", down, nil)
	if ex.Summary != "Disponibilidade do nginx em inativo (0); o esperado é ativo (1), por 3 coletas consecutivas." {
		t.Errorf("resumo da regra equals: %q", ex.Summary)
	}
	if !strings.Contains(Recommend("nginx_up", down, nil), "nginx -t") {
		t.Error("recomendação do nginx_up deveria citar nginx -t")
	}
	sat := anomaly.Result{Detector: anomaly.DetectorRule, Rule: "above 0.9", Value: 0.97, Mean: 0.9, Streak: 1}
	if s := Explain("apache_workers_busy_ratio", sat, nil).Summary; s != "Ocupação dos workers do Apache em 97,0%, acima do limite de 90,0%." {
		t.Errorf("resumo da regra above: %q", s)
	}
	if !strings.Contains(Explain("apache_workers_busy_ratio", sat, nil).ProbableCause, "esgotados") {
		t.Error("regra above deveria usar a orientação de pico")
	}
}

func TestOverrideFromConfig(t *testing.T) {
	o := &Override{Label: "Fila de jobs", Unit: "count", Up: Guidance{Recommendation: "Escalar os workers da fila."}}
	r := anomaly.Result{Detector: anomaly.DetectorStatistical, Value: 120, Mean: 10, StdDev: 5, ZScore: 22, Window: "5m"}
	ex := Explain("queue_depth", r, o)
	if !strings.HasPrefix(ex.Summary, "Fila de jobs em 120,0, 22,0 desvios-padrão acima") {
		t.Errorf("override de rótulo/unidade não aplicado: %q", ex.Summary)
	}
	if Recommend("queue_depth", r, o) != "Escalar os workers da fila." || ex.ProbableCause == "" {
		t.Error("override de recomendação deveria valer e a causa cair no genérico")
	}
	if label, unit := Describe("nginx_http_requests_total", nil); label != "Requisições HTTP no nginx" || unit != "per_second" {
		t.Errorf("Describe do catálogo: %q %q", label, unit)
	}
}

func TestExplainInEnglish(t *testing.T) {
	r := anomaly.Result{Value: 0.41, Mean: 0.12, StdDev: 0.03, ZScore: 9.67, Streak: 3, Window: "5m"}
	ex := ExplainIn(EN, "node_disk_io_time_seconds_total", r, nil)
	want := "Disk I/O utilization at 41.0%, 9.7 standard deviations above the 5m baseline (mean 12.0%, stddev 3.0%), for 3 consecutive scrapes."
	if ex.Summary != want {
		t.Fatalf("resumo em inglês:\n  obtido   %q\n  esperado %q", ex.Summary, want)
	}
	if !strings.HasPrefix(ex.ProbableCause, "I/O pressure") || !strings.Contains(RecommendIn(EN, "node_disk_io_time_seconds_total", r, nil), "iotop") {
		t.Errorf("causa/recomendação em inglês inesperadas: %q", ex.ProbableCause)
	}
	rule := anomaly.Result{Detector: anomaly.DetectorRule, Rule: "equals 1", Value: 0, Mean: 1}
	if s := ExplainIn(EN, "nginx_up", rule, nil).Summary; s != "nginx availability at inactive (0); expected active (1)." {
		t.Errorf("regra em inglês: %q", s)
	}
	if got := FormatIn(EN, "seconds", 3*86400); got != "3.0 days" {
		t.Errorf("FormatIn(seconds) = %q", got)
	}
}

func TestEnglishFallbacks(t *testing.T) {
	r := anomaly.Result{Value: 10, Mean: 1, StdDev: 1, ZScore: 9, Window: "5m"}
	// Sem catálogo nem describe: rótulo e textos genéricos em inglês.
	ex := ExplainIn(EN, "custom_metric", r, nil)
	if !strings.HasPrefix(ex.Summary, "Metric custom_metric at 10.000") || ex.ProbableCause != genericGuidanceEN.Cause {
		t.Errorf("fallback em inglês: %+v", ex)
	}
	// describe só em português: o texto do YAML vale mais que o genérico.
	pt := &Override{Label: "Fila do worker", Up: Guidance{Cause: "Fila crescendo."}}
	if ex := ExplainIn(EN, "worker_queue", r, pt); !strings.HasPrefix(ex.Summary, "Fila do worker at") || ex.ProbableCause != "Fila crescendo." {
		t.Errorf("describe em português como fallback: %+v", ex)
	}
	// describe.en tem prioridade.
	pt.EN = &Translation{Label: "Worker queue", Up: Guidance{Cause: "Queue growing.", Recommendation: "Scale workers."}}
	if ex := ExplainIn(EN, "worker_queue", r, pt); !strings.HasPrefix(ex.Summary, "Worker queue at") || ex.ProbableCause != "Queue growing." ||
		RecommendIn(EN, "worker_queue", r, pt) != "Scale workers." {
		t.Errorf("describe.en: %+v", ex)
	}
	// Português continua igual.
	if ex := Explain("worker_queue", r, pt); !strings.HasPrefix(ex.Summary, "Fila do worker em 10,000") {
		t.Errorf("português mudou: %q", ex.Summary)
	}
}
