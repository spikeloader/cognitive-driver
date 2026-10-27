// Package api expõe a API HTTP/JSON do serviço.
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/spikeloader/cognitive-driver/internal/collector"
	"github.com/spikeloader/cognitive-driver/internal/driver"
	"github.com/spikeloader/cognitive-driver/internal/feature"
	"github.com/spikeloader/cognitive-driver/internal/store"
	"github.com/spikeloader/cognitive-driver/internal/telemetry"
)

const maxBodyBytes = 64 * 1024

type Server struct {
	Version     string
	Drivers     []driver.Driver // todos, inclusive desabilitados
	DefaultHost string          // usado por /predict quando host é omitido
	Store       *store.Store
	Metrics     *telemetry.Metrics
	Status      *collector.StatusTracker
	Token       string // vazio = autenticação desativada
	Log         *slog.Logger
	Started     time.Time
	Now         func() time.Time
}

// Handler monta as rotas. /health e /metrics são públicos para health
// checks e scrape do Prometheus; os demais exigem o token, se configurado.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.health)
	mux.Handle("GET /metrics", promhttp.HandlerFor(s.Metrics.Registry, promhttp.HandlerOpts{}))
	mux.Handle("POST /predict", s.auth(http.HandlerFunc(s.predict)))
	mux.Handle("GET /events", s.auth(http.HandlerFunc(s.listEvents)))
	mux.Handle("GET /events/{id}", s.auth(http.HandlerFunc(s.getEvent)))
	mux.Handle("PATCH /events/{id}", s.auth(http.HandlerFunc(s.updateEvent)))
	// O painel é público; os dados que ele busca continuam exigindo o token.
	mux.Handle("GET /ui/", uiHandler())
	mux.Handle("GET /favicon.ico", http.RedirectHandler("/ui/favicon-32.png", http.StatusMovedPermanently))
	mux.Handle("GET /{$}", http.RedirectHandler("/ui/", http.StatusFound))
	mux.Handle("GET /ui", http.RedirectHandler("/ui/", http.StatusMovedPermanently))
	return s.logRequests(mux)
}

func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.Token != "" {
			got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(s.Token)) != 1 {
				w.Header().Set("WWW-Authenticate", `Bearer realm="cognitive-driver"`)
				writeError(w, http.StatusUnauthorized, "token ausente ou inválido")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		s.Log.Debug("http_request", "method", r.Method, "path", r.URL.Path,
			"status", rec.status, "duration_ms", time.Since(start).Milliseconds(), "remote", r.RemoteAddr)
	})
}

type healthResponse struct {
	Status        string                   `json:"status"`
	Version       string                   `json:"version"`
	UptimeSeconds int64                    `json:"uptime_seconds"`
	Drivers       []driver.Info            `json:"drivers"`
	Targets       []collector.TargetStatus `json:"targets"`
	Storage       string                   `json:"storage"`
}

// health responde 200 enquanto o processo e o banco estão operantes; falha
// de coleta aparece como "degraded" sem derrubar o health check.
func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	resp := healthResponse{
		Status:        "ok",
		Version:       s.Version,
		UptimeSeconds: int64(s.now().Sub(s.Started).Seconds()),
		Targets:       s.Status.Snapshot(),
		Storage:       "ok",
	}
	for _, d := range s.Drivers {
		resp.Drivers = append(resp.Drivers, d.Info())
	}
	for _, t := range resp.Targets {
		if !t.OK && !t.LastAttempt.IsZero() {
			resp.Status = "degraded"
		}
	}
	code := http.StatusOK
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.Store.Ping(ctx); err != nil {
		resp.Status, resp.Storage, code = "unhealthy", err.Error(), http.StatusServiceUnavailable
	}
	writeJSON(w, code, resp)
}

type predictRequest struct {
	Driver    string    `json:"driver"`
	Host      string    `json:"host"`
	Metric    string    `json:"metric"`
	Value     *float64  `json:"value"`
	Timestamp time.Time `json:"timestamp"`
}

type baseline struct {
	Mean   float64 `json:"mean"`
	StdDev float64 `json:"stddev"`
}

type predictResponse struct {
	driver.Anomaly
	Baseline       baseline `json:"baseline"`
	Explanation    string   `json:"explanation,omitempty"`
	ProbableCause  string   `json:"probable_cause,omitempty"`
	Recommendation string   `json:"recommendation,omitempty"`
	Mode           string   `json:"mode"`
}

// predict avalia um valor contra a baseline atual sem alterá-la e sem
// gerar evento. O valor deve estar na unidade da feature (taxa por segundo
// para contadores).
func (s *Server) predict(w http.ResponseWriter, r *http.Request) {
	var req predictRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Metric == "" || req.Value == nil {
		writeError(w, http.StatusBadRequest, "campos obrigatórios: metric, value")
		return
	}
	if req.Host == "" {
		req.Host = s.DefaultHost
	}
	if req.Host == "" {
		writeError(w, http.StatusBadRequest, "campo host é obrigatório quando há mais de um alvo")
		return
	}
	if req.Timestamp.IsZero() {
		req.Timestamp = s.now()
	}
	d, err := s.findDriver(req.Driver, req.Metric)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	info := d.Info()
	if !info.Enabled {
		writeError(w, http.StatusConflict, "driver desabilitado: "+info.Name)
		return
	}
	a, err := d.Evaluate(feature.Feature{Host: req.Host, Metric: req.Metric, Value: *req.Value, Timestamp: req.Timestamp})
	switch {
	case errors.Is(err, driver.ErrNoBaseline):
		writeError(w, http.StatusConflict, err.Error())
		return
	case err != nil:
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	resp := predictResponse{Anomaly: a, Baseline: baseline{a.Mean, a.StdDev}, Mode: info.Mode}
	if a.Anomalous {
		ex := d.Explain(a)
		resp.Explanation = ex.Summary
		resp.ProbableCause = ex.ProbableCause
		resp.Recommendation = d.Recommend(a).Description
	}
	s.Log.Info("api_prediction", "driver", info.Name, "host", req.Host, "metric", req.Metric,
		"observed", *req.Value, "zscore", a.ZScore, "anomaly", a.Anomalous, "severity", string(a.Severity))
	writeJSON(w, http.StatusOK, resp)
}

// findDriver escolhe pelo nome ou, sem nome, pelo primeiro driver que
// monitora a métrica.
func (s *Server) findDriver(name, metric string) (driver.Driver, error) {
	for _, d := range s.Drivers {
		info := d.Info()
		if name != "" {
			if info.Name == name {
				return d, nil
			}
			continue
		}
		base, _ := feature.SplitSeries(metric)
		for _, m := range info.Metrics {
			if m == base && info.Enabled {
				return d, nil
			}
		}
	}
	if name != "" {
		return nil, errors.New("driver não encontrado: " + name)
	}
	return nil, errors.New("nenhum driver habilitado monitora a métrica: " + metric)
}

func (s *Server) listEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.Filter{
		Host:     q.Get("host"),
		Driver:   q.Get("driver"),
		Metric:   q.Get("metric"),
		Severity: q.Get("severity"),
		Status:   q.Get("status"),
		Limit:    50,
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 500 {
			writeError(w, http.StatusBadRequest, "limit deve estar entre 1 e 500")
			return
		}
		f.Limit = n
	}
	if v := q.Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "since deve estar em RFC3339")
			return
		}
		f.Since = t
	}
	if f.Status != "" && !store.ValidStatus(f.Status) {
		writeError(w, http.StatusBadRequest, "status inválido")
		return
	}
	events, err := s.Store.ListEvents(r.Context(), f)
	if err != nil {
		s.Log.Error("events_list_failed", "error", err)
		writeError(w, http.StatusInternalServerError, "falha ao listar eventos")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events, "count": len(events)})
}

func (s *Server) getEvent(w http.ResponseWriter, r *http.Request) {
	e, err := s.Store.GetEvent(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		s.Log.Error("event_get_failed", "error", err)
		writeError(w, http.StatusInternalServerError, "falha ao buscar evento")
		return
	}
	writeJSON(w, http.StatusOK, e)
}

type updateRequest struct {
	Status   string  `json:"status"`
	Feedback *string `json:"feedback"`
}

// updateEvent registra o feedback do SRE (ack, resolução, falso positivo).
func (s *Server) updateEvent(w http.ResponseWriter, r *http.Request) {
	var req updateRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if !store.ValidStatus(req.Status) {
		writeError(w, http.StatusBadRequest, "status deve ser open, acknowledged, resolved ou false_positive")
		return
	}
	id := r.PathValue("id")
	current, err := s.Store.GetEvent(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "falha ao buscar evento")
		return
	}
	feedback := current.Feedback
	if req.Feedback != nil {
		feedback = *req.Feedback
	}
	before, after, err := s.Store.UpdateEvent(r.Context(), id, req.Status, feedback, s.now())
	if errors.Is(err, store.ErrActiveExists) {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		s.Log.Error("event_update_failed", "event_id", id, "error", err)
		writeError(w, http.StatusInternalServerError, "falha ao atualizar evento")
		return
	}
	if after.Status == store.StatusFalsePositive && before.Status != store.StatusFalsePositive {
		s.Metrics.FalsePositives.WithLabelValues(after.Driver, after.Metric).Inc()
	}
	s.Log.Info("event_feedback", "event_id", id, "driver", after.Driver, "metric", after.Metric,
		"status_before", before.Status, "status", after.Status, "feedback", after.Feedback)
	writeJSON(w, http.StatusOK, after)
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "JSON inválido: "+err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}
