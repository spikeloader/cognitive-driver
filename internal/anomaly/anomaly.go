// Package anomaly implementa a baseline adaptativa (EWMA + variância
// exponencial), o z-score e as regras de detecção do MVP.
package anomaly

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// EWMA mantém média e variância com decaimento exponencial.
type EWMA struct {
	Alpha float64 `json:"alpha"`
	Mean  float64 `json:"mean"`
	Var   float64 `json:"var"`
	N     int     `json:"n"`
}

// AlphaFor converte uma janela de tempo em fator de suavização, de forma
// que observações mais antigas que a janela pesem ~1/e.
func AlphaFor(interval, window time.Duration) float64 {
	return 1 - math.Exp(-float64(interval)/float64(window))
}

// Update incorpora uma observação (algoritmo incremental de West).
func (b *EWMA) Update(x float64) {
	if b.N == 0 {
		b.Mean, b.Var = x, 0
	} else {
		diff := x - b.Mean
		incr := b.Alpha * diff
		b.Mean += incr
		b.Var = (1 - b.Alpha) * (b.Var + diff*incr)
	}
	b.N++
}

func (b EWMA) StdDev() float64 { return math.Sqrt(b.Var) }

// ZScore retorna quantos desvios-padrão x está da média; 0 se sd <= 0.
func ZScore(x, mean, sd float64) float64 {
	if sd <= 0 {
		return 0
	}
	return (x - mean) / sd
}

type Severity string

const (
	SeverityNone     Severity = ""
	SeverityLow      Severity = "low"
	SeverityMedium   Severity = "medium"
	SeverityHigh     Severity = "high"
	SeverityCritical Severity = "critical"
)

// Classify aplica as faixas da especificação: low [limiar,4), medium [4,6),
// high [6,10) e critical >= 10 desvios-padrão.
func Classify(absZ, threshold float64) Severity {
	switch {
	case absZ < threshold:
		return SeverityNone
	case absZ < 4:
		return SeverityLow
	case absZ < 6:
		return SeverityMedium
	case absZ < 10:
		return SeverityHigh
	default:
		return SeverityCritical
	}
}

// Score mapeia |z| para [0,1): 3σ ≈ 0.78, 4σ ≈ 0.86, 6σ ≈ 0.95.
func Score(absZ float64) float64 {
	return math.Round((1-math.Exp(-absZ/2))*1000) / 1000
}

type Params struct {
	Interval       time.Duration
	Windows        []time.Duration
	DecisionWindow time.Duration
	Threshold      float64
	Consecutive    int
	Cooldown       time.Duration
	Warmup         int
	MinStddevRatio float64
	// MinStddev é um piso absoluto do desvio-padrão (ex.: contagens pequenas).
	MinStddev float64
	// FreezeLimit é quantos pontos anômalos seguidos ficam fora da
	// baseline; a partir daí o novo patamar passa a ser aprendido.
	FreezeLimit int
}

// WindowStat é a evidência numérica de uma janela.
type WindowStat struct {
	Window string  `json:"window"`
	Mean   float64 `json:"mean"`
	StdDev float64 `json:"stddev"`
	ZScore float64 `json:"zscore"`
}

// Tipos de detector.
const (
	DetectorStatistical = "statistical"
	DetectorRule        = "rule"
)

// Result é a avaliação de uma observação.
type Result struct {
	Detector  string    `json:"detector"`
	Rule      string    `json:"rule,omitempty"` // ex.: "equals 1"; só para regras
	Value     float64   `json:"observed"`
	Timestamp time.Time `json:"timestamp"`
	Learning  bool      `json:"learning"`
	Mean      float64   `json:"baseline_mean"`
	StdDev    float64   `json:"baseline_stddev"`
	ZScore    float64   `json:"zscore"`
	Anomalous bool      `json:"anomaly"`
	Severity  Severity  `json:"severity,omitempty"`
	Score     float64   `json:"score"`
	Streak    int       `json:"streak"`
	Emit      bool      `json:"-"`
	// BaselineFrozen indica que o ponto não foi incorporado à baseline.
	BaselineFrozen bool         `json:"-"`
	Window         string       `json:"window"`
	Windows        []WindowStat `json:"windows"`
}

// Detector mantém o estado de uma métrica de um host.
type Detector struct {
	p        Params
	decision int
	b        []EWMA
	streak   int
	lastEmit time.Time
	prevEmit time.Time // lastEmit anterior à última emissão, para CancelEmit
}

func NewDetector(p Params) *Detector {
	d := &Detector{p: p, b: make([]EWMA, len(p.Windows))}
	for i, w := range p.Windows {
		d.b[i].Alpha = AlphaFor(p.Interval, w)
		if w == p.DecisionWindow {
			d.decision = i
		}
	}
	return d
}

// Evaluate avalia x contra a baseline atual sem alterar o estado.
func (d *Detector) Evaluate(x float64, ts time.Time) Result {
	r := Result{
		Detector:  DetectorStatistical,
		Value:     x,
		Timestamp: ts,
		Window:    FormatWindow(d.p.DecisionWindow),
		Windows:   make([]WindowStat, len(d.b)),
	}
	for i, b := range d.b {
		sd := d.stddev(b)
		r.Windows[i] = WindowStat{
			Window: FormatWindow(d.p.Windows[i]),
			Mean:   b.Mean,
			StdDev: sd,
			ZScore: ZScore(x, b.Mean, sd),
		}
	}
	dec := r.Windows[d.decision]
	r.Mean, r.StdDev, r.ZScore = dec.Mean, dec.StdDev, dec.ZScore
	r.Learning = d.b[d.decision].N < d.p.Warmup
	if r.Learning {
		return r
	}
	absZ := math.Abs(r.ZScore)
	r.Severity = Classify(absZ, d.p.Threshold)
	r.Anomalous = r.Severity != SeverityNone
	r.Score = Score(absZ)
	if r.Anomalous {
		r.Streak = d.streak + 1
	}
	return r
}

// Observe avalia x, decide se um evento deve ser emitido (pontos
// consecutivos + cooldown) e só então incorpora x à baseline. Pontos
// anômalos não entram na baseline até a sequência passar de FreezeLimit,
// para que um pico não infle o desvio-padrão e mascare a própria detecção.
func (d *Detector) Observe(x float64, ts time.Time) Result {
	r := d.Evaluate(x, ts)
	d.streak = r.Streak
	if r.Anomalous && d.streak >= d.p.Consecutive &&
		(d.lastEmit.IsZero() || ts.Sub(d.lastEmit) >= d.p.Cooldown) {
		r.Emit = true
		d.prevEmit, d.lastEmit = d.lastEmit, ts
	}
	r.BaselineFrozen = r.Anomalous && d.streak <= d.p.FreezeLimit
	if r.BaselineFrozen {
		return r
	}
	for i := range d.b {
		d.b[i].Update(x)
	}
	return r
}

// CancelEmit desfaz a última emissão (ex.: o evento não foi gravado), para
// que o cooldown não suprima a próxima tentativa.
func (d *Detector) CancelEmit() { d.lastEmit = d.prevEmit }

// stddev aplica um piso proporcional à média para evitar z-scores
// explosivos em métricas praticamente constantes.
func (d *Detector) stddev(b EWMA) float64 {
	return max(b.StdDev(), math.Abs(b.Mean)*d.p.MinStddevRatio, d.p.MinStddev, 1e-9)
}

// State é o estado persistível do detector.
type State struct {
	Baselines []EWMA    `json:"baselines"`
	LastEmit  time.Time `json:"last_emit,omitzero"`
}

func (d *Detector) State() State {
	return State{Baselines: append([]EWMA(nil), d.b...), LastEmit: d.lastEmit}
}

// Restore carrega um estado salvo; é ignorado (retorna false) se as janelas
// ou o intervalo mudaram desde que foi salvo.
func (d *Detector) Restore(s State) bool {
	if len(s.Baselines) != len(d.b) {
		return false
	}
	for i := range d.b {
		if math.Abs(s.Baselines[i].Alpha-d.b[i].Alpha) > 1e-12 {
			return false
		}
	}
	copy(d.b, s.Baselines)
	d.lastEmit = s.LastEmit
	return true
}

// FormatWindow formata durações como "1m", "15m", "1h30m".
func FormatWindow(w time.Duration) string {
	s := w.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// Rule é uma condição fixa: dispara quando o valor fica acima (above),
// abaixo (below) ou diferente (equals) de Value.
type Rule struct {
	Op       string // above | below | equals
	Value    float64
	Severity Severity
}

// Violated informa se x desrespeita a regra.
func (r Rule) Violated(x float64) bool {
	switch r.Op {
	case "above":
		return x > r.Value
	case "below":
		return x < r.Value
	default:
		return x != r.Value
	}
}

func (r Rule) String() string { return fmt.Sprintf("%s %g", r.Op, r.Value) }

// RuleDetector aplica uma regra com as mesmas exigências de pontos
// consecutivos e cooldown do detector estatístico, mas sem aprendizado.
type RuleDetector struct {
	rule        Rule
	consecutive int
	cooldown    time.Duration
	streak      int
	lastEmit    time.Time
	prevEmit    time.Time
}

func NewRuleDetector(r Rule, consecutive int, cooldown time.Duration) *RuleDetector {
	return &RuleDetector{rule: r, consecutive: consecutive, cooldown: cooldown}
}

// Evaluate avalia x sem alterar o estado.
func (d *RuleDetector) Evaluate(x float64, ts time.Time) Result {
	r := Result{
		Detector:  DetectorRule,
		Rule:      d.rule.String(),
		Value:     x,
		Timestamp: ts,
		Mean:      d.rule.Value,
	}
	if d.rule.Violated(x) {
		r.Anomalous = true
		r.Severity = d.rule.Severity
		r.Score = 1
		r.Streak = d.streak + 1
	}
	return r
}

// Observe avalia x e decide se um evento deve ser emitido.
func (d *RuleDetector) Observe(x float64, ts time.Time) Result {
	r := d.Evaluate(x, ts)
	d.streak = r.Streak
	if r.Anomalous && d.streak >= d.consecutive &&
		(d.lastEmit.IsZero() || ts.Sub(d.lastEmit) >= d.cooldown) {
		r.Emit = true
		d.prevEmit, d.lastEmit = d.lastEmit, ts
	}
	return r
}

// CancelEmit desfaz a última emissão; veja Detector.CancelEmit.
func (d *RuleDetector) CancelEmit() { d.lastEmit = d.prevEmit }

func (d *RuleDetector) State() State { return State{LastEmit: d.lastEmit} }

// Restore só aceita estados sem baseline (gravados por um RuleDetector).
func (d *RuleDetector) Restore(s State) bool {
	if len(s.Baselines) != 0 {
		return false
	}
	d.lastEmit = s.LastEmit
	return true
}
