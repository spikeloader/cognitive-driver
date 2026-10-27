// Package store persiste eventos e baselines em SQLite.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

var (
	ErrNotFound = errors.New("evento não encontrado")
	// ErrActiveExists impede reabrir um evento quando a mesma métrica já tem outro ativo.
	ErrActiveExists = errors.New("já existe um evento ativo para este driver, host e métrica")
)

const (
	StatusOpen          = "open"
	StatusAcknowledged  = "acknowledged"
	StatusResolved      = "resolved"
	StatusFalsePositive = "false_positive"
)

// ValidStatus informa se s é um status de evento conhecido.
func ValidStatus(s string) bool {
	switch s {
	case StatusOpen, StatusAcknowledged, StatusResolved, StatusFalsePositive:
		return true
	}
	return false
}

// activeStatuses são os status em que um evento ainda está "aberto": novas
// emissões da mesma métrica atualizam esse evento em vez de criar outro.
const activeStatuses = `'open', 'acknowledged'`

func isActive(status string) bool { return status == StatusOpen || status == StatusAcknowledged }

// Event é o registro auditável de uma anomalia detectada.
type Event struct {
	ID             string          `json:"id"`
	Timestamp      time.Time       `json:"timestamp"`
	Host           string          `json:"host"`
	Driver         string          `json:"driver"`
	DriverVersion  string          `json:"driver_version"`
	Mode           string          `json:"mode"`
	Metric         string          `json:"metric"`
	MetricLabel    string          `json:"metric_label"`
	Unit           string          `json:"unit"`
	Detector       string          `json:"detector"`
	Rule           string          `json:"rule,omitempty"`
	Value          float64         `json:"value"`
	BaselineMean   float64         `json:"baseline_mean"`
	BaselineStddev float64         `json:"baseline_stddev"`
	ZScore         float64         `json:"zscore"`
	Score          float64         `json:"score"`
	Severity       string          `json:"severity"`
	Explanation    string          `json:"explanation"`
	ProbableCause  string          `json:"probable_cause"`
	Recommendation string          `json:"recommendation"`
	Evidence       json.RawMessage `json:"evidence"`
	Status         string          `json:"status"`
	Feedback       string          `json:"feedback"`
	UpdatedAt      time.Time       `json:"updated_at"`
	// LastSeen é a emissão mais recente; Timestamp guarda a primeira.
	LastSeen    time.Time `json:"last_seen"`
	Occurrences int       `json:"occurrences"`
	// Attribution aponta os processos que mais consumiam o recurso no
	// momento da observação de pico (attribution.Result em JSON).
	Attribution json.RawMessage `json:"attribution,omitempty"`
}

type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS events (
	id              TEXT PRIMARY KEY,
	timestamp       TEXT NOT NULL,
	host            TEXT NOT NULL,
	driver          TEXT NOT NULL,
	driver_version  TEXT NOT NULL,
	mode            TEXT NOT NULL,
	metric          TEXT NOT NULL,
	value           REAL NOT NULL,
	baseline_mean   REAL NOT NULL,
	baseline_stddev REAL NOT NULL,
	zscore          REAL NOT NULL,
	score           REAL NOT NULL,
	severity        TEXT NOT NULL,
	explanation     TEXT NOT NULL,
	probable_cause  TEXT NOT NULL,
	recommendation  TEXT NOT NULL,
	evidence        TEXT NOT NULL,
	status          TEXT NOT NULL,
	feedback        TEXT NOT NULL DEFAULT '',
	updated_at      TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS events_timestamp ON events(timestamp DESC);
CREATE TABLE IF NOT EXISTS baselines (
	driver     TEXT NOT NULL,
	host       TEXT NOT NULL,
	metric     TEXT NOT NULL,
	state      TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	PRIMARY KEY (driver, host, metric)
);`

// Open abre (ou cria) o banco em path.
func Open(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("criando schema: %w", err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrando schema: %w", err)
	}
	return &Store{db: db}, nil
}

// addedColumns são colunas criadas depois da primeira versão do schema;
// bancos existentes as recebem via ALTER TABLE na abertura.
var addedColumns = []struct{ name, def string }{
	{"metric_label", "TEXT NOT NULL DEFAULT ''"},
	{"unit", "TEXT NOT NULL DEFAULT ''"},
	{"detector", "TEXT NOT NULL DEFAULT 'statistical'"},
	{"rule", "TEXT NOT NULL DEFAULT ''"},
	{"last_seen", "TEXT NOT NULL DEFAULT ''"},
	{"occurrences", "INTEGER NOT NULL DEFAULT 1"},
	{"attribution", "TEXT NOT NULL DEFAULT ''"},
}

func migrate(db *sql.DB) error {
	rows, err := db.Query(`SELECT name FROM pragma_table_info('events')`)
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return err
		}
		have[n] = true
	}
	rows.Close()
	for _, c := range addedColumns {
		if have[c.name] {
			continue
		}
		if _, err := db.Exec(`ALTER TABLE events ADD COLUMN ` + c.name + ` ` + c.def); err != nil {
			return err
		}
	}
	if err := consolidateActive(db); err != nil {
		return fmt.Errorf("consolidando eventos ativos duplicados: %w", err)
	}
	// Garante no banco no máximo um evento ativo por driver/host/métrica.
	_, err = db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS events_active_key
		ON events(driver, host, metric) WHERE status IN (` + activeStatuses + `)`)
	return err
}

// consolidateActive funde eventos ativos duplicados (gravados antes da
// deduplicação): o mais antigo fica ativo e acumula ocorrências e
// last_seen; os demais são resolvidos com uma nota apontando para ele.
func consolidateActive(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT driver, host, metric FROM events
		WHERE status IN (` + activeStatuses + `) GROUP BY driver, host, metric HAVING COUNT(*) > 1`)
	if err != nil {
		return err
	}
	var keys [][3]string
	for rows.Next() {
		var k [3]string
		if err := rows.Scan(&k[0], &k[1], &k[2]); err != nil {
			rows.Close()
			return err
		}
		keys = append(keys, k)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, k := range keys {
		var keep string
		err := tx.QueryRow(`SELECT id FROM events WHERE driver = ? AND host = ? AND metric = ?
			AND status IN (`+activeStatuses+`) ORDER BY timestamp ASC LIMIT 1`, k[0], k[1], k[2]).Scan(&keep)
		if err != nil {
			return err
		}
		_, err = tx.Exec(`UPDATE events SET
			occurrences = (SELECT SUM(occurrences) FROM events WHERE driver = ?1 AND host = ?2 AND metric = ?3 AND status IN (`+activeStatuses+`)),
			last_seen = (SELECT MAX(CASE WHEN last_seen > timestamp THEN last_seen ELSE timestamp END)
				FROM events WHERE driver = ?1 AND host = ?2 AND metric = ?3 AND status IN (`+activeStatuses+`))
			WHERE id = ?4`, k[0], k[1], k[2], keep)
		if err != nil {
			return err
		}
		_, err = tx.Exec(`UPDATE events SET status = ?, feedback = CASE WHEN feedback = '' THEN ? ELSE feedback END
			WHERE driver = ? AND host = ? AND metric = ? AND status IN (`+activeStatuses+`) AND id <> ?`,
			StatusResolved, "Duplicado: consolidado no evento "+keep+".", k[0], k[1], k[2], keep)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

const tsLayout = time.RFC3339Nano

func (s *Store) InsertEvent(ctx context.Context, e Event) error {
	return insertEvent(ctx, s.db, e)
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func insertEvent(ctx context.Context, db execer, e Event) error {
	if len(e.Evidence) == 0 {
		e.Evidence = json.RawMessage("[]")
	}
	if e.LastSeen.IsZero() {
		e.LastSeen = e.Timestamp
	}
	if e.Occurrences <= 0 {
		e.Occurrences = 1
	}
	_, err := db.ExecContext(ctx, `INSERT INTO events (
		id, timestamp, host, driver, driver_version, mode, metric, metric_label, unit, detector, rule, value,
		baseline_mean, baseline_stddev, zscore, score, severity, explanation,
		probable_cause, recommendation, evidence, status, feedback, updated_at, last_seen, occurrences, attribution
	) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		e.ID, e.Timestamp.UTC().Format(tsLayout), e.Host, e.Driver, e.DriverVersion, e.Mode,
		e.Metric, e.MetricLabel, e.Unit, detectorOrDefault(e.Detector), e.Rule, e.Value, e.BaselineMean, e.BaselineStddev, e.ZScore, e.Score, e.Severity,
		e.Explanation, e.ProbableCause, e.Recommendation, string(e.Evidence), e.Status,
		e.Feedback, e.UpdatedAt.UTC().Format(tsLayout), e.LastSeen.UTC().Format(tsLayout), e.Occurrences,
		string(e.Attribution))
	return err
}

// RecordEvent abre um evento ou, se já houver um ativo (open/acknowledged)
// para o mesmo driver, host e métrica, registra nele mais uma ocorrência.
// É idempotente por chave: reemissões (cooldown, reinício do serviço) não
// criam eventos duplicados. O evento ativo guarda o primeiro instante
// (Timestamp), o último (LastSeen) e a observação de maior score; status e
// feedback do SRE são preservados. Retorna o evento gravado e se foi criado.
func (s *Store) RecordEvent(ctx context.Context, e Event) (Event, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return e, false, err
	}
	defer tx.Rollback()
	cur, err := scanEvent(tx.QueryRowContext(ctx, `SELECT `+eventColumns+` FROM events
		WHERE driver = ? AND host = ? AND metric = ? AND status IN (`+activeStatuses+`)`,
		e.Driver, e.Host, e.Metric))
	if errors.Is(err, sql.ErrNoRows) {
		e.Status, e.LastSeen, e.Occurrences = StatusOpen, e.Timestamp, 1
		if err := insertEvent(ctx, tx, e); err != nil {
			return e, false, err
		}
		return e, true, tx.Commit()
	}
	if err != nil {
		return e, false, err
	}

	out := cur
	if e.Score > cur.Score {
		out = e
		out.ID, out.Timestamp, out.Status, out.Feedback = cur.ID, cur.Timestamp, cur.Status, cur.Feedback
	}
	if len(out.Attribution) == 0 { // pico sem atribuição: aproveita a desta emissão
		out.Attribution = e.Attribution
	}
	out.Occurrences = cur.Occurrences + 1
	out.LastSeen = cur.LastSeen
	if e.Timestamp.After(out.LastSeen) {
		out.LastSeen = e.Timestamp
	}
	out.UpdatedAt = e.UpdatedAt
	if len(out.Evidence) == 0 {
		out.Evidence = json.RawMessage("[]")
	}
	_, err = tx.ExecContext(ctx, `UPDATE events SET
		driver_version = ?, mode = ?, metric_label = ?, unit = ?, detector = ?, rule = ?, value = ?,
		baseline_mean = ?, baseline_stddev = ?, zscore = ?, score = ?, severity = ?, explanation = ?,
		probable_cause = ?, recommendation = ?, evidence = ?, updated_at = ?, last_seen = ?, occurrences = ?,
		attribution = ?
		WHERE id = ?`,
		out.DriverVersion, out.Mode, out.MetricLabel, out.Unit, detectorOrDefault(out.Detector), out.Rule, out.Value,
		out.BaselineMean, out.BaselineStddev, out.ZScore, out.Score, out.Severity, out.Explanation,
		out.ProbableCause, out.Recommendation, string(out.Evidence), out.UpdatedAt.UTC().Format(tsLayout),
		out.LastSeen.UTC().Format(tsLayout), out.Occurrences, string(out.Attribution), out.ID)
	if err != nil {
		return e, false, err
	}
	return out, false, tx.Commit()
}

func detectorOrDefault(d string) string {
	if d == "" {
		return "statistical"
	}
	return d
}

const eventColumns = `id, timestamp, host, driver, driver_version, mode, metric, metric_label, unit, detector, rule, value,
	baseline_mean, baseline_stddev, zscore, score, severity, explanation,
	probable_cause, recommendation, evidence, status, feedback, updated_at, last_seen, occurrences, attribution`

func scanEvent(row interface{ Scan(...any) error }) (Event, error) {
	var e Event
	var ts, updated, lastSeen, evidence, attribution string
	err := row.Scan(&e.ID, &ts, &e.Host, &e.Driver, &e.DriverVersion, &e.Mode, &e.Metric,
		&e.MetricLabel, &e.Unit, &e.Detector, &e.Rule, &e.Value, &e.BaselineMean, &e.BaselineStddev, &e.ZScore, &e.Score, &e.Severity,
		&e.Explanation, &e.ProbableCause, &e.Recommendation, &evidence, &e.Status,
		&e.Feedback, &updated, &lastSeen, &e.Occurrences, &attribution)
	if err != nil {
		return e, err
	}
	e.Timestamp, _ = time.Parse(tsLayout, ts)
	e.UpdatedAt, _ = time.Parse(tsLayout, updated)
	e.LastSeen, _ = time.Parse(tsLayout, lastSeen)
	if e.LastSeen.IsZero() {
		e.LastSeen = e.Timestamp
	}
	e.Evidence = json.RawMessage(evidence)
	if attribution != "" {
		e.Attribution = json.RawMessage(attribution)
	}
	return e, nil
}

func (s *Store) GetEvent(ctx context.Context, id string) (Event, error) {
	e, err := scanEvent(s.db.QueryRowContext(ctx, `SELECT `+eventColumns+` FROM events WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return e, ErrNotFound
	}
	return e, err
}

// Filter restringe a listagem; campos vazios são ignorados.
type Filter struct {
	Host     string
	Driver   string
	Metric   string
	Severity string
	Status   string
	Since    time.Time
	Limit    int
}

// ListEvents retorna os eventos mais recentes primeiro.
func (s *Store) ListEvents(ctx context.Context, f Filter) ([]Event, error) {
	var where []string
	var args []any
	for col, v := range map[string]string{
		"host": f.Host, "driver": f.Driver, "metric": f.Metric,
		"severity": f.Severity, "status": f.Status,
	} {
		if v != "" {
			where = append(where, col+" = ?")
			args = append(args, v)
		}
	}
	if !f.Since.IsZero() {
		where = append(where, "timestamp >= ?")
		args = append(args, f.Since.UTC().Format(tsLayout))
	}
	q := `SELECT ` + eventColumns + ` FROM events`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	if f.Limit <= 0 {
		f.Limit = 50
	}
	q += " ORDER BY timestamp DESC LIMIT ?"
	args = append(args, f.Limit)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// UpdateEvent altera status e feedback e retorna o evento antes e depois.
func (s *Store) UpdateEvent(ctx context.Context, id, status, feedback string, at time.Time) (before, after Event, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return before, after, err
	}
	defer tx.Rollback()
	before, err = scanEvent(tx.QueryRowContext(ctx, `SELECT `+eventColumns+` FROM events WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return before, after, ErrNotFound
	}
	if err != nil {
		return before, after, err
	}
	if isActive(status) && !isActive(before.Status) {
		var other string
		err := tx.QueryRowContext(ctx, `SELECT id FROM events WHERE driver = ? AND host = ? AND metric = ?
			AND status IN (`+activeStatuses+`) AND id <> ?`, before.Driver, before.Host, before.Metric, id).Scan(&other)
		if err == nil {
			return before, after, fmt.Errorf("%w (%s)", ErrActiveExists, other)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return before, after, err
		}
	}
	after = before
	after.Status = status
	after.Feedback = feedback
	after.UpdatedAt = at
	_, err = tx.ExecContext(ctx, `UPDATE events SET status = ?, feedback = ?, updated_at = ? WHERE id = ?`,
		status, feedback, at.UTC().Format(tsLayout), id)
	if err != nil {
		return before, after, err
	}
	return before, after, tx.Commit()
}

// Baseline é o estado serializado de um detector.
type Baseline struct {
	Driver string
	Host   string
	Metric string
	State  json.RawMessage
}

func (s *Store) SaveBaselines(ctx context.Context, bs []Baseline, at time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, b := range bs {
		_, err := tx.ExecContext(ctx, `INSERT INTO baselines (driver, host, metric, state, updated_at)
			VALUES (?,?,?,?,?)
			ON CONFLICT (driver, host, metric) DO UPDATE SET state = excluded.state, updated_at = excluded.updated_at`,
			b.Driver, b.Host, b.Metric, string(b.State), at.UTC().Format(tsLayout))
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) LoadBaselines(ctx context.Context, driver string) ([]Baseline, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT host, metric, state FROM baselines WHERE driver = ?`, driver)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Baseline
	for rows.Next() {
		b := Baseline{Driver: driver}
		var state string
		if err := rows.Scan(&b.Host, &b.Metric, &state); err != nil {
			return nil, err
		}
		b.State = json.RawMessage(state)
		out = append(out, b)
	}
	return out, rows.Err()
}
