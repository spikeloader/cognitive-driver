package attribution

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// exporter simula o process-exporter: a cada scrape, os contadores de CPU
// e I/O avançam pela taxa de cada grupo.
type exporter struct {
	scrapes atomic.Int64
	start   time.Time
}

type group struct {
	name         string
	cpu, io, rss float64 // núcleos, bytes/s, bytes
	procs        int
}

var groups = []group{
	{"stress-ng", 1.9, 0, 50 << 20, 4},
	{"postgres", 0.3, 8 << 20, 900 << 20, 12},
	{"sshd", 0, 0, 5 << 20, 1},
}

func (e *exporter) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	e.scrapes.Add(1)
	t := time.Since(e.start).Seconds() + 100 // contadores já em andamento
	var b strings.Builder
	for _, g := range groups {
		l := fmt.Sprintf(`groupname=%q`, g.name)
		fmt.Fprintf(&b, "namedprocess_namegroup_cpu_seconds_total{%s,mode=\"user\"} %g\n", l, g.cpu*t*0.75)
		fmt.Fprintf(&b, "namedprocess_namegroup_cpu_seconds_total{%s,mode=\"system\"} %g\n", l, g.cpu*t*0.25)
		fmt.Fprintf(&b, "namedprocess_namegroup_read_bytes_total{%s} %g\n", l, g.io*t/2)
		fmt.Fprintf(&b, "namedprocess_namegroup_write_bytes_total{%s} %g\n", l, g.io*t/2)
		fmt.Fprintf(&b, "namedprocess_namegroup_memory_bytes{%s,memtype=\"resident\"} %g\n", l, g.rss)
		fmt.Fprintf(&b, "namedprocess_namegroup_memory_bytes{%s,memtype=\"virtual\"} %g\n", l, g.rss*10)
		fmt.Fprintf(&b, "namedprocess_namegroup_num_procs{%s} %d\n", l, g.procs)
	}
	fmt.Fprint(w, b.String())
}

func newTest(t *testing.T) (*Attributor, *exporter) {
	t.Helper()
	ex := &exporter{start: time.Now()}
	srv := httptest.NewServer(ex)
	t.Cleanup(srv.Close)
	return New(map[string]string{"node-1": srv.URL + "/metrics"}, 200*time.Millisecond, time.Second, 2), ex
}

func near(a, b, tol float64) bool { return math.Abs(a-b) <= tol }

func TestAttributeCPU(t *testing.T) {
	a, ex := newTest(t)
	res, err := a.Attribute(context.Background(), "node-1", CPU)
	if err != nil {
		t.Fatal(err)
	}
	if ex.scrapes.Load() != 2 {
		t.Errorf("CPU exige 2 amostras, feitas %d", ex.scrapes.Load())
	}
	p := res.Responsible()
	if p == nil || p.Group != "stress-ng" || !near(p.Value, 1.9, 0.1) || p.Procs != 4 || !near(p.Share, 1.9/2.2, 0.03) {
		t.Fatalf("responsável incorreto: %+v", res)
	}
	// top=2 e sshd (0 núcleos) fica de fora.
	if len(res.Processes) != 2 || res.Processes[1].Group != "postgres" || res.Unit != "cores" {
		t.Fatalf("lista incorreta: %+v", res)
	}
}

func TestAttributeIOAndMemory(t *testing.T) {
	a, ex := newTest(t)
	io, err := a.Attribute(context.Background(), "node-1", IO)
	if err != nil {
		t.Fatal(err)
	}
	if p := io.Responsible(); p == nil || p.Group != "postgres" || !near(p.Value, 8<<20, 1<<19) || len(io.Processes) != 1 {
		t.Fatalf("I/O: %+v", io)
	}
	before := ex.scrapes.Load()
	mem, err := a.Attribute(context.Background(), "node-1", Memory)
	if err != nil {
		t.Fatal(err)
	}
	if ex.scrapes.Load()-before != 1 {
		t.Errorf("memória deveria usar 1 amostra")
	}
	if p := mem.Responsible(); p == nil || p.Group != "postgres" || p.Value != 900<<20 || mem.Unit != "bytes" {
		t.Fatalf("memória (residente, não virtual): %+v", mem)
	}
}

func TestAttributeCachesAndErrors(t *testing.T) {
	a, ex := newTest(t)
	ctx := context.Background()
	if _, err := a.Attribute(ctx, "node-1", CPU); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Attribute(ctx, "node-1", CPU); err != nil || ex.scrapes.Load() != 2 {
		t.Fatalf("2ª atribuição na mesma rodada deveria vir do cache: scrapes=%d err=%v", ex.scrapes.Load(), err)
	}
	if _, err := a.Attribute(ctx, "outro", CPU); err == nil {
		t.Fatal("host sem process-exporter deveria falhar")
	}
	down := New(map[string]string{"h": "http://127.0.0.1:1/metrics"}, time.Millisecond, time.Second, 5)
	if _, err := down.Attribute(ctx, "h", CPU); err == nil {
		t.Fatal("exporter fora do ar deveria falhar")
	}
}

func TestProcessURL(t *testing.T) {
	for in, want := range map[string]string{
		"http://localhost:9100/metrics":     "http://localhost:9256/metrics",
		"https://10.0.0.5:9100/x?y=1":       "https://10.0.0.5:9256/metrics",
		"http://[2001:db8::1]:9100/metrics": "http://[2001:db8::1]:9256/metrics",
		"http://web-01.internal/metrics":    "http://web-01.internal:9256/metrics",
	} {
		if got, err := ProcessURL(in, 9256); err != nil || got != want {
			t.Errorf("ProcessURL(%q) = %q, %v; esperado %q", in, got, err, want)
		}
	}
}
