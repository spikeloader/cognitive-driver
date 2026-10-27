// Package collector coleta métricas de um endpoint no formato de exposição
// de texto do Prometheus (ex.: node_exporter).
package collector

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Series é uma amostra de uma série temporal.
type Series struct {
	Name   string
	Labels map[string]string
	Value  float64
}

// Snapshot é o resultado de uma coleta de um host.
type Snapshot struct {
	Host      string
	Job       string
	Timestamp time.Time
	Series    []Series
}

// Collector é a fonte de dados de um host. Novas fontes (arquivos de log,
// outros exporters) implementam esta interface.
type Collector interface {
	Host() string
	Job() string
	Collect(ctx context.Context) (Snapshot, error)
}

// HTTPScraper coleta um endpoint /metrics via HTTP.
type HTTPScraper struct {
	host   string
	job    string
	url    string
	client *http.Client
	keep   map[string]bool
	now    func() time.Time
}

// NewHTTPScraper cria um scraper que mantém apenas as métricas em names.
// job identifica o conjunto de alvos ("node" ou o nome do driver dono).
func NewHTTPScraper(host, job, url string, timeout time.Duration, names []string) *HTTPScraper {
	keep := make(map[string]bool, len(names))
	for _, n := range names {
		keep[n] = true
	}
	return &HTTPScraper{
		host:   host,
		job:    job,
		url:    url,
		client: &http.Client{Timeout: timeout},
		keep:   keep,
		now:    time.Now,
	}
}

func (s *HTTPScraper) Host() string { return s.host }
func (s *HTTPScraper) Job() string  { return s.job }

func (s *HTTPScraper) Collect(ctx context.Context) (Snapshot, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url, nil)
	if err != nil {
		return Snapshot{}, err
	}
	req.Header.Set("Accept", "text/plain;version=0.0.4")
	resp, err := s.client.Do(req)
	if err != nil {
		return Snapshot{}, fmt.Errorf("coletando %s: %w", s.url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Snapshot{}, fmt.Errorf("coletando %s: status %d", s.url, resp.StatusCode)
	}
	ts := s.now()
	series, err := ParseText(resp.Body, func(name string) bool { return s.keep[name] })
	if err != nil {
		return Snapshot{}, fmt.Errorf("interpretando %s: %w", s.url, err)
	}
	return Snapshot{Host: s.host, Job: s.job, Timestamp: ts, Series: series}, nil
}

// ParseText interpreta o formato de texto do Prometheus. Comentários,
// linhas vazias e métricas rejeitadas por keep são ignorados.
func ParseText(r io.Reader, keep func(name string) bool) ([]Series, error) {
	var out []Series
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' {
			continue
		}
		end := strings.IndexAny(line, "{ \t")
		if end <= 0 {
			return nil, fmt.Errorf("linha %d: amostra sem valor", lineNo)
		}
		name := line[:end]
		if keep != nil && !keep(name) {
			continue
		}
		rest := line[end:]
		labels := map[string]string{}
		if rest[0] == '{' {
			var err error
			labels, rest, err = parseLabels(rest[1:])
			if err != nil {
				return nil, fmt.Errorf("linha %d: %w", lineNo, err)
			}
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			return nil, fmt.Errorf("linha %d: amostra sem valor", lineNo)
		}
		v, err := strconv.ParseFloat(fields[0], 64)
		if err != nil {
			return nil, fmt.Errorf("linha %d: valor inválido %q", lineNo, fields[0])
		}
		out = append(out, Series{Name: name, Labels: labels, Value: v})
	}
	return out, sc.Err()
}

// parseLabels consome `a="x",b="y"}` e retorna o restante da linha.
func parseLabels(s string) (map[string]string, string, error) {
	labels := map[string]string{}
	for {
		s = strings.TrimLeft(s, " \t,")
		if s == "" {
			return nil, "", fmt.Errorf("labels não terminados")
		}
		if s[0] == '}' {
			return labels, s[1:], nil
		}
		eq := strings.IndexByte(s, '=')
		if eq <= 0 {
			return nil, "", fmt.Errorf("label malformado")
		}
		key := strings.TrimSpace(s[:eq])
		s = strings.TrimLeft(s[eq+1:], " \t")
		if s == "" || s[0] != '"' {
			return nil, "", fmt.Errorf("valor do label %q sem aspas", key)
		}
		var b strings.Builder
		i := 1
		for ; i < len(s); i++ {
			c := s[i]
			if c == '\\' && i+1 < len(s) {
				i++
				switch s[i] {
				case 'n':
					b.WriteByte('\n')
				default:
					b.WriteByte(s[i])
				}
				continue
			}
			if c == '"' {
				break
			}
			b.WriteByte(c)
		}
		if i >= len(s) {
			return nil, "", fmt.Errorf("valor do label %q não terminado", key)
		}
		labels[key] = b.String()
		s = s[i+1:]
	}
}

// TargetStatus é o estado da última coleta de um alvo (host + job).
type TargetStatus struct {
	Host        string    `json:"host"`
	Job         string    `json:"job"`
	URL         string    `json:"url"`
	OK          bool      `json:"ok"`
	LastAttempt time.Time `json:"last_attempt,omitzero"`
	LastSuccess time.Time `json:"last_success,omitzero"`
	LastError   string    `json:"last_error,omitempty"`
}

// StatusTracker guarda o estado das coletas para o /health.
type StatusTracker struct {
	mu      sync.Mutex
	targets map[string]*TargetStatus
	order   []string
}

func NewStatusTracker() *StatusTracker {
	return &StatusTracker{targets: map[string]*TargetStatus{}}
}

func statusKey(host, job string) string { return host + "|" + job }

// Register adiciona um alvo (na ordem de exibição) antes da primeira coleta.
func (t *StatusTracker) Register(host, job, url string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	k := statusKey(host, job)
	if _, ok := t.targets[k]; ok {
		return
	}
	t.targets[k] = &TargetStatus{Host: host, Job: job, URL: url}
	t.order = append(t.order, k)
}

func (t *StatusTracker) Record(host, job string, at time.Time, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	k := statusKey(host, job)
	s, ok := t.targets[k]
	if !ok {
		s = &TargetStatus{Host: host, Job: job}
		t.targets[k] = s
		t.order = append(t.order, k)
	}
	s.LastAttempt = at
	s.OK = err == nil
	if err != nil {
		s.LastError = err.Error()
		return
	}
	s.LastSuccess = at
	s.LastError = ""
}

func (t *StatusTracker) Snapshot() []TargetStatus {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]TargetStatus, 0, len(t.order))
	for _, k := range t.order {
		out = append(out, *t.targets[k])
	}
	return out
}
