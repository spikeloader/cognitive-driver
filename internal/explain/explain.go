// Package explain gera explicações e recomendações a partir de um catálogo
// de métricas (regras + templates), que pode ser complementado pelo YAML.
package explain

import (
	"fmt"
	"math"
	"strings"

	"github.com/spikeloader/cognitive-driver/internal/anomaly"
)

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
// Campos vazios mantêm o valor do catálogo.
type Override struct {
	Label string
	Unit  string
	Up    Guidance
	Down  Guidance
}

type family struct {
	label string
	unit  string
	up    Guidance
	down  Guidance
}

// Format exibe um valor na unidade informada.
func Format(unit string, v float64) string {
	switch unit {
	case "cores":
		return num(v, 2) + " núcleos ocupados"
	case "bytes":
		return bytes(v)
	case "bytes_per_second":
		return bytes(v) + "/s"
	case "ratio":
		return num(v*100, 1) + "%"
	case "per_second":
		return num(v, 2) + "/s"
	case "count":
		return num(v, 1)
	case "milliseconds":
		return num(v, 1) + " ms"
	case "seconds":
		return duration(v)
	case "bool":
		if v >= 0.5 {
			return "ativo (1)"
		}
		return "inativo (0)"
	default:
		return num(v, 3)
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
	merge := func(dst *Guidance, src Guidance) {
		if src.Cause != "" {
			dst.Cause = src.Cause
		}
		if src.Recommendation != "" {
			dst.Recommendation = src.Recommendation
		}
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

// Describe retorna o rótulo e a unidade de exibição de uma métrica.
func Describe(metric string, o *Override) (label, unit string) {
	f := lookup(metric, o)
	return f.label, f.unit
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

// Explain descreve a anomalia com evidência numérica e causa provável.
func Explain(metric string, r anomaly.Result, o *Override) Explanation {
	f := lookup(metric, o)
	value := Format(f.unit, r.Value)
	var summary string
	if r.Detector == anomaly.DetectorRule {
		limit := Format(f.unit, r.Mean)
		switch strings.Fields(r.Rule)[0] {
		case "above":
			summary = fmt.Sprintf("%s em %s, acima do limite de %s", f.label, value, limit)
		case "below":
			summary = fmt.Sprintf("%s em %s, abaixo do mínimo de %s", f.label, value, limit)
		default:
			summary = fmt.Sprintf("%s em %s; o esperado é %s", f.label, value, limit)
		}
	} else {
		dir := "acima"
		if r.ZScore < 0 {
			dir = "abaixo"
		}
		summary = fmt.Sprintf("%s em %s, %s desvios-padrão %s da baseline de %s (média %s, desvio %s)",
			f.label, value, num(math.Abs(r.ZScore), 1), dir, r.Window,
			Format(f.unit, r.Mean), Format(f.unit, r.StdDev))
	}
	if r.Streak > 1 {
		summary += fmt.Sprintf(", por %d coletas consecutivas", r.Streak)
	}
	return Explanation{
		Summary:       summary + ".",
		ProbableCause: pick(f, r).Cause,
		Evidence:      r.Windows,
	}
}

// Recommend retorna a ação sugerida para um operador humano.
func Recommend(metric string, r anomaly.Result, o *Override) string {
	return pick(lookup(metric, o), r).Recommendation
}

// duration exibe segundos na maior unidade legível: "45 s", "12,5 min", "3,2 h", "2,0 dias".
func duration(v float64) string {
	switch a := math.Abs(v); {
	case a < 60:
		return num(v, 0) + " s"
	case a < 3600:
		return num(v/60, 1) + " min"
	case a < 86400:
		return num(v/3600, 1) + " h"
	default:
		return num(v/86400, 1) + " dias"
	}
}

// num formata com vírgula decimal (pt-BR).
func num(v float64, prec int) string {
	return strings.Replace(fmt.Sprintf("%.*f", prec, v), ".", ",", 1)
}

func bytes(v float64) string {
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	i := 0
	for math.Abs(v) >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	return num(v, 1) + " " + units[i]
}
