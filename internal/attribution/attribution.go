// Package attribution identifica os processos responsáveis por um evento:
// no momento da emissão, lê o process-exporter do host em duas amostras
// curtas e ordena os grupos de processos pelo consumo do recurso afetado.
package attribution

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/spikeloader/cognitive-driver/internal/collector"
)

// Recursos atribuíveis (campo attribute das métricas no YAML).
const (
	CPU    = "cpu"
	Memory = "memory"
	IO     = "io"
)

// Valid informa se r é um recurso atribuível.
func Valid(r string) bool { return r == CPU || r == Memory || r == IO }

// Métricas do process-exporter (ncabatoff/process-exporter, grupos por nome).
const (
	metricCPU    = "namedprocess_namegroup_cpu_seconds_total"
	metricMemory = "namedprocess_namegroup_memory_bytes"
	metricRead   = "namedprocess_namegroup_read_bytes_total"
	metricWrite  = "namedprocess_namegroup_write_bytes_total"
	metricProcs  = "namedprocess_namegroup_num_procs"
)

// Process é um grupo de processos (mesmo nome) e o quanto consome.
type Process struct {
	Group string  `json:"group"`
	Value float64 `json:"value"`
	Share float64 `json:"share"` // fração do total do recurso entre os grupos
	Procs int     `json:"procs"`
}

// Result é gravado no evento: o primeiro processo é o responsável.
type Result struct {
	Resource  string    `json:"resource"`
	Unit      string    `json:"unit"` // mesma convenção do describe.unit
	Source    string    `json:"source"`
	At        time.Time `json:"at"`
	Processes []Process `json:"processes"`
}

// Responsible retorna o grupo que mais consome, ou nil.
func (r *Result) Responsible() *Process {
	if r == nil || len(r.Processes) == 0 {
		return nil
	}
	return &r.Processes[0]
}

// Attributor consulta o process-exporter de cada host.
type Attributor struct {
	urls    map[string]string // host -> URL do process-exporter
	gap     time.Duration     // intervalo entre as duas amostras (taxas)
	timeout time.Duration
	top     int

	mu    sync.Mutex
	cache map[string]cached // host|recurso: vários eventos na mesma coleta
}

type cached struct {
	res *Result
	at  time.Time
}

// cacheTTL evita repetir as amostras quando várias métricas do mesmo host
// disparam na mesma rodada de coleta.
const cacheTTL = 10 * time.Second

// New cria o Attributor para os hosts em urls.
func New(urls map[string]string, gap, timeout time.Duration, top int) *Attributor {
	return &Attributor{urls: urls, gap: gap, timeout: timeout, top: top, cache: map[string]cached{}}
}

// ProcessURL deriva a URL do process-exporter da URL do node_exporter do
// host, trocando a porta e o caminho.
func ProcessURL(nodeURL string, port int) (string, error) {
	u, err := url.Parse(nodeURL)
	if err != nil {
		return "", err
	}
	u.Host = net.JoinHostPort(u.Hostname(), strconv.Itoa(port))
	u.Path, u.RawQuery = "/metrics", ""
	return u.String(), nil
}

// Attribute retorna os grupos que mais consomem resource em host.
func (a *Attributor) Attribute(ctx context.Context, host, resource string) (*Result, error) {
	u, ok := a.urls[host]
	if !ok {
		return nil, fmt.Errorf("sem process-exporter configurado para o host %q", host)
	}
	key := host + "|" + resource
	a.mu.Lock()
	if c, ok := a.cache[key]; ok && time.Since(c.at) < cacheTTL {
		a.mu.Unlock()
		return c.res, nil
	}
	a.mu.Unlock()

	scraper := collector.NewHTTPScraper(host, "processes", u, a.timeout,
		[]string{metricCPU, metricMemory, metricRead, metricWrite, metricProcs})
	first, err := scraper.Collect(ctx)
	if err != nil {
		return nil, err
	}
	second := first
	if resource != Memory { // memória é instantânea; CPU e I/O são taxas
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(a.gap):
		}
		if second, err = scraper.Collect(ctx); err != nil {
			return nil, err
		}
	}
	res := compute(resource, first, second, a.top)
	res.Source = "process-exporter"

	a.mu.Lock()
	a.cache[key] = cached{res: res, at: time.Now()}
	a.mu.Unlock()
	return res, nil
}

// compute ordena os grupos pelo consumo: CPU em núcleos e I/O em bytes/s
// (diferença entre as amostras), memória residente em bytes (2ª amostra).
func compute(resource string, first, second collector.Snapshot, top int) *Result {
	value := func(s collector.Snapshot) map[string]float64 {
		out := map[string]float64{}
		for _, se := range s.Series {
			g := se.Labels["groupname"]
			switch {
			case resource == CPU && se.Name == metricCPU,
				resource == IO && (se.Name == metricRead || se.Name == metricWrite),
				resource == Memory && se.Name == metricMemory && se.Labels["memtype"] == "resident":
				out[g] += se.Value
			}
		}
		return out
	}
	procs := map[string]int{}
	for _, se := range second.Series {
		if se.Name == metricProcs {
			procs[se.Labels["groupname"]] = int(se.Value)
		}
	}

	res := &Result{Resource: resource, At: second.Timestamp}
	cur := value(second)
	switch resource {
	case CPU, IO:
		res.Unit = "cores"
		if resource == IO {
			res.Unit = "bytes_per_second"
		}
		prev := value(first)
		dt := second.Timestamp.Sub(first.Timestamp).Seconds()
		for g, v := range cur {
			p, ok := prev[g]
			if !ok || dt <= 0 || v < p { // grupo novo ou contador reiniciado
				delete(cur, g)
				continue
			}
			cur[g] = (v - p) / dt
		}
	case Memory:
		res.Unit = "bytes"
	}

	var total float64
	for g, v := range cur {
		if v <= 0 {
			delete(cur, g)
			continue
		}
		total += v
	}
	for g, v := range cur {
		res.Processes = append(res.Processes, Process{Group: g, Value: v, Share: v / total, Procs: procs[g]})
	}
	sort.Slice(res.Processes, func(i, j int) bool {
		if res.Processes[i].Value != res.Processes[j].Value {
			return res.Processes[i].Value > res.Processes[j].Value
		}
		return res.Processes[i].Group < res.Processes[j].Group
	})
	if len(res.Processes) > top {
		res.Processes = res.Processes[:top]
	}
	return res
}
