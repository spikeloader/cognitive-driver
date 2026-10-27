package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func event(id string, ts time.Time, severity string) Event {
	return Event{
		ID: id, Timestamp: ts, Host: "h", Driver: "ai-disk-health", DriverVersion: "0.1.0",
		Mode: "observe_only", Metric: "node_disk_io_time_seconds_total", Value: 0.41,
		BaselineMean: 0.12, BaselineStddev: 0.03, ZScore: 9.7, Score: 0.99, Severity: severity,
		Explanation: "x", ProbableCause: "y", Recommendation: "z",
		Evidence: json.RawMessage(`[{"window":"5m"}]`), Status: StatusOpen, UpdatedAt: ts,
	}
}

func TestInsertGetList(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	t0 := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	for i, sev := range []string{"low", "high", "medium"} {
		e := event(string(rune('a'+i)), t0.Add(time.Duration(i)*time.Minute), sev)
		e.Host = e.ID // um evento ativo por driver/host/métrica
		if err := s.InsertEvent(ctx, e); err != nil {
			t.Fatal(err)
		}
	}

	e, err := s.GetEvent(ctx, "b")
	if err != nil {
		t.Fatal(err)
	}
	if e.Severity != "high" || !e.Timestamp.Equal(t0.Add(time.Minute)) || string(e.Evidence) != `[{"window":"5m"}]` {
		t.Fatalf("evento lido difere: %+v", e)
	}

	all, err := s.ListEvents(ctx, Filter{})
	if err != nil || len(all) != 3 || all[0].ID != "c" {
		t.Fatalf("listagem deveria vir do mais recente: %v %+v", err, all)
	}
	high, _ := s.ListEvents(ctx, Filter{Severity: "high"})
	if len(high) != 1 || high[0].ID != "b" {
		t.Fatalf("filtro por severidade falhou: %+v", high)
	}
	recent, _ := s.ListEvents(ctx, Filter{Since: t0.Add(time.Minute), Limit: 1})
	if len(recent) != 1 || recent[0].ID != "c" {
		t.Fatalf("filtro since/limit falhou: %+v", recent)
	}
	if _, err := s.GetEvent(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("esperado ErrNotFound, obtido %v", err)
	}
}

func TestUpdateEvent(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now()
	if err := s.InsertEvent(ctx, event("a", now, "low")); err != nil {
		t.Fatal(err)
	}
	before, after, err := s.UpdateEvent(ctx, "a", StatusFalsePositive, "backup agendado", now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if before.Status != StatusOpen || after.Status != StatusFalsePositive {
		t.Fatalf("status antes/depois incorretos: %s %s", before.Status, after.Status)
	}
	got, _ := s.GetEvent(ctx, "a")
	if got.Status != StatusFalsePositive || got.Feedback != "backup agendado" {
		t.Fatalf("atualização não persistida: %+v", got)
	}
	if _, _, err := s.UpdateEvent(ctx, "nope", StatusResolved, "", now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("esperado ErrNotFound, obtido %v", err)
	}
}

func TestBaselinesUpsert(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	b := Baseline{Driver: "d", Host: "h", Metric: "m", State: json.RawMessage(`{"v":1}`)}
	if err := s.SaveBaselines(ctx, []Baseline{b}, time.Now()); err != nil {
		t.Fatal(err)
	}
	b.State = json.RawMessage(`{"v":2}`)
	if err := s.SaveBaselines(ctx, []Baseline{b}, time.Now()); err != nil {
		t.Fatal(err)
	}
	got, err := s.LoadBaselines(ctx, "d")
	if err != nil || len(got) != 1 || string(got[0].State) != `{"v":2}` {
		t.Fatalf("upsert falhou: %v %+v", err, got)
	}
}

func TestPersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.InsertEvent(context.Background(), event("a", time.Now(), "low")); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.GetEvent(context.Background(), "a"); err != nil {
		t.Fatalf("evento não persistiu após reabrir: %v", err)
	}
}

// Um banco criado pela primeira versão (sem metric_label/unit/detector/rule)
// é migrado na abertura e mantém os eventos.
func TestRecordEventDeduplicatesActive(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	t0 := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)

	first, created, err := s.RecordEvent(ctx, event("a", t0, "medium"))
	if err != nil || !created || first.Occurrences != 1 {
		t.Fatalf("primeira emissão deveria criar: created=%v err=%v %+v", created, err, first)
	}
	// Reemissão com score maior: atualiza o pico, mantém id/abertura/feedback.
	if _, _, err := s.UpdateEvent(ctx, "a", StatusAcknowledged, "olhando", t0); err != nil {
		t.Fatal(err)
	}
	peak := event("b", t0.Add(5*time.Minute), "critical")
	peak.Score, peak.Value = 0.999, 0.9
	got, created, err := s.RecordEvent(ctx, peak)
	if err != nil || created {
		t.Fatalf("reemissão não deveria criar: created=%v err=%v", created, err)
	}
	if got.ID != "a" || !got.Timestamp.Equal(t0) || !got.LastSeen.Equal(t0.Add(5*time.Minute)) ||
		got.Occurrences != 2 || got.Severity != "critical" || got.Value != 0.9 ||
		got.Status != StatusAcknowledged || got.Feedback != "olhando" {
		t.Fatalf("evento ativo atualizado incorretamente: %+v", got)
	}
	// Reemissão mais fraca: só conta a ocorrência, mantém o pico.
	weak := event("c", t0.Add(10*time.Minute), "low")
	weak.Score = 0.5
	got, _, _ = s.RecordEvent(ctx, weak)
	if got.Occurrences != 3 || got.Severity != "critical" || !got.LastSeen.Equal(t0.Add(10*time.Minute)) {
		t.Fatalf("emissão mais fraca não deveria trocar o pico: %+v", got)
	}
	if all, _ := s.ListEvents(ctx, Filter{}); len(all) != 1 {
		t.Fatalf("esperado 1 evento, obtido %d", len(all))
	}

	// Depois de resolvido, uma nova emissão abre outro evento.
	if _, _, err := s.UpdateEvent(ctx, "a", StatusResolved, "", t0); err != nil {
		t.Fatal(err)
	}
	next, created, err := s.RecordEvent(ctx, event("d", t0.Add(20*time.Minute), "high"))
	if err != nil || !created || next.ID != "d" {
		t.Fatalf("emissão após resolução deveria criar: created=%v err=%v %+v", created, err, next)
	}
	// Reabrir "a" colidiria com "d", que está ativo.
	if _, _, err := s.UpdateEvent(ctx, "a", StatusOpen, "", t0); !errors.Is(err, ErrActiveExists) {
		t.Fatalf("esperado ErrActiveExists, obtido %v", err)
	}
	if _, _, err := s.UpdateEvent(ctx, "d", StatusAcknowledged, "", t0); err != nil {
		t.Fatalf("open -> acknowledged no próprio evento deveria passar: %v", err)
	}
}

func TestMigrationConsolidatesActiveDuplicates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dup.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	row := func(id, ts, status string) string {
		return `('` + id + `','` + ts + `','h','ai-disk-health','0.1.0','observe_only','m',0.4,0.1,0.03,10,0.99,'high','x','y','z','[]','` + status + `','','` + ts + `')`
	}
	_, err = db.Exec(`CREATE TABLE events (
		id TEXT PRIMARY KEY, timestamp TEXT NOT NULL, host TEXT NOT NULL, driver TEXT NOT NULL,
		driver_version TEXT NOT NULL, mode TEXT NOT NULL, metric TEXT NOT NULL, value REAL NOT NULL,
		baseline_mean REAL NOT NULL, baseline_stddev REAL NOT NULL, zscore REAL NOT NULL, score REAL NOT NULL,
		severity TEXT NOT NULL, explanation TEXT NOT NULL, probable_cause TEXT NOT NULL, recommendation TEXT NOT NULL,
		evidence TEXT NOT NULL, status TEXT NOT NULL, feedback TEXT NOT NULL DEFAULT '', updated_at TEXT NOT NULL);
	INSERT INTO events VALUES ` + row("e1", "2026-10-27T12:00:00Z", "open") + `,` +
		row("e2", "2026-10-27T12:05:00Z", "acknowledged") + `,` +
		row("e3", "2026-10-27T12:10:00Z", "open") + `,` +
		row("e0", "2026-10-27T11:00:00Z", "resolved"))
	db.Close()
	if err != nil {
		t.Fatal(err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	keep, _ := s.GetEvent(ctx, "e1")
	if keep.Status != StatusOpen || keep.Occurrences != 3 || keep.LastSeen.Format(time.RFC3339) != "2026-10-27T12:10:00Z" {
		t.Fatalf("evento mantido incorreto: %+v", keep)
	}
	for _, id := range []string{"e2", "e3"} {
		e, _ := s.GetEvent(ctx, id)
		if e.Status != StatusResolved || e.Feedback != "Duplicado: consolidado no evento e1." {
			t.Fatalf("duplicado %s não foi resolvido: %+v", id, e)
		}
	}
	if e, _ := s.GetEvent(ctx, "e0"); e.Status != StatusResolved || e.Feedback != "" || e.Occurrences != 1 {
		t.Fatalf("evento já resolvido não deveria mudar: %+v", e)
	}
}

func TestMigratesOldSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE events (
		id TEXT PRIMARY KEY, timestamp TEXT NOT NULL, host TEXT NOT NULL, driver TEXT NOT NULL,
		driver_version TEXT NOT NULL, mode TEXT NOT NULL, metric TEXT NOT NULL, value REAL NOT NULL,
		baseline_mean REAL NOT NULL, baseline_stddev REAL NOT NULL, zscore REAL NOT NULL, score REAL NOT NULL,
		severity TEXT NOT NULL, explanation TEXT NOT NULL, probable_cause TEXT NOT NULL, recommendation TEXT NOT NULL,
		evidence TEXT NOT NULL, status TEXT NOT NULL, feedback TEXT NOT NULL DEFAULT '', updated_at TEXT NOT NULL);
	INSERT INTO events VALUES ('old-1','2026-10-27T12:00:00Z','h','ai-disk-health','0.1.0','observe_only',
		'node_disk_io_time_seconds_total',0.4,0.1,0.03,10,0.99,'critical','x','y','z','[]','open','','2026-10-27T12:00:00Z');`)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	e, err := s.GetEvent(context.Background(), "old-1")
	if err != nil {
		t.Fatal(err)
	}
	if e.Detector != "statistical" || e.MetricLabel != "" || e.Severity != "critical" {
		t.Fatalf("evento antigo após migração: %+v", e)
	}
	// Reabrir não tenta adicionar as colunas de novo.
	s.Close()
	if s2, err := Open(path); err != nil {
		t.Fatalf("segunda abertura falhou: %v", err)
	} else {
		s2.Close()
	}
}

func TestRecordEventKeepsAttribution(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	t0 := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)

	// Pico sem atribuição (process-exporter indisponível no momento).
	peak := event("a", t0, "critical")
	if _, _, err := s.RecordEvent(ctx, peak); err != nil {
		t.Fatal(err)
	}
	// Emissão mais fraca, mas com atribuição: preenche o que faltava.
	weak := event("b", t0.Add(5*time.Minute), "low")
	weak.Score = 0.5
	weak.Attribution = json.RawMessage(`{"resource":"io","processes":[{"group":"rsync","value":1}]}`)
	got, _, err := s.RecordEvent(ctx, weak)
	if err != nil {
		t.Fatal(err)
	}
	if got.Severity != "critical" || string(got.Attribution) != string(weak.Attribution) {
		t.Fatalf("atribuição deveria ser preenchida mantendo o pico: %+v", got)
	}
	read, _ := s.GetEvent(ctx, "a")
	if string(read.Attribution) != string(weak.Attribution) {
		t.Fatalf("atribuição não persistida: %q", read.Attribution)
	}
	// Evento sem atribuição volta como nulo (omitido no JSON).
	if _, _, err := s.UpdateEvent(ctx, "a", StatusResolved, "", t0); err != nil {
		t.Fatal(err)
	}
	other := event("c", t0, "low")
	other.Metric = "outra"
	if err := s.InsertEvent(ctx, other); err != nil {
		t.Fatal(err)
	}
	if e, _ := s.GetEvent(ctx, "c"); e.Attribution != nil {
		t.Fatalf("sem atribuição deveria ser nil: %q", e.Attribution)
	}
}
