// Package runner executa o ciclo observe → detect → explain → recommend
// para cada coleta e persiste os eventos.
package runner

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/spikeloader/cognitive-driver/internal/collector"
	"github.com/spikeloader/cognitive-driver/internal/driver"
	"github.com/spikeloader/cognitive-driver/internal/feature"
	"github.com/spikeloader/cognitive-driver/internal/policy"
	"github.com/spikeloader/cognitive-driver/internal/store"
	"github.com/spikeloader/cognitive-driver/internal/telemetry"
)

// Persistent é implementado por drivers cuja baseline pode ser salva.
type Persistent interface {
	Baselines() []driver.BaselineState
	RestoreBaselines([]driver.BaselineState) int
}

type Runner struct {
	Interval time.Duration
	Timeout  time.Duration
	Targets  []collector.Collector
	Drivers  []driver.Driver // apenas drivers habilitados
	Store    *store.Store
	Metrics  *telemetry.Metrics
	Status   *collector.StatusTracker
	Log      *slog.Logger
	Now      func() time.Time
}

// Run coleta imediatamente e depois a cada Interval, até ctx terminar.
func (r *Runner) Run(ctx context.Context) {
	t := time.NewTicker(r.Interval)
	defer t.Stop()
	for {
		r.Tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Tick executa uma rodada de coleta em todos os alvos, em paralelo.
func (r *Runner) Tick(ctx context.Context) {
	var wg sync.WaitGroup
	for _, c := range r.Targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.process(ctx, c)
		}()
	}
	wg.Wait()
}

func (r *Runner) process(ctx context.Context, c collector.Collector) {
	host := c.Host()
	start := r.now()
	cctx, cancel := context.WithTimeout(ctx, r.Timeout)
	snap, err := c.Collect(cctx)
	cancel()
	r.Metrics.ScrapeDuration.WithLabelValues(host, c.Job()).Observe(time.Since(start).Seconds())
	r.Status.Record(host, c.Job(), start, err)
	if err != nil {
		r.Metrics.Scrapes.WithLabelValues(host, c.Job(), "error").Inc()
		r.Log.Warn("scrape_failed", "host", host, "job", c.Job(), "error", err)
		return
	}
	r.Metrics.Scrapes.WithLabelValues(host, c.Job(), "success").Inc()

	for _, d := range r.Drivers {
		if d.Info().Job != snap.Job {
			continue
		}
		for _, f := range d.Observe(snap) {
			r.evaluate(ctx, d, f)
		}
	}
}

// emitCanceler é implementado por drivers capazes de desfazer uma emissão.
type emitCanceler interface {
	CancelEmit(host, metric string)
}

func (r *Runner) evaluate(ctx context.Context, d driver.Driver, f feature.Feature) {
	info := d.Info()
	// Séries agrupadas (um job, um pool) usam a métrica base nos contadores
	// Prometheus para não multiplicar a cardinalidade.
	base, _ := feature.SplitSeries(f.Metric)
	t0 := time.Now()
	a := d.Predict(f)
	r.Metrics.InferenceDuration.WithLabelValues(info.Name).Observe(time.Since(t0).Seconds())
	r.Metrics.Predictions.WithLabelValues(info.Name, base).Inc()
	if a.Anomalous {
		r.Metrics.Anomalies.WithLabelValues(info.Name, base).Inc()
	}
	r.Log.Info("prediction",
		"driver", info.Name, "host", f.Host, "metric", f.Metric,
		"observed", f.Value, "baseline_mean", a.Mean, "baseline_stddev", a.StdDev,
		"zscore", a.ZScore, "detector", a.Detector, "learning", a.Learning, "anomaly", a.Anomalous,
		"severity", string(a.Severity), "streak", a.Streak, "emit", a.Emit,
		"baseline_frozen", a.BaselineFrozen)
	if !a.Emit {
		return
	}

	ex := d.Explain(a)
	action := d.Recommend(a)
	evidence, _ := json.Marshal(ex.Evidence)
	now := r.now()
	ev := store.Event{
		ID:             uuid.NewString(),
		Timestamp:      a.Timestamp,
		Host:           a.Host,
		Driver:         info.Name,
		DriverVersion:  info.Version,
		Mode:           info.Mode,
		Metric:         a.Metric,
		MetricLabel:    a.MetricLabel,
		Unit:           a.Unit,
		Detector:       a.Detector,
		Rule:           a.Rule,
		Value:          a.Value,
		BaselineMean:   a.Mean,
		BaselineStddev: a.StdDev,
		ZScore:         a.ZScore,
		Score:          a.Score,
		Severity:       string(a.Severity),
		Explanation:    ex.Summary,
		ProbableCause:  ex.ProbableCause,
		Recommendation: action.Description,
		Evidence:       evidence,
		Status:         store.StatusOpen,
		UpdatedAt:      now,
	}
	// Um evento ativo da mesma métrica recebe a ocorrência em vez de duplicar.
	saved, created, err := r.Store.RecordEvent(ctx, ev)
	if err != nil {
		r.Metrics.StoreErrors.Inc()
		r.Log.Error("event_store_failed", "driver", ev.Driver, "host", ev.Host, "metric", ev.Metric,
			"severity", ev.Severity, "zscore", ev.ZScore, "error", err)
		// Sem isso o cooldown suprimiria a próxima tentativa e o evento se perderia.
		if c, ok := d.(emitCanceler); ok {
			c.CancelEmit(ev.Host, ev.Metric)
		}
		return
	}
	ev = saved
	r.Metrics.Events.WithLabelValues(info.Name, base, ev.Severity).Inc()
	r.Log.Warn("anomaly_event",
		"event_id", ev.ID, "created", created, "occurrences", ev.Occurrences,
		"driver", ev.Driver, "host", ev.Host, "metric", ev.Metric,
		"severity", ev.Severity, "score", ev.Score, "zscore", ev.ZScore,
		"explanation", ev.Explanation, "probable_cause", ev.ProbableCause,
		"recommendation", ev.Recommendation, "mode", ev.Mode)

	// Toda ação passa pela política; no MVP ela sempre é bloqueada.
	if err := d.Act(ctx, action); errors.Is(err, policy.ErrActionBlocked) {
		r.Metrics.ActionsBlocked.WithLabelValues(info.Name).Inc()
		r.Log.Info("action_blocked", "event_id", ev.ID, "driver", info.Name, "reason", err.Error())
	} else if err != nil {
		r.Log.Error("action_failed", "event_id", ev.ID, "driver", info.Name, "error", err)
	}
}

// SaveBaselines persiste o estado de todos os drivers que o suportam.
func (r *Runner) SaveBaselines(ctx context.Context) error {
	var bs []store.Baseline
	for _, d := range r.Drivers {
		p, ok := d.(Persistent)
		if !ok {
			continue
		}
		for _, b := range p.Baselines() {
			state, err := json.Marshal(b.State)
			if err != nil {
				return err
			}
			bs = append(bs, store.Baseline{Driver: d.Info().Name, Host: b.Host, Metric: b.Metric, State: state})
		}
	}
	if err := r.Store.SaveBaselines(ctx, bs, r.now()); err != nil {
		r.Metrics.StoreErrors.Inc()
		return err
	}
	return nil
}

// RestoreBaselines carrega as baselines salvas para evitar novo aprendizado
// após um restart.
func (r *Runner) RestoreBaselines(ctx context.Context) error {
	for _, d := range r.Drivers {
		p, ok := d.(Persistent)
		if !ok {
			continue
		}
		name := d.Info().Name
		saved, err := r.Store.LoadBaselines(ctx, name)
		if err != nil {
			return err
		}
		states := make([]driver.BaselineState, 0, len(saved))
		for _, b := range saved {
			st := driver.BaselineState{Host: b.Host, Metric: b.Metric}
			if err := json.Unmarshal(b.State, &st.State); err != nil {
				r.Log.Warn("baseline_restore_skipped", "driver", name, "host", b.Host, "metric", b.Metric, "error", err)
				continue
			}
			states = append(states, st)
		}
		n := p.RestoreBaselines(states)
		r.Log.Info("baselines_restored", "driver", name, "restored", n, "stored", len(saved))
	}
	return nil
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}
