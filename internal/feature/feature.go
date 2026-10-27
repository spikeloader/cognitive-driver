// Package feature transforma amostras brutas em features por host e
// métrica: contadores viram taxa por segundo, timestamps viram idade, as
// séries filtradas são somadas (por grupo de labels, com group_by) e
// métricas derivadas combinam duas seleções (diferença ou razão).
package feature

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/spikeloader/cognitive-driver/internal/collector"
	"github.com/spikeloader/cognitive-driver/internal/config"
)

// Feature é o valor observado de uma métrica de um host em um instante.
// Com group_by, Metric identifica a série: `cron_job_failed{job="backup"}`.
type Feature struct {
	Host      string    `json:"host"`
	Metric    string    `json:"metric"`
	Value     float64   `json:"value"`
	Timestamp time.Time `json:"timestamp"`
	Series    int       `json:"series"`
}

// SeriesID compõe o nome de uma feature agrupada a partir da chave da
// métrica e dos labels do grupo (já formatados por groupKey).
func SeriesID(key, group string) string {
	if group == "" {
		return key
	}
	return key + "{" + group + "}"
}

// SplitSeries separa a chave da métrica e o grupo de um SeriesID.
func SplitSeries(id string) (key, group string) {
	if i := strings.IndexByte(id, '{'); i >= 0 && strings.HasSuffix(id, "}") {
		return id[:i], id[i+1 : len(id)-1]
	}
	return id, ""
}

// GroupValues retorna só os valores do grupo (`job="a",pool="b"` -> "a / b").
func GroupValues(group string) string {
	var vals []string
	for rest := group; rest != ""; {
		eq := strings.IndexByte(rest, '=')
		if eq < 0 {
			break
		}
		q, err := strconv.QuotedPrefix(rest[eq+1:])
		if err != nil {
			break
		}
		v, _ := strconv.Unquote(q)
		vals = append(vals, v)
		rest = strings.TrimPrefix(rest[eq+1+len(q):], ",")
	}
	return strings.Join(vals, " / ")
}

type operand struct {
	name      string
	counter   bool
	timestamp bool // valor é um Unix timestamp; a feature é a idade em segundos
	match     map[string]*regexp.Regexp
	exclude   map[string]*regexp.Regexp
}

func newOperand(o config.Operand) operand {
	return operand{
		name:      o.Name,
		counter:   o.Type == "counter" || (o.Type == "" && strings.HasSuffix(o.Name, "_total")),
		timestamp: o.Type == "timestamp",
		match:     compile(o.Match),
		exclude:   compile(o.Exclude),
	}
}

func (o operand) accepts(labels map[string]string) bool {
	for k, re := range o.match {
		if !re.MatchString(labels[k]) {
			return false
		}
	}
	for k, re := range o.exclude {
		if re.MatchString(labels[k]) {
			return false
		}
	}
	return true
}

type spec struct {
	key     string
	groupBy []string
	primary operand
	second  *operand
	op      string // "", "subtract", "divide"
}

type point struct {
	value float64
	ts    time.Time
}

// Engine guarda o último valor de cada série de contador para calcular taxas.
type Engine struct {
	specs    []spec
	counters map[string]bool // métricas tratadas como contador
	mu       sync.Mutex
	prev     map[string]point
}

// NewEngine compila as especificações. A configuração já foi validada,
// então as regex são compiladas com MustCompile.
func NewEngine(metrics []config.Metric) *Engine {
	e := &Engine{prev: map[string]point{}, counters: map[string]bool{}}
	for _, m := range metrics {
		s := spec{key: m.Key(), groupBy: m.GroupBy, primary: newOperand(m.Operand)}
		switch {
		case m.Subtract != nil:
			o := newOperand(*m.Subtract)
			s.second, s.op = &o, "subtract"
		case m.DivideBy != nil:
			o := newOperand(*m.DivideBy)
			s.second, s.op = &o, "divide"
		}
		for _, o := range s.operands() {
			if o.counter {
				e.counters[o.name] = true
			}
		}
		e.specs = append(e.specs, s)
	}
	return e
}

func (s spec) operands() []operand {
	if s.second != nil {
		return []operand{s.primary, *s.second}
	}
	return []operand{s.primary}
}

func compile(m map[string]string) map[string]*regexp.Regexp {
	out := make(map[string]*regexp.Regexp, len(m))
	for k, v := range m {
		out[k] = regexp.MustCompile("^(?:" + v + ")$")
	}
	return out
}

// Compute gera uma feature por métrica configurada (uma por grupo de
// labels quando há group_by). As taxas de contador
// são calculadas uma única vez por coleta (uma métrica pode alimentar mais
// de uma feature). Uma feature só é emitida quando todas as séries têm
// valor válido: na primeira coleta, ou após reset de contador ou série
// nova, ela é pulada nesta rodada para não gerar um valor parcial.
func (e *Engine) Compute(snap collector.Snapshot) []Feature {
	e.mu.Lock()
	defer e.mu.Unlock()

	rates := map[string]float64{}
	for _, se := range snap.Series {
		if !e.counters[se.Name] {
			continue
		}
		key := snap.Host + "|" + seriesKey(se)
		prev, ok := e.prev[key]
		e.prev[key] = point{value: se.Value, ts: snap.Timestamp}
		dt := snap.Timestamp.Sub(prev.ts).Seconds()
		if ok && dt > 0 && se.Value >= prev.value {
			rates[key] = (se.Value - prev.value) / dt
		}
	}

	var out []Feature
	for _, s := range e.specs {
		primary := e.sum(snap, s.primary, s.groupBy, rates)
		var second map[string]*acc
		if s.second != nil {
			second = e.sum(snap, *s.second, s.groupBy, rates)
		}
		groups := make([]string, 0, len(primary))
		for g := range primary {
			groups = append(groups, g)
		}
		sort.Strings(groups)
		for _, g := range groups {
			a := primary[g]
			if !a.ok {
				continue
			}
			v, n := a.v, a.n
			if s.second != nil {
				b, found := second[g]
				if !found || !b.ok {
					continue
				}
				n += b.n
				switch s.op {
				case "subtract":
					v -= b.v
				case "divide":
					if b.v == 0 {
						continue
					}
					v /= b.v
				}
			}
			out = append(out, Feature{Host: snap.Host, Metric: SeriesID(s.key, g), Value: v, Timestamp: snap.Timestamp, Series: n})
		}
	}
	return out
}

type acc struct {
	v  float64
	n  int
	ok bool
}

// sum soma as séries aceitas pelo operando, por grupo (um único grupo ""
// sem group_by). Um grupo fica ok=false se algum contador ainda não tem
// taxa; séries sem algum label do group_by são ignoradas.
func (e *Engine) sum(snap collector.Snapshot, o operand, groupBy []string, rates map[string]float64) map[string]*acc {
	out := map[string]*acc{}
	for _, se := range snap.Series {
		if se.Name != o.name || !o.accepts(se.Labels) {
			continue
		}
		g, ok := groupKey(se.Labels, groupBy)
		if !ok {
			continue
		}
		a := out[g]
		if a == nil {
			a = &acc{ok: true}
			out[g] = a
		}
		a.n++
		switch {
		case o.counter:
			r, ok := rates[snap.Host+"|"+seriesKey(se)]
			if !ok {
				a.ok = false
			}
			a.v += r
		case o.timestamp:
			a.v += snap.Timestamp.Sub(time.Unix(0, int64(se.Value*1e9))).Seconds()
		default:
			a.v += se.Value
		}
	}
	return out
}

// groupKey formata os labels do grupo na ordem do group_by.
func groupKey(labels map[string]string, groupBy []string) (string, bool) {
	parts := make([]string, 0, len(groupBy))
	for _, k := range groupBy {
		v, ok := labels[k]
		if !ok || v == "" {
			return "", false
		}
		parts = append(parts, fmt.Sprintf("%s=%q", k, v))
	}
	return strings.Join(parts, ","), true
}

// Metrics retorna os nomes das features produzidas.
func (e *Engine) Metrics() []string {
	out := make([]string, len(e.specs))
	for i, s := range e.specs {
		out[i] = s.key
	}
	return out
}

// SourceMetrics retorna os nomes das métricas brutas necessárias na coleta.
func (e *Engine) SourceMetrics() []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range e.specs {
		for _, o := range s.operands() {
			if !seen[o.name] {
				seen[o.name] = true
				out = append(out, o.name)
			}
		}
	}
	return out
}

func seriesKey(s collector.Series) string {
	keys := make([]string, 0, len(s.Labels))
	for k := range s.Labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(s.Name)
	for _, k := range keys {
		b.WriteString("|" + k + "=" + s.Labels[k])
	}
	return b.String()
}
