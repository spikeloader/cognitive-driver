// Package runner executa o ciclo observe → detect → explain → recommend
// para cada coleta e persiste os eventos.
package runner

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/spikeloader/cognitive-driver/internal/anomaly"
	"github.com/spikeloader/cognitive-driver/internal/attribution"
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
	// Attributor aponta o processo responsável; nil desativa.
	Attributor *attribution.Attributor
	Log        *slog.Logger
	Now        func() time.Time
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

// responsible extrai o nome do processo responsável gravado no evento.
func responsible(ev store.Event) string {
	var res attribution.Result
	if len(ev.Attribution) == 0 || json.Unmarshal(ev.Attribution, &res) != nil {
		return ""
	}
	if p := res.Responsible(); p != nil {
		return p.Group
	}
	return ""
}

// O resumo em português guarda a janela e as coletas seguidas, que não
// têm coluna própria: "…baseline de 5m (média …), por 3 coletas consecutivas."
var (
	reWindow = regexp.MustCompile(`baseline de (\S+) \(`)
	reStreak = regexp.MustCompile(`por (\d+) coletas consecutivas`)
)

// BackfillTranslations gera as traduções dos eventos gravados antes delas,
// a partir dos números do evento. Eventos de drivers ou métricas que não
// estão mais carregados ficam como estão. Retorna quantos foram traduzidos.
func (r *Runner) BackfillTranslations(ctx context.Context) (int, error) {
	evs, err := r.Store.Untranslated(ctx)
	if err != nil {
		return 0, err
	}
	byName := map[string]translator{}
	for _, d := range r.Drivers {
		if t, ok := d.(translator); ok {
			byName[d.Info().Name] = t
		}
	}
	n := 0
	for _, e := range evs {
		t, ok := byName[e.Driver]
		if !ok {
			continue
		}
		res := anomaly.Result{
			Detector: e.Detector, Rule: e.Rule, Value: e.Value, Timestamp: e.Timestamp,
			Mean: e.BaselineMean, StdDev: e.BaselineStddev, ZScore: e.ZScore, Score: e.Score,
			Severity: anomaly.Severity(e.Severity), Anomalous: true,
		}
		if m := reWindow.FindStringSubmatch(e.Explanation); m != nil {
			res.Window = m[1]
		}
		if m := reStreak.FindStringSubmatch(e.Explanation); m != nil {
			res.Streak, _ = strconv.Atoi(m[1])
		}
		tr := t.Translate(driver.Anomaly{Driver: e.Driver, Host: e.Host, Metric: e.Metric, Result: res})
		if len(tr) == 0 {
			continue
		}
		js, _ := json.Marshal(tr)
		if err := r.Store.SetTranslations(ctx, e.ID, js); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// translator gera os textos do evento em outros idiomas.
type translator interface {
	Translate(a driver.Anomaly) map[string]driver.Localized
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
	if t, ok := d.(translator); ok {
		ev.Translations, _ = json.Marshal(t.Translate(a))
	}

	if r.Attributor != nil && a.Attribute != "" {
		res, err := r.Attributor.Attribute(ctx, a.Host, a.Attribute)
		if err != nil {
			r.Log.Info("attribution_unavailable", "host", a.Host, "metric", a.Metric, "error", err)
		} else if p := res.Responsible(); p != nil {
			ev.Attribution, _ = json.Marshal(res)
		}
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
		"event_id", ev.ID, "created", created, "occurrences", ev.Occurrences, "responsible", responsible(ev),
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
