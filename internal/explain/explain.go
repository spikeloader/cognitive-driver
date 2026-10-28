// Package explain gera explicações e recomendações a partir de um catálogo
// de métricas (regras + templates), que pode ser complementado pelo YAML.
package explain

import (
	"fmt"
	"math"
	"strings"

	"github.com/spikeloader/cognitive-driver/internal/anomaly"
)

// Idiomas dos textos gerados. PT é o padrão (campos principais do evento);
// os demais são gravados como traduções.
const (
	PT = "pt-BR"
	EN = "en"
)

// Translations lista os idiomas gerados além de PT.
var Translations = []string{EN}

// Explanation é a explicação estruturada de uma anomalia.
type Explanation struct {
	Summary       string               `json:"summary"`
	ProbableCause string               `json:"probable_cause"`
	Evidence      []anomaly.WindowStat `json:"evidence"`
}

type Guidance struct {
	Cause          string
	Recommendation string
}

// Override personaliza a descrição de uma métrica (describe no YAML).
// Campos vazios mantêm o valor do catálogo. EN traz os textos em inglês.
type Override struct {
	Label string
	Unit  string
	Up    Guidance
	Down  Guidance
	EN    *Translation
}

// Translation são os textos de uma métrica em outro idioma.
type Translation struct {
	Label string
	Up    Guidance
	Down  Guidance
}

type family struct {
	label string
	unit  string
	up    Guidance
	down  Guidance
}

// text é uma entrada de catálogo traduzida (a unidade vem de catalog).
type text struct {
	label string
	up    Guidance
	down  Guidance
}

// Format exibe um valor na unidade informada, em português.
func Format(unit string, v float64) string { return FormatIn(PT, unit, v) }

// FormatIn exibe um valor na unidade informada, no idioma lang.
func FormatIn(lang, unit string, v float64) string {
	en := lang == EN
	switch unit {
	case "cores":
		if en {
			return num(lang, v, 2) + " busy cores"
		}
		return num(lang, v, 2) + " núcleos ocupados"
	case "bytes":
		return bytes(lang, v)
	case "bytes_per_second":
		return bytes(lang, v) + "/s"
	case "ratio":
		return num(lang, v*100, 1) + "%"
	case "per_second":
		return num(lang, v, 2) + "/s"
	case "count":
		return num(lang, v, 1)
	case "milliseconds":
		return num(lang, v, 1) + " ms"
	case "seconds":
		return duration(lang, v)
	case "bool":
		switch {
		case v >= 0.5 && en:
			return "active (1)"
		case v >= 0.5:
			return "ativo (1)"
		case en:
			return "inactive (0)"
		}
		return "inativo (0)"
	default:
		return num(lang, v, 3)
	}
}

func merge(dst *Guidance, src Guidance) {
	if src.Cause != "" {
		dst.Cause = src.Cause
	}
	if src.Recommendation != "" {
		dst.Recommendation = src.Recommendation
	}
}

func (f family) override(o *Override) family {
	if o == nil {
		return f
	}
	if o.Label != "" {
		f.label = o.Label
	}
	if o.Unit != "" {
		f.unit = o.Unit
	}
	merge(&f.up, o.Up)
	merge(&f.down, o.Down)
	return f
}

var genericGuidance = Guidance{
	Cause:          "Valor fora do comportamento habitual da métrica.",
	Recommendation: "Correlacionar com deploys, mudanças de configuração e outras métricas do host no mesmo período.",
}

func lookup(metric string, o *Override) family {
	f, ok := catalog[metric]
	if !ok {
		f = family{label: "Métrica " + metric, up: genericGuidance, down: genericGuidance}
	}
	return f.override(o)
}

// lookupIn resolve os textos no idioma lang. Em inglês, a prioridade é:
// describe.en do YAML, catálogo em inglês, texto do YAML em português e,
// por fim, o texto genérico.
func lookupIn(lang, metric string, o *Override) family {
	f := lookup(metric, o)
	if lang != EN {
		return f
	}
	if t, ok := catalogEN[metric]; ok {
		f.label, f.up, f.down = t.label, t.up, t.down
	} else {
		if o == nil || o.Label == "" {
			f.label = "Metric " + metric
		}
		fill := func(g *Guidance, pt Guidance) {
			if pt.Cause == "" || pt.Cause == genericGuidance.Cause {
				g.Cause = genericGuidanceEN.Cause
			}
			if pt.Recommendation == "" || pt.Recommendation == genericGuidance.Recommendation {
				g.Recommendation = genericGuidanceEN.Recommendation
			}
		}
		fill(&f.up, f.up)
		fill(&f.down, f.down)
	}
	if o != nil && o.EN != nil {
		if o.EN.Label != "" {
			f.label = o.EN.Label
		}
		merge(&f.up, o.EN.Up)
		merge(&f.down, o.EN.Down)
	}
	return f
}

// Describe retorna o rótulo e a unidade de exibição de uma métrica.
func Describe(metric string, o *Override) (label, unit string) {
	f := lookup(metric, o)
	return f.label, f.unit
}

// DescribeIn retorna o rótulo da métrica no idioma lang.
func DescribeIn(lang, metric string, o *Override) string {
	return lookupIn(lang, metric, o).label
}

// up informa se a anomalia é para cima (pico) ou para baixo (queda).
func up(r anomaly.Result) bool {
	if r.Detector == anomaly.DetectorRule {
		switch strings.Fields(r.Rule)[0] {
		case "above":
			return true
		case "below":
			return false
		}
		return r.Value > r.Mean
	}
	return r.ZScore >= 0
}

func pick(f family, r anomaly.Result) Guidance {
	if up(r) {
		return f.up
	}
	return f.down
}

// phrases são as frases da explicação por idioma.
var phrases = map[string]struct {
	above, below, equals, stat, up, down, streak string
}{
	PT: {
		above:  "%s em %s, acima do limite de %s",
		below:  "%s em %s, abaixo do mínimo de %s",
		equals: "%s em %s; o esperado é %s",
		stat:   "%s em %s, %s desvios-padrão %s da baseline de %s (média %s, desvio %s)",
		up:     "acima", down: "abaixo",
		streak: ", por %d coletas consecutivas",
	},
	EN: {
		above:  "%s at %s, above the limit of %s",
		below:  "%s at %s, below the minimum of %s",
		equals: "%s at %s; expected %s",
		stat:   "%s at %s, %s standard deviations %s the %s baseline (mean %s, stddev %s)",
		up:     "above", down: "below",
		streak: ", for %d consecutive scrapes",
	},
}

// Explain descreve a anomalia com evidência numérica e causa provável.
func Explain(metric string, r anomaly.Result, o *Override) Explanation {
	return ExplainIn(PT, metric, r, o)
}

// ExplainIn descreve a anomalia no idioma lang.
func ExplainIn(lang, metric string, r anomaly.Result, o *Override) Explanation {
	ph, ok := phrases[lang]
	if !ok {
		lang, ph = PT, phrases[PT]
	}
	f := lookupIn(lang, metric, o)
	value := FormatIn(lang, f.unit, r.Value)
	var summary string
	if r.Detector == anomaly.DetectorRule {
		limit := FormatIn(lang, f.unit, r.Mean)
		switch strings.Fields(r.Rule)[0] {
		case "above":
			summary = fmt.Sprintf(ph.above, f.label, value, limit)
		case "below":
			summary = fmt.Sprintf(ph.below, f.label, value, limit)
		default:
			summary = fmt.Sprintf(ph.equals, f.label, value, limit)
		}
	} else {
		dir := ph.up
		if r.ZScore < 0 {
			dir = ph.down
		}
		summary = fmt.Sprintf(ph.stat, f.label, value, num(lang, math.Abs(r.ZScore), 1), dir, r.Window,
			FormatIn(lang, f.unit, r.Mean), FormatIn(lang, f.unit, r.StdDev))
	}
	if r.Streak > 1 {
		summary += fmt.Sprintf(ph.streak, r.Streak)
	}
	return Explanation{
		Summary:       summary + ".",
		ProbableCause: pick(f, r).Cause,
		Evidence:      r.Windows,
	}
}

// Recommend retorna a ação sugerida para um operador humano.
func Recommend(metric string, r anomaly.Result, o *Override) string {
	return RecommendIn(PT, metric, r, o)
}

// RecommendIn retorna a ação sugerida no idioma lang.
func RecommendIn(lang, metric string, r anomaly.Result, o *Override) string {
	return pick(lookupIn(lang, metric, o), r).Recommendation
}

// duration exibe segundos na maior unidade legível: "45 s", "12,5 min", "3,2 h", "2,0 dias".
func duration(lang string, v float64) string {
	switch a := math.Abs(v); {
	case a < 60:
		return num(lang, v, 0) + " s"
	case a < 3600:
		return num(lang, v/60, 1) + " min"
	case a < 86400:
		return num(lang, v/3600, 1) + " h"
	case lang == EN:
		return num(lang, v/86400, 1) + " days"
	default:
		return num(lang, v/86400, 1) + " dias"
	}
}

// num formata com vírgula decimal em português e ponto em inglês.
func num(lang string, v float64, prec int) string {
	s := fmt.Sprintf("%.*f", prec, v)
	if lang == EN {
		return s
	}
	return strings.Replace(s, ".", ",", 1)
}

func bytes(lang string, v float64) string {
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	i := 0
	for math.Abs(v) >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	return num(lang, v, 1) + " " + units[i]
}
