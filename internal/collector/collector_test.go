package collector

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const sample = `# HELP node_cpu_seconds_total Seconds the CPUs spent in each mode.
# TYPE node_cpu_seconds_total counter
node_cpu_seconds_total{cpu="0",mode="idle"} 1234.5
node_cpu_seconds_total{cpu="0",mode="user"} 56.25
node_memory_MemAvailable_bytes 8.123456e+09
node_disk_io_time_seconds_total{device="sda"} 12.5 1700000000000
weird_metric{path="C:\\dir",msg="say \"hi\", ok",nl="a\nb"} +Inf
node_load1 0.5
`

func TestParseText(t *testing.T) {
	series, err := ParseText(strings.NewReader(sample), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(series) != 6 {
		t.Fatalf("esperado 6 séries, obtido %d", len(series))
	}
	if s := series[1]; s.Name != "node_cpu_seconds_total" || s.Labels["mode"] != "user" || s.Value != 56.25 {
		t.Errorf("série inesperada: %+v", s)
	}
	if s := series[2]; s.Value != 8.123456e9 || len(s.Labels) != 0 {
		t.Errorf("gauge sem labels inesperado: %+v", s)
	}
	if s := series[3]; s.Value != 12.5 {
		t.Errorf("timestamp não deveria afetar o valor: %+v", s)
	}
	w := series[4]
	if w.Labels["path"] != `C:\dir` || w.Labels["msg"] != `say "hi", ok` || w.Labels["nl"] != "a\nb" {
		t.Errorf("escapes de label incorretos: %#v", w.Labels)
	}
}

func TestParseTextFilters(t *testing.T) {
	series, err := ParseText(strings.NewReader(sample), func(n string) bool { return n == "node_load1" })
	if err != nil {
		t.Fatal(err)
	}
	if len(series) != 1 || series[0].Name != "node_load1" {
		t.Fatalf("filtro não aplicado: %+v", series)
	}
}

func TestParseTextErrors(t *testing.T) {
	for _, in := range []string{
		"metric_without_value\n",
		`m{a="1" 3` + "\n",
		`m{a=1} 3` + "\n",
		"m abc\n",
	} {
		if _, err := ParseText(strings.NewReader(in), nil); err == nil {
			t.Errorf("esperado erro para %q", in)
		}
	}
}

func TestHTTPScraper(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(sample))
	}))
	defer srv.Close()

	s := NewHTTPScraper("node-1", "node", srv.URL, time.Second, []string{"node_memory_MemAvailable_bytes"})
	snap, err := s.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snap.Host != "node-1" || snap.Job != "node" || len(snap.Series) != 1 || snap.Timestamp.IsZero() {
		t.Fatalf("snapshot inesperado: %+v", snap)
	}
}

func TestHTTPScraperErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	if _, err := NewHTTPScraper("h", "node", srv.URL, time.Second, nil).Collect(context.Background()); err == nil {
		t.Fatal("esperado erro para status 500")
	}
}

func TestStatusTracker(t *testing.T) {
	tr := NewStatusTracker()
	tr.Register("a", "node", "http://a:9100/metrics")
	tr.Register("a", "ai-nginx-health", "http://a:9113/metrics")
	now := time.Now()
	tr.Record("a", "node", now, nil)
	tr.Record("a", "node", now.Add(time.Second), errors.New("timeout"))
	if got := tr.Snapshot(); len(got) != 2 || got[1].Job != "ai-nginx-health" || !got[1].LastAttempt.IsZero() {
		t.Fatalf("alvos por host+job incorretos: %+v", got)
	}
	s := tr.Snapshot()[0]
	if s.OK || s.LastError != "timeout" || !s.LastSuccess.Equal(now) {
		t.Fatalf("status inesperado: %+v", s)
	}
}
