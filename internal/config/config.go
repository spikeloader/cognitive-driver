// Package config carrega e valida a configuração YAML do serviço.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// ModeObserveOnly é o único modo de operação suportado no MVP.
const ModeObserveOnly = "observe_only"

// Units aceitas em describe.unit; definem como os valores são exibidos.
var Units = []string{"", "cores", "bytes", "bytes_per_second", "ratio", "per_second", "count", "milliseconds", "seconds", "bool"}

var severities = []string{"low", "medium", "high", "critical"}

var labelName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

type Config struct {
	Service   Service   `yaml:"service"`
	Collector Collector `yaml:"collector"`
	API       API       `yaml:"api"`
	// Attribution identifica o processo responsável pelos eventos.
	Attribution Attribution `yaml:"attribution"`
	Drivers     []Driver    `yaml:"drivers"`
}

// Attribution lê o process-exporter dos hosts de collector.targets (mesmo
// endereço do node_exporter, outra porta) quando um evento é emitido.
type Attribution struct {
	// ProcessExporterPort é a porta do process-exporter; 0 desativa.
	ProcessExporterPort *int `yaml:"process_exporter_port"`
	// SampleGap separa as duas amostras usadas para calcular CPU e I/O.
	SampleGap time.Duration `yaml:"sample_gap"`
	// Top é quantos grupos de processos guardar no evento.
	Top int `yaml:"top"`
}

// Port retorna a porta do process-exporter (padrão 9256) ou 0 se desativado.
func (a Attribution) Port() int {
	if a.ProcessExporterPort == nil {
		return 9256
	}
	return *a.ProcessExporterPort
}

// Recursos aceitos em metrics[].attribute ("none" desativa o padrão).
var attributeResources = []string{"cpu", "memory", "io", "none"}

type Service struct {
	DataDir              string        `yaml:"data_dir"`
	LogLevel             string        `yaml:"log_level"`
	BaselineSaveInterval time.Duration `yaml:"baseline_save_interval"`
	// DriversDir contém arquivos *.yaml com drivers adicionais (um serviço
	// por arquivo). Padrão: drivers.d ao lado do arquivo de configuração.
	DriversDir string `yaml:"drivers_dir"`
}

type Collector struct {
	Interval time.Duration `yaml:"interval"`
	Timeout  time.Duration `yaml:"timeout"`
	Targets  []Target      `yaml:"targets"`
}

type Target struct {
	Host string `yaml:"host"`
	URL  string `yaml:"url"`
}

type API struct {
	Listen       string `yaml:"listen"`
	AuthTokenEnv string `yaml:"auth_token_env"`
}

type Driver struct {
	Name              string          `yaml:"name"`
	Version           string          `yaml:"version"`
	Enabled           *bool           `yaml:"enabled"`
	Mode              string          `yaml:"mode"`
	WindowSizes       []time.Duration `yaml:"window_sizes"`
	DecisionWindow    time.Duration   `yaml:"decision_window"`
	AnomalyThreshold  float64         `yaml:"anomaly_threshold"`
	ConsecutivePoints int             `yaml:"consecutive_points"`
	Cooldown          time.Duration   `yaml:"cooldown"`
	WarmupPoints      int             `yaml:"warmup_points"`
	MinStddevRatio    float64         `yaml:"min_stddev_ratio"`
	BaselineFreeze    int             `yaml:"baseline_freeze_points"`
	// Targets próprios do driver (ex.: o exporter do nginx). Sem eles, o
	// driver observa os alvos globais de collector.targets.
	Targets []Target `yaml:"targets"`
	Metrics []Metric `yaml:"metrics"`

	// Source é o arquivo de onde o driver foi lido (para mensagens de erro).
	Source string `yaml:"-"`
}

// IsEnabled retorna true quando o driver não foi desativado explicitamente.
func (d Driver) IsEnabled() bool { return d.Enabled == nil || *d.Enabled }

// Job identifica o conjunto de alvos que o driver observa.
func (d Driver) Job() string {
	if len(d.Targets) > 0 {
		return d.Name
	}
	return "node"
}

// Operand é uma seleção de séries de uma métrica, somadas.
type Operand struct {
	Name string `yaml:"name"`
	// counter | gauge | timestamp; vazio = inferido pelo sufixo _total.
	// timestamp: o valor é um Unix timestamp e a feature é a idade em segundos.
	Type    string            `yaml:"type"`
	Match   map[string]string `yaml:"match"`
	Exclude map[string]string `yaml:"exclude"`
}

// Metric descreve uma métrica monitorada. Aceita a forma curta (apenas o
// nome) ou a forma completa com filtros de labels (regex ancorada).
type Metric struct {
	Operand `yaml:",inline"`
	// Alias é o nome da feature; obrigatório para métricas derivadas.
	Alias string `yaml:"alias"`
	// GroupBy gera uma feature (e um evento) por combinação desses labels,
	// em vez de somar as séries: um job de cron, um pool do PHP-FPM.
	GroupBy []string `yaml:"group_by"`
	// Métricas derivadas: valor = métrica - subtract, ou métrica / divide_by.
	Subtract *Operand `yaml:"subtract"`
	DivideBy *Operand `yaml:"divide_by"`
	// Attribute indica o recurso (cpu, memory, io) cujo maior consumidor é
	// apontado como responsável pelos eventos da métrica. Vazio usa o padrão
	// da métrica (CPU, memória e disco do host já têm um); none desativa.
	Attribute string `yaml:"attribute"`
	// Rule troca a detecção estatística por uma regra fixa (sem aprendizado).
	Rule *Rule `yaml:"rule"`
	// MinStddev é um piso absoluto do desvio-padrão (útil para contagens).
	MinStddev float64   `yaml:"min_stddev"`
	Describe  *Describe `yaml:"describe"`
}

// Key é o nome da feature produzida pela métrica.
func (m Metric) Key() string {
	if m.Alias != "" {
		return m.Alias
	}
	return m.Name
}

// Rule dispara quando o valor fica acima de Above, abaixo de Below ou
// diferente de Equals (exatamente um deles).
type Rule struct {
	Above    *float64 `yaml:"above"`
	Below    *float64 `yaml:"below"`
	Equals   *float64 `yaml:"equals"`
	Severity string   `yaml:"severity"`
}

// Describe personaliza a explicação de uma métrica (sobrepõe o catálogo).
type Describe struct {
	Label string   `yaml:"label"`
	Unit  string   `yaml:"unit"`
	Up    Guidance `yaml:"up"`
	Down  Guidance `yaml:"down"`
}

type Guidance struct {
	Cause          string `yaml:"cause"`
	Recommendation string `yaml:"recommendation"`
}

func (m *Metric) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		m.Name = n.Value
		return nil
	}
	type plain Metric
	return n.Decode((*plain)(m))
}

// Load lê o arquivo principal e os drivers de drivers.d, aplica defaults e
// valida o resultado.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("lendo configuração: %w", err)
	}
	cfg, err := decode(data)
	if err != nil {
		return nil, err
	}
	dir := cfg.Service.DriversDir
	if dir == "" {
		dir = filepath.Join(filepath.Dir(path), "drivers.d")
	} else if !filepath.IsAbs(dir) {
		dir = filepath.Join(filepath.Dir(path), dir)
	}
	extra, err := loadDriversDir(dir)
	if err != nil {
		return nil, err
	}
	cfg.Drivers = append(cfg.Drivers, extra...)
	return cfg.finish()
}

// Parse interpreta apenas o conteúdo informado (sem drivers.d).
func Parse(data []byte) (*Config, error) {
	cfg, err := decode(data)
	if err != nil {
		return nil, err
	}
	return cfg.finish()
}

func decode(data []byte) (*Config, error) {
	var cfg Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("interpretando YAML: %w", err)
	}
	return &cfg, nil
}

func (c *Config) finish() (*Config, error) {
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// loadDriversDir lê, em ordem alfabética, os arquivos *.yaml do diretório.
// Cada arquivo contém `drivers: [...]`. Diretório inexistente é ignorado.
func loadDriversDir(dir string) ([]Driver, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, err
	}
	var out []Driver
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("lendo %s: %w", f, err)
		}
		var doc struct {
			Drivers []Driver `yaml:"drivers"`
		}
		dec := yaml.NewDecoder(bytes.NewReader(data))
		dec.KnownFields(true)
		if err := dec.Decode(&doc); err != nil {
			return nil, fmt.Errorf("interpretando %s: %w", f, err)
		}
		for i := range doc.Drivers {
			doc.Drivers[i].Source = f
		}
		out = append(out, doc.Drivers...)
	}
	return out, nil
}

func (c *Config) applyDefaults() {
	if c.Service.DataDir == "" {
		c.Service.DataDir = "./data"
	}
	if c.Service.LogLevel == "" {
		c.Service.LogLevel = "info"
	}
	if c.Service.BaselineSaveInterval == 0 {
		c.Service.BaselineSaveInterval = time.Minute
	}
	if c.Collector.Interval == 0 {
		c.Collector.Interval = 15 * time.Second
	}
	if c.Collector.Timeout == 0 {
		c.Collector.Timeout = 5 * time.Second
	}
	if c.API.Listen == "" {
		c.API.Listen = ":8088"
	}
	if c.API.AuthTokenEnv == "" {
		c.API.AuthTokenEnv = "MICRO_AI_TOKEN"
	}
	if c.Attribution.SampleGap == 0 {
		c.Attribution.SampleGap = 2 * time.Second
	}
	if c.Attribution.Top == 0 {
		c.Attribution.Top = 5
	}
	for i := range c.Drivers {
		d := &c.Drivers[i]
		if d.Version == "" {
			d.Version = "0.1.0"
		}
		if d.Mode == "" {
			d.Mode = ModeObserveOnly
		}
		if len(d.WindowSizes) == 0 {
			d.WindowSizes = []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute}
		}
		if d.DecisionWindow == 0 {
			d.DecisionWindow = 5 * time.Minute
		}
		if d.AnomalyThreshold == 0 {
			d.AnomalyThreshold = 3.0
		}
		if d.ConsecutivePoints == 0 {
			d.ConsecutivePoints = 3
		}
		if d.Cooldown == 0 {
			d.Cooldown = 5 * time.Minute
		}
		if d.WarmupPoints == 0 {
			d.WarmupPoints = 20
		}
		if d.MinStddevRatio == 0 {
			d.MinStddevRatio = 0.01
		}
		if d.BaselineFreeze == 0 {
			d.BaselineFreeze = 10
		}
		for j := range d.Metrics {
			if r := d.Metrics[j].Rule; r != nil && r.Severity == "" {
				r.Severity = "high"
			}
		}
	}
}

// Validate retorna todos os problemas encontrados de uma vez.
func (c *Config) Validate() error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if c.Collector.Interval < time.Second {
		add("collector.interval deve ser >= 1s")
	}
	if c.Collector.Timeout <= 0 || c.Collector.Timeout > c.Collector.Interval {
		add("collector.timeout deve ser > 0 e <= collector.interval")
	}
	if len(c.Collector.Targets) == 0 {
		add("collector.targets: ao menos um alvo é obrigatório")
	}
	validateTargets("collector.targets", c.Collector.Targets, add)
	if p := c.Attribution.Port(); p < 0 || p > 65535 {
		add("attribution.process_exporter_port inválida: %d", p)
	}
	if c.Attribution.SampleGap < 100*time.Millisecond || c.Attribution.SampleGap > c.Collector.Timeout {
		add("attribution.sample_gap deve estar entre 100ms e collector.timeout")
	}
	if c.Attribution.Top < 1 || c.Attribution.Top > 20 {
		add("attribution.top deve estar entre 1 e 20")
	}
	switch c.Service.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		add("service.log_level inválido: %q", c.Service.LogLevel)
	}

	if len(c.Drivers) == 0 {
		add("drivers: ao menos um driver é obrigatório")
	}
	names := map[string]bool{}
	for i, d := range c.Drivers {
		p := fmt.Sprintf("drivers[%d]", i)
		if d.Source != "" {
			p = fmt.Sprintf("%s (%s)", p, filepath.Base(d.Source))
		}
		if d.Name == "" {
			add("%s.name é obrigatório", p)
		} else if names[d.Name] {
			add("%s.name duplicado: %q", p, d.Name)
		}
		names[d.Name] = true
		if d.Name == "node" {
			add("%s.name %q é reservado", p, d.Name)
		}
		if d.Mode != ModeObserveOnly {
			add("%s.mode %q não suportado no MVP (apenas %q)", p, d.Mode, ModeObserveOnly)
		}
		validateTargets(p+".targets", d.Targets, add)
		for _, w := range d.WindowSizes {
			if w < c.Collector.Interval {
				add("%s.window_sizes: janela %s menor que o intervalo de coleta", p, w)
			}
		}
		if !slices.Contains(d.WindowSizes, d.DecisionWindow) {
			add("%s.decision_window %s deve estar em window_sizes", p, d.DecisionWindow)
		}
		if d.AnomalyThreshold <= 0 {
			add("%s.anomaly_threshold deve ser > 0", p)
		}
		if d.ConsecutivePoints < 1 {
			add("%s.consecutive_points deve ser >= 1", p)
		}
		if d.Cooldown < 0 {
			add("%s.cooldown não pode ser negativo", p)
		}
		if d.WarmupPoints < 2 {
			add("%s.warmup_points deve ser >= 2", p)
		}
		if d.BaselineFreeze < 0 {
			add("%s.baseline_freeze_points não pode ser negativo", p)
		}
		if d.MinStddevRatio < 0 {
			add("%s.min_stddev_ratio não pode ser negativo", p)
		}
		if len(d.Metrics) == 0 {
			add("%s.metrics: ao menos uma métrica é obrigatória", p)
		}
		keys := map[string]bool{}
		for j, m := range d.Metrics {
			mp := fmt.Sprintf("%s.metrics[%d]", p, j)
			validateOperand(mp, m.Operand, add)
			if keys[m.Key()] {
				add("%s: métrica %q duplicada no driver (use alias)", mp, m.Key())
			}
			keys[m.Key()] = true
			if m.Subtract != nil && m.DivideBy != nil {
				add("%s: use subtract ou divide_by, não ambos", mp)
			}
			if (m.Subtract != nil || m.DivideBy != nil) && m.Alias == "" {
				add("%s: métrica derivada exige alias", mp)
			}
			if m.Subtract != nil {
				validateOperand(mp+".subtract", *m.Subtract, add)
			}
			if m.DivideBy != nil {
				validateOperand(mp+".divide_by", *m.DivideBy, add)
			}
			if m.MinStddev < 0 {
				add("%s.min_stddev não pode ser negativo", mp)
			}
			if m.Attribute != "" && !slices.Contains(attributeResources, m.Attribute) {
				add("%s.attribute inválido: %q (use %v)", mp, m.Attribute, attributeResources)
			}
			for _, l := range m.GroupBy {
				if !labelName.MatchString(l) {
					add("%s.group_by: label inválido %q", mp, l)
				}
			}
			if strings.ContainsAny(m.Key(), "{}") {
				add("%s: o nome da feature não pode conter chaves", mp)
			}
			if r := m.Rule; r != nil {
				n := 0
				for _, v := range []*float64{r.Above, r.Below, r.Equals} {
					if v != nil {
						n++
					}
				}
				if n != 1 {
					add("%s.rule: defina exatamente um entre above, below e equals", mp)
				}
				if !slices.Contains(severities, r.Severity) {
					add("%s.rule.severity inválida: %q", mp, r.Severity)
				}
			}
			if m.Describe != nil && !slices.Contains(Units, m.Describe.Unit) {
				add("%s.describe.unit inválida: %q (use %v)", mp, m.Describe.Unit, Units[1:])
			}
		}
	}
	return errors.Join(errs...)
}

func validateTargets(p string, targets []Target, add func(string, ...any)) {
	seen := map[string]bool{}
	for i, t := range targets {
		if t.Host == "" {
			add("%s[%d].host é obrigatório", p, i)
		} else if seen[t.Host] {
			add("%s[%d].host duplicado: %q", p, i, t.Host)
		}
		seen[t.Host] = true
		if u, err := url.Parse(t.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			add("%s[%d].url inválida: %q", p, i, t.URL)
		}
	}
}

func validateOperand(p string, o Operand, add func(string, ...any)) {
	if o.Name == "" {
		add("%s.name é obrigatório", p)
	}
	if o.Type != "" && o.Type != "counter" && o.Type != "gauge" && o.Type != "timestamp" {
		add("%s.type inválido: %q", p, o.Type)
	}
	for k, v := range o.Match {
		if _, err := regexp.Compile(v); err != nil {
			add("%s.match[%s]: %v", p, k, err)
		}
	}
	for k, v := range o.Exclude {
		if _, err := regexp.Compile(v); err != nil {
			add("%s.exclude[%s]: %v", p, k, err)
		}
	}
}
