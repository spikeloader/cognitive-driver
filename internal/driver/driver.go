// Package driver define o contrato estável de uma micro-IA ("cognitive
// driver") e a implementação estatística do MVP.
package driver

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/spikeloader/cognitive-driver/internal/anomaly"
	"github.com/spikeloader/cognitive-driver/internal/collector"
	"github.com/spikeloader/cognitive-driver/internal/config"
	"github.com/spikeloader/cognitive-driver/internal/explain"
	"github.com/spikeloader/cognitive-driver/internal/feature"
	"github.com/spikeloader/cognitive-driver/internal/policy"
)

// ContractVersion versiona a interface Driver. Mudanças incompatíveis
// exigem uma nova versão; campos novos em Info/Anomaly são compatíveis.
const ContractVersion = "v1"

var (
	ErrUnknownMetric = errors.New("métrica não monitorada por este driver")
	ErrNoBaseline    = errors.New("ainda não há baseline para este host e métrica")
)

// MetricInfo descreve uma feature produzida pelo driver.
type MetricInfo struct {
	Name     string `json:"name"`
	Label    string `json:"label"`
	Unit     string `json:"unit"`
	Detector string `json:"detector"`
	// Attribute é o recurso (cpu, memory, io) usado para apontar o
	// processo responsável pelos eventos da métrica.
	Attribute string `json:"attribute,omitempty"`
	// Labels traz o rótulo em outros idiomas ({"en": "CPU usage"}).
	Labels map[string]string `json:"labels,omitempty"`
}

// Localized são os textos de um evento em um idioma.
type Localized struct {
	MetricLabel    string `json:"metric_label"`
	Explanation    string `json:"explanation"`
	ProbableCause  string `json:"probable_cause"`
	Recommendation string `json:"recommendation"`
}

// Info descreve um driver para o /health.
type Info struct {
	Name     string   `json:"name"`
	Version  string   `json:"version"`
	Contract string   `json:"contract"`
	Mode     string   `json:"mode"`
	Enabled  bool     `json:"enabled"`
	Job      string   `json:"job"` // conjunto de alvos observado ("node" = collector.targets)
	Metrics  []string `json:"metrics"`
	// MetricDetails traz rótulo, unidade e tipo de detector de cada métrica.
	MetricDetails []MetricInfo `json:"metric_details"`
}

// Anomaly é o resultado de Predict para uma feature.
type Anomaly struct {
	Driver      string `json:"driver"`
	Host        string `json:"host"`
	Metric      string `json:"metric"`
	MetricLabel string `json:"metric_label"`
	Unit        string `json:"unit"`
	Attribute   string `json:"attribute,omitempty"`
	anomaly.Result
}

// Driver é o contrato lógico de uma micro-IA:
//
//	observe(event) -> features
//	predict(features) -> anomaly
//	explain(anomaly) -> explanation
//	recommend(anomaly) -> action
//	act(action) -> resultado   (bloqueado no MVP)
//
// A implementação interna (estatística, TinyML, LLM local) pode mudar sem
// afetar quem consome a interface.
type Driver interface {
	Info() Info
	Observe(snap collector.Snapshot) []feature.Feature
	// Predict avalia a feature e atualiza a baseline (aprendizado online).
	Predict(f feature.Feature) Anomaly
	// Evaluate avalia a feature contra a baseline atual sem alterá-la.
	Evaluate(f feature.Feature) (Anomaly, error)
	Explain(a Anomaly) explain.Explanation
	Recommend(a Anomaly) policy.Action
	Act(ctx context.Context, a policy.Action) error
}

// detector é a parte comum dos detectores estatístico e por regra.
type detector interface {
	Evaluate(x float64, ts time.Time) anomaly.Result
	Observe(x float64, ts time.Time) anomaly.Result
	State() anomaly.State
	Restore(anomaly.State) bool
	CancelEmit()
}

// defaultAttribute é o recurso atribuído às métricas do host quando o YAML
// não define attribute (configurações instaladas antes da atribuição).
var defaultAttribute = map[string]string{
	"node_cpu_seconds_total":          "cpu",
	"node_memory_MemAvailable_bytes":  "memory",
	"node_disk_io_time_seconds_total": "io",
}

func attributeFor(m config.Metric) string {
	switch m.Attribute {
	case "none":
		return ""
	case "":
		return defaultAttribute[m.Key()]
	}
	return m.Attribute
}

// metricSpec guarda o que o driver sabe sobre cada feature.
type metricSpec struct {
	info     MetricInfo
	override *explain.Override
	newDet   func() detector
}

// Statistical é o driver do MVP: EWMA + z-score, regras fixas e catálogo
// de explicações.
type Statistical struct {
	cfg      config.Driver
	features *feature.Engine
	metrics  map[string]*metricSpec
	order    []string

	mu        sync.Mutex
	detectors map[key]detector
}

type key struct{ host, metric string }

func NewStatistical(cfg config.Driver, interval time.Duration) *Statistical {
	s := &Statistical{
		cfg:       cfg,
		features:  feature.NewEngine(cfg.Metrics),
		metrics:   map[string]*metricSpec{},
		detectors: map[key]detector{},
	}
	base := anomaly.Params{
		Interval:       interval,
		Windows:        cfg.WindowSizes,
		DecisionWindow: cfg.DecisionWindow,
		Threshold:      cfg.AnomalyThreshold,
		Consecutive:    cfg.ConsecutivePoints,
		Cooldown:       cfg.Cooldown,
		Warmup:         cfg.WarmupPoints,
		MinStddevRatio: cfg.MinStddevRatio,
		FreezeLimit:    cfg.BaselineFreeze,
	}
	for _, m := range cfg.Metrics {
		spec := &metricSpec{override: toOverride(m.Describe)}
		label, unit := explain.Describe(m.Key(), spec.override)
		spec.info = MetricInfo{Name: m.Key(), Label: label, Unit: unit, Detector: anomaly.DetectorStatistical, Attribute: attributeFor(m),
			Labels: map[string]string{}}
		for _, lang := range explain.Translations {
			spec.info.Labels[lang] = explain.DescribeIn(lang, m.Key(), spec.override)
		}
		if r := m.Rule; r != nil {
			rule := toRule(r)
			spec.info.Detector = anomaly.DetectorRule
			spec.newDet = func() detector {
				return anomaly.NewRuleDetector(rule, cfg.ConsecutivePoints, cfg.Cooldown)
			}
		} else {
			p := base
			p.MinStddev = m.MinStddev
			spec.newDet = func() detector { return anomaly.NewDetector(p) }
		}
		s.metrics[m.Key()] = spec
		s.order = append(s.order, m.Key())
	}
	return s
}

func toRule(r *config.Rule) anomaly.Rule {
	out := anomaly.Rule{Severity: anomaly.Severity(r.Severity)}
	switch {
	case r.Above != nil:
		out.Op, out.Value = "above", *r.Above
	case r.Below != nil:
		out.Op, out.Value = "below", *r.Below
	default:
		out.Op, out.Value = "equals", *r.Equals
	}
	return out
}

func toOverride(d *config.Describe) *explain.Override {
	if d == nil {
		return nil
	}
	o := &explain.Override{
		Label: d.Label,
		Unit:  d.Unit,
		Up:    explain.Guidance{Cause: d.Up.Cause, Recommendation: d.Up.Recommendation},
		Down:  explain.Guidance{Cause: d.Down.Cause, Recommendation: d.Down.Recommendation},
	}
	if t := d.EN; t != nil {
		o.EN = &explain.Translation{
			Label: t.Label,
			Up:    explain.Guidance{Cause: t.Up.Cause, Recommendation: t.Up.Recommendation},
			Down:  explain.Guidance{Cause: t.Down.Cause, Recommendation: t.Down.Recommendation},
		}
	}
	return o
}

func (s *Statistical) Info() Info {
	info := Info{
		Name:     s.cfg.Name,
		Version:  s.cfg.Version,
		Contract: ContractVersion,
		Mode:     s.cfg.Mode,
		Enabled:  s.cfg.IsEnabled(),
		Job:      s.cfg.Job(),
		Metrics:  s.order,
	}
	for _, name := range s.order {
		info.MetricDetails = append(info.MetricDetails, s.metrics[name].info)
	}
	return info
}

// SourceMetrics lista as métricas brutas que o coletor precisa trazer.
func (s *Statistical) SourceMetrics() []string { return s.features.SourceMetrics() }

func (s *Statistical) Observe(snap collector.Snapshot) []feature.Feature {
	return s.features.Compute(snap)
}

func (s *Statistical) Predict(f feature.Feature) Anomaly {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := key{f.Host, f.Metric}
	d, ok := s.detectors[k]
	if !ok {
		d = s.spec(f.Metric).newDet()
		s.detectors[k] = d
	}
	return s.wrap(f, d.Observe(f.Value, f.Timestamp))
}

// CancelEmit desfaz a última emissão do par host/métrica quando o evento
// não pôde ser gravado. Não faz parte do contrato Driver: o runner usa se
// o driver oferecer.
func (s *Statistical) CancelEmit(host, metric string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d, ok := s.detectors[key{host, metric}]; ok {
		d.CancelEmit()
	}
}

// Evaluate não exige histórico para regras; métricas estatísticas precisam
// de um detector já criado pela coleta.
func (s *Statistical) Evaluate(f feature.Feature) (Anomaly, error) {
	spec := s.spec(f.Metric)
	if spec == nil {
		return Anomaly{}, ErrUnknownMetric
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.detectors[key{f.Host, f.Metric}]
	if !ok {
		if spec.info.Detector != anomaly.DetectorRule {
			return Anomaly{}, ErrNoBaseline
		}
		d = spec.newDet()
	}
	return s.wrap(f, d.Evaluate(f.Value, f.Timestamp)), nil
}

// spec aceita o nome da feature ou o de uma série agrupada (`m{job="x"}`).
func (s *Statistical) spec(metric string) *metricSpec {
	base, _ := feature.SplitSeries(metric)
	return s.metrics[base]
}

func (s *Statistical) wrap(f feature.Feature, r anomaly.Result) Anomaly {
	info := s.spec(f.Metric).info
	label := info.Label
	if _, g := feature.SplitSeries(f.Metric); g != "" {
		label += " · " + feature.GroupValues(g)
	}
	return Anomaly{Driver: s.cfg.Name, Host: f.Host, Metric: f.Metric, MetricLabel: label, Unit: info.Unit,
		Attribute: info.Attribute, Result: r}
}

func (s *Statistical) Explain(a Anomaly) explain.Explanation {
	base, _ := feature.SplitSeries(a.Metric)
	return explain.Explain(base, a.Result, s.override(a))
}

func (s *Statistical) Recommend(a Anomaly) policy.Action {
	base, _ := feature.SplitSeries(a.Metric)
	return policy.Action{Type: "recommendation", Description: explain.Recommend(base, a.Result, s.override(a))}
}

// Translate gera rótulo, explicação, causa e recomendação nos idiomas de
// explain.Translations. Não faz parte do contrato Driver: o runner usa se o
// driver oferecer.
func (s *Statistical) Translate(a Anomaly) map[string]Localized {
	spec := s.spec(a.Metric)
	if spec == nil {
		return nil
	}
	base, group := feature.SplitSeries(a.Metric)
	out := map[string]Localized{}
	for _, lang := range explain.Translations {
		label := spec.info.Labels[lang]
		if group != "" {
			label += " · " + feature.GroupValues(group)
		}
		var o *explain.Override
		if spec.override != nil {
			c := *spec.override
			o = &c
		}
		if group != "" { // a explicação cita o job/pool, como em português
			if o == nil {
				o = &explain.Override{}
			}
			en := explain.Translation{}
			if o.EN != nil {
				en = *o.EN
			}
			en.Label = label
			o.EN = &en
		}
		ex := explain.ExplainIn(lang, base, a.Result, o)
		out[lang] = Localized{
			MetricLabel:    label,
			Explanation:    ex.Summary,
			ProbableCause:  ex.ProbableCause,
			Recommendation: explain.RecommendIn(lang, base, a.Result, o),
		}
	}
	return out
}

// override devolve o describe da métrica; em séries agrupadas o rótulo
// inclui o grupo, para a explicação dizer qual job ou pool foi afetado.
func (s *Statistical) override(a Anomaly) *explain.Override {
	spec := s.spec(a.Metric)
	if spec == nil {
		return nil
	}
	if _, g := feature.SplitSeries(a.Metric); g == "" || a.MetricLabel == "" {
		return spec.override
	}
	o := explain.Override{}
	if spec.override != nil {
		o = *spec.override
	}
	o.Label = a.MetricLabel
	return &o
}

func (s *Statistical) Act(_ context.Context, a policy.Action) error {
	return policy.Authorize(s.cfg.Mode, a)
}

// BaselineState é o estado persistível de um par host/métrica.
type BaselineState struct {
	Host   string
	Metric string
	State  anomaly.State
}

func (s *Statistical) Baselines() []BaselineState {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]BaselineState, 0, len(s.detectors))
	for k, d := range s.detectors {
		out = append(out, BaselineState{Host: k.host, Metric: k.metric, State: d.State()})
	}
	return out
}

// RestoreBaselines carrega estados salvos e retorna quantos foram aceitos.
func (s *Statistical) RestoreBaselines(states []BaselineState) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, st := range states {
		spec := s.spec(st.Metric)
		if spec == nil {
			continue
		}
		d := spec.newDet()
		if d.Restore(st.State) {
			s.detectors[key{st.Host, st.Metric}] = d
			n++
		}
	}
	return n
}
