// Package telemetry expõe as métricas internas do serviço.
package telemetry

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

const namespace = "cognitive_driver"

type Metrics struct {
	Registry *prometheus.Registry

	Info              *prometheus.GaugeVec
	Predictions       *prometheus.CounterVec
	Anomalies         *prometheus.CounterVec
	Events            *prometheus.CounterVec
	FalsePositives    *prometheus.CounterVec
	ActionsBlocked    *prometheus.CounterVec
	InferenceDuration *prometheus.HistogramVec
	Scrapes           *prometheus.CounterVec
	ScrapeDuration    *prometheus.HistogramVec
	StoreErrors       prometheus.Counter
}

// New cria um registry próprio com as métricas do serviço, do runtime Go
// (inclui uso de memória) e do processo.
func New() *Metrics {
	m := &Metrics{
		Registry: prometheus.NewRegistry(),
		Info: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace, Name: "driver_info",
			Help: "Drivers carregados (valor 1 = habilitado, 0 = desabilitado).",
		}, []string{"driver", "version", "contract", "mode"}),
		Predictions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "predictions_total",
			Help: "Previsões realizadas por driver e métrica.",
		}, []string{"driver", "metric"}),
		Anomalies: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "anomalous_points_total",
			Help: "Pontos acima do limiar (antes do filtro de consecutivos e cooldown).",
		}, []string{"driver", "metric"}),
		Events: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "events_total",
			Help: "Eventos de anomalia gerados.",
		}, []string{"driver", "metric", "severity"}),
		FalsePositives: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "false_positives_total",
			Help: "Eventos marcados como falso positivo via API.",
		}, []string{"driver", "metric"}),
		ActionsBlocked: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "actions_blocked_total",
			Help: "Ações recomendadas e bloqueadas pela política.",
		}, []string{"driver"}),
		InferenceDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace, Name: "inference_duration_seconds",
			Help:    "Latência de inferência (predict) por feature.",
			Buckets: prometheus.ExponentialBuckets(1e-6, 4, 10),
		}, []string{"driver"}),
		Scrapes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "scrapes_total",
			Help: "Coletas realizadas por host, job e resultado.",
		}, []string{"host", "job", "result"}),
		ScrapeDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace, Name: "scrape_duration_seconds",
			Help:    "Duração das coletas.",
			Buckets: prometheus.DefBuckets,
		}, []string{"host", "job"}),
		StoreErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace, Name: "store_errors_total",
			Help: "Falhas ao persistir eventos ou baselines.",
		}),
	}
	m.Registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		m.Info, m.Predictions, m.Anomalies, m.Events, m.FalsePositives, m.ActionsBlocked,
		m.InferenceDuration, m.Scrapes, m.ScrapeDuration, m.StoreErrors,
	)
	return m
}
