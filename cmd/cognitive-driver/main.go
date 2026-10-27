// Comando cognitive-driver: daemon de micro-IA para detecção de anomalias
// em métricas de hosts Linux (modo observe_only).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"github.com/spikeloader/cognitive-driver/internal/api"
	"github.com/spikeloader/cognitive-driver/internal/attribution"
	"github.com/spikeloader/cognitive-driver/internal/collector"
	"github.com/spikeloader/cognitive-driver/internal/config"
	"github.com/spikeloader/cognitive-driver/internal/driver"
	"github.com/spikeloader/cognitive-driver/internal/runner"
	"github.com/spikeloader/cognitive-driver/internal/store"
	"github.com/spikeloader/cognitive-driver/internal/telemetry"
)

// version é definido em build via -ldflags "-X main.version=...".
var version = "0.1.0-dev"

func main() {
	configPath := flag.String("config", "config.yaml", "caminho do arquivo de configuração YAML")
	showVersion := flag.Bool("version", false, "exibe a versão e sai")
	healthURL := flag.String("healthcheck", "", "consulta a URL de /health e sai com código 0 se saudável")
	checkOnly := flag.Bool("check", false, "valida o arquivo de configuração e sai")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	if *checkOnly {
		if _, err := config.Load(*configPath); err != nil {
			fmt.Fprintf(os.Stderr, "configuração inválida (%s):\n%v\n", *configPath, err)
			os.Exit(1)
		}
		fmt.Printf("configuração válida: %s\n", *configPath)
		return
	}
	if *healthURL != "" {
		os.Exit(healthcheck(*healthURL))
	}
	if err := run(*configPath); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run(configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	log := newLogger(cfg.Service.LogLevel)
	slog.SetDefault(log)

	if err := os.MkdirAll(cfg.Service.DataDir, 0o750); err != nil {
		return fmt.Errorf("criando data_dir: %w", err)
	}
	st, err := store.Open(filepath.Join(cfg.Service.DataDir, "cognitive-driver.db"))
	if err != nil {
		return fmt.Errorf("abrindo banco: %w", err)
	}
	defer st.Close()

	metrics := telemetry.New()
	var all, enabled []driver.Driver
	// Métricas brutas por job: "node" agrega os drivers que usam os alvos
	// globais; cada driver com alvos próprios tem o seu job.
	wanted := map[string]map[string]bool{}
	var targets []collector.Collector
	status := collector.NewStatusTracker()
	addTarget := func(host, job, url string, names map[string]bool) {
		list := make([]string, 0, len(names))
		for n := range names {
			list = append(list, n)
		}
		targets = append(targets, collector.NewHTTPScraper(host, job, url, cfg.Collector.Timeout, list))
		status.Register(host, job, url)
	}
	for _, dc := range cfg.Drivers {
		d := driver.NewStatistical(dc, cfg.Collector.Interval)
		all = append(all, d)
		info := d.Info()
		active := 0.0
		if info.Enabled {
			enabled = append(enabled, d)
			active = 1
			if wanted[info.Job] == nil {
				wanted[info.Job] = map[string]bool{}
			}
			for _, m := range d.SourceMetrics() {
				wanted[info.Job][m] = true
			}
		}
		metrics.Info.WithLabelValues(info.Name, info.Version, info.Contract, info.Mode).Set(active)
		log.Info("driver_loaded", "driver", info.Name, "version", info.Version, "contract", info.Contract,
			"mode", info.Mode, "enabled", info.Enabled, "job", info.Job, "metrics", info.Metrics, "source", dc.Source)
	}
	var hosts []string
	if names := wanted["node"]; names != nil {
		for _, t := range cfg.Collector.Targets {
			addTarget(t.Host, "node", t.URL, names)
			hosts = append(hosts, t.Host)
		}
	}
	for _, dc := range cfg.Drivers {
		if !dc.IsEnabled() || len(dc.Targets) == 0 {
			continue
		}
		for _, t := range dc.Targets {
			addTarget(t.Host, dc.Name, t.URL, wanted[dc.Name])
			if !slices.Contains(hosts, t.Host) {
				hosts = append(hosts, t.Host)
			}
		}
	}

	r := &runner.Runner{
		Interval: cfg.Collector.Interval,
		Timeout:  cfg.Collector.Timeout,
		Targets:  targets,
		Drivers:  enabled,
		Store:    st,
		Metrics:  metrics,
		Status:   status,
		Log:      log,
	}
	// O process-exporter de cada host fica no mesmo endereço do node_exporter.
	if port := cfg.Attribution.Port(); port > 0 {
		urls := map[string]string{}
		for _, t := range cfg.Collector.Targets {
			u, err := attribution.ProcessURL(t.URL, port)
			if err != nil {
				return fmt.Errorf("attribution: %w", err)
			}
			urls[t.Host] = u
		}
		r.Attributor = attribution.New(urls, cfg.Attribution.SampleGap, cfg.Collector.Timeout, cfg.Attribution.Top)
		log.Info("attribution_enabled", "source", "process-exporter", "port", port, "hosts", len(urls))
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := r.RestoreBaselines(ctx); err != nil {
		log.Warn("baseline_restore_failed", "error", err)
	}

	token := os.Getenv(cfg.API.AuthTokenEnv)
	if token == "" {
		log.Warn("auth_disabled", "reason", "variável "+cfg.API.AuthTokenEnv+" vazia; /predict e /events sem autenticação")
	}
	defaultHost := ""
	if len(hosts) == 1 {
		defaultHost = hosts[0]
	}
	srv := &http.Server{
		Addr: cfg.API.Listen,
		Handler: (&api.Server{
			Version:     version,
			Drivers:     all,
			DefaultHost: defaultHost,
			Store:       st,
			Metrics:     metrics,
			Status:      status,
			Token:       token,
			Log:         log,
			Started:     time.Now(),
		}).Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	errc := make(chan error, 1)
	go func() {
		log.Info("api_listening", "addr", cfg.API.Listen, "version", version)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	done := make(chan struct{})
	go func() {
		defer close(done)
		r.Run(ctx)
	}()

	saveTicker := time.NewTicker(cfg.Service.BaselineSaveInterval)
	defer saveTicker.Stop()
	var runErr error
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case runErr = <-errc:
			stop()
			break loop
		case <-saveTicker.C:
			if err := r.SaveBaselines(ctx); err != nil {
				log.Error("baseline_save_failed", "error", err)
			}
		}
	}

	log.Info("shutting_down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	<-done
	if err := r.SaveBaselines(shutdownCtx); err != nil {
		log.Error("baseline_save_failed", "error", err)
	}
	return runErr
}

func healthcheck(url string) int {
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get(url)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "status", resp.StatusCode)
		return 1
	}
	return 0
}

func newLogger(level string) *slog.Logger {
	var l slog.Level
	_ = l.UnmarshalText([]byte(level))
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: l})).
		With("service", "cognitive-driver")
}
