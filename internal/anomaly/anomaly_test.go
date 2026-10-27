package anomaly

import (
	"math"
	"testing"
	"time"
)

func TestEWMAConstantSeries(t *testing.T) {
	b := EWMA{Alpha: 0.1}
	for range 100 {
		b.Update(5)
	}
	if b.Mean != 5 || b.Var != 0 {
		t.Fatalf("série constante: mean=%v var=%v, esperado 5 e 0", b.Mean, b.Var)
	}
}

func TestEWMAConvergesToDistribution(t *testing.T) {
	// Alterna 9 e 11: média 10, desvio-padrão 1.
	b := EWMA{Alpha: 0.02}
	for i := range 5000 {
		b.Update(10 + float64(i%2*2-1))
	}
	if math.Abs(b.Mean-10) > 0.05 {
		t.Errorf("mean=%v, esperado ~10", b.Mean)
	}
	if math.Abs(b.StdDev()-1) > 0.05 {
		t.Errorf("stddev=%v, esperado ~1", b.StdDev())
	}
}

func TestEWMAAdaptsToLevelShift(t *testing.T) {
	b := EWMA{Alpha: AlphaFor(15*time.Second, time.Minute)}
	for range 50 {
		b.Update(1)
	}
	for range 50 {
		b.Update(3)
	}
	if math.Abs(b.Mean-3) > 0.01 {
		t.Errorf("após mudança de nível mean=%v, esperado ~3", b.Mean)
	}
}

func TestAlphaFor(t *testing.T) {
	a1 := AlphaFor(15*time.Second, time.Minute)
	a15 := AlphaFor(15*time.Second, 15*time.Minute)
	if !(a1 > a15 && a15 > 0 && a1 < 1) {
		t.Fatalf("alphas inválidos: 1m=%v 15m=%v", a1, a15)
	}
	if want := 1 - math.Exp(-0.25); math.Abs(a1-want) > 1e-12 {
		t.Errorf("AlphaFor(15s,1m)=%v, esperado %v", a1, want)
	}
}

func TestZScore(t *testing.T) {
	cases := []struct{ x, mean, sd, want float64 }{
		{0.41, 0.12, 0.03, 9.666666666666666},
		{7, 10, 1, -3},
		{10, 10, 1, 0},
		{5, 1, 0, 0},
	}
	for _, c := range cases {
		if got := ZScore(c.x, c.mean, c.sd); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("ZScore(%v,%v,%v)=%v, esperado %v", c.x, c.mean, c.sd, got, c.want)
		}
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		z    float64
		want Severity
	}{
		{0, SeverityNone}, {2.99, SeverityNone}, {3, SeverityLow}, {3.9, SeverityLow},
		{4, SeverityMedium}, {5.9, SeverityMedium}, {6, SeverityHigh}, {9.9, SeverityHigh},
		{10, SeverityCritical}, {50, SeverityCritical},
	}
	for _, c := range cases {
		if got := Classify(c.z, 3); got != c.want {
			t.Errorf("Classify(%v)=%q, esperado %q", c.z, got, c.want)
		}
	}
}

func TestScoreMonotonic(t *testing.T) {
	prev := -1.0
	for z := 0.0; z <= 20; z += 0.5 {
		s := Score(z)
		if s < prev || s < 0 || s > 1 {
			t.Fatalf("Score(%v)=%v não é monotônico em [0,1]", z, s)
		}
		prev = s
	}
}

func params() Params {
	return Params{
		Interval:       15 * time.Second,
		Windows:        []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute},
		DecisionWindow: 5 * time.Minute,
		Threshold:      3,
		Consecutive:    3,
		Cooldown:       5 * time.Minute,
		Warmup:         20,
		MinStddevRatio: 0.01,
		FreezeLimit:    10,
	}
}

// feed alimenta o detector com ruído determinístico em torno de 1.0.
func feed(d *Detector, start time.Time, n int) time.Time {
	ts := start
	for i := range n {
		d.Observe(1+0.05*math.Sin(float64(i)), ts)
		ts = ts.Add(15 * time.Second)
	}
	return ts
}

func TestDetectorLearningPhase(t *testing.T) {
	d := NewDetector(params())
	ts := time.Unix(0, 0)
	for i := range 19 {
		r := d.Observe(100, ts.Add(time.Duration(i)*15*time.Second))
		if !r.Learning || r.Anomalous || r.Emit {
			t.Fatalf("ponto %d: esperado aprendizado sem anomalia, obtido %+v", i, r)
		}
	}
}

func TestDetectorRequiresConsecutivePoints(t *testing.T) {
	d := NewDetector(params())
	ts := feed(d, time.Unix(0, 0), 60)

	r1 := d.Observe(5, ts)
	r2 := d.Observe(5, ts.Add(15*time.Second))
	if !r1.Anomalous || !r2.Anomalous {
		t.Fatalf("pico deveria ser anômalo: %+v %+v", r1, r2)
	}
	if r1.Emit || r2.Emit {
		t.Fatal("evento emitido antes de 3 pontos consecutivos")
	}
	r3 := d.Observe(5, ts.Add(30*time.Second))
	if !r3.Emit || r3.Streak != 3 {
		t.Fatalf("esperado evento no 3º ponto, obtido %+v", r3)
	}
	if r3.ZScore <= 3 || r3.Severity == SeverityNone {
		t.Fatalf("z-score/severidade inesperados: %+v", r3)
	}
}

func TestDetectorStreakResets(t *testing.T) {
	d := NewDetector(params())
	ts := feed(d, time.Unix(0, 0), 60)
	d.Observe(5, ts)
	d.Observe(5, ts.Add(15*time.Second))
	d.Observe(1, ts.Add(30*time.Second)) // volta ao normal
	r := d.Observe(5, ts.Add(45*time.Second))
	if r.Emit || r.Streak != 1 {
		t.Fatalf("sequência deveria ter reiniciado, obtido %+v", r)
	}
}

func TestDetectorCooldown(t *testing.T) {
	p := params()
	p.Consecutive = 1
	d := NewDetector(p)
	ts := feed(d, time.Unix(0, 0), 60)

	if r := d.Observe(5, ts); !r.Emit {
		t.Fatalf("primeiro pico deveria emitir: %+v", r)
	}
	if r := d.Observe(50, ts.Add(time.Minute)); !r.Anomalous || r.Emit {
		t.Fatalf("pico dentro do cooldown não deveria emitir: %+v", r)
	}
	if r := d.Observe(500, ts.Add(5*time.Minute)); !r.Emit {
		t.Fatalf("pico após o cooldown deveria emitir: %+v", r)
	}
}

func TestDetectorCancelEmit(t *testing.T) {
	p := params()
	p.Consecutive = 1
	d := NewDetector(p)
	ts := feed(d, time.Unix(0, 0), 60)

	if r := d.Observe(5, ts); !r.Emit {
		t.Fatalf("primeiro pico deveria emitir: %+v", r)
	}
	d.CancelEmit() // o evento não foi gravado
	if r := d.Observe(50, ts.Add(time.Minute)); !r.Emit {
		t.Fatalf("após CancelEmit o cooldown não deveria suprimir: %+v", r)
	}
	if r := d.Observe(60, ts.Add(2*time.Minute)); r.Emit {
		t.Fatalf("emissão confirmada deveria iniciar o cooldown: %+v", r)
	}

	rd := NewRuleDetector(Rule{Op: "equals", Value: 1, Severity: SeverityCritical}, 1, 5*time.Minute)
	if r := rd.Observe(0, ts); !r.Emit {
		t.Fatalf("regra violada deveria emitir: %+v", r)
	}
	rd.CancelEmit()
	if r := rd.Observe(0, ts.Add(time.Minute)); !r.Emit {
		t.Fatalf("após CancelEmit a regra deveria emitir de novo: %+v", r)
	}
}

// Reproduz o achado do teste real: CPU saltando de ~0,8 para 12 núcleos
// saía como "low" porque os próprios pontos do pico inflavam o desvio.
func TestSpikeDoesNotInflateBaseline(t *testing.T) {
	d := NewDetector(params())
	ts := feed(d, time.Unix(0, 0), 60)
	before := d.State()

	var rs []Result
	for i := range 3 {
		rs = append(rs, d.Observe(12, ts.Add(time.Duration(i)*15*time.Second)))
	}
	if !rs[2].Emit {
		t.Fatalf("pico sustentado por 3 pontos deveria emitir: %+v", rs[2])
	}
	if rs[2].ZScore != rs[0].ZScore {
		t.Errorf("z-score caiu durante o pico: 1º=%v 3º=%v", rs[0].ZScore, rs[2].ZScore)
	}
	if rs[2].Severity != SeverityCritical {
		t.Errorf("salto de ~1 para 12 deveria ser critical, obtido %q (z=%v)", rs[2].Severity, rs[2].ZScore)
	}
	if after := d.State(); after.Baselines[1] != before.Baselines[1] {
		t.Error("pontos anômalos não deveriam alterar a baseline durante o congelamento")
	}
}

func TestPersistentLevelShiftIsLearned(t *testing.T) {
	d := NewDetector(params())
	ts := feed(d, time.Unix(0, 0), 60)
	var r Result
	for i := range 200 {
		r = d.Observe(5+0.25*math.Sin(float64(i)), ts.Add(time.Duration(i)*15*time.Second))
		if i < 10 && !r.BaselineFrozen {
			t.Fatalf("ponto %d do novo patamar deveria ficar fora da baseline", i)
		}
	}
	if r.Anomalous {
		t.Fatalf("após 50 min no novo patamar ele deveria ser o normal: %+v", r)
	}
	if math.Abs(r.Mean-5) > 0.2 {
		t.Fatalf("baseline deveria convergir para ~5, obtido %v", r.Mean)
	}
}

func TestDetectorDetectsDrop(t *testing.T) {
	d := NewDetector(params())
	ts := feed(d, time.Unix(0, 0), 60)
	r := d.Evaluate(0.2, ts)
	if !r.Anomalous || r.ZScore >= 0 {
		t.Fatalf("queda deveria ser anômala com z negativo: %+v", r)
	}
}

func TestEvaluateDoesNotMutate(t *testing.T) {
	d := NewDetector(params())
	ts := feed(d, time.Unix(0, 0), 60)
	before := d.State()
	for range 10 {
		d.Evaluate(100, ts)
	}
	after := d.State()
	for i := range before.Baselines {
		if before.Baselines[i] != after.Baselines[i] {
			t.Fatal("Evaluate alterou a baseline")
		}
	}
}

func TestStddevFloor(t *testing.T) {
	d := NewDetector(params())
	ts := time.Unix(0, 0)
	for i := range 30 {
		d.Observe(100, ts.Add(time.Duration(i)*15*time.Second))
	}
	// Série constante: piso = 1% de 100 = 1, então 101 está a 1σ, não a infinito.
	r := d.Evaluate(101, ts)
	if math.Abs(r.ZScore-1) > 1e-9 || r.Anomalous {
		t.Fatalf("piso de desvio-padrão não aplicado: %+v", r)
	}
}

func TestStateRestore(t *testing.T) {
	d := NewDetector(params())
	feed(d, time.Unix(0, 0), 60)
	d2 := NewDetector(params())
	if !d2.Restore(d.State()) {
		t.Fatal("Restore com mesmos parâmetros deveria funcionar")
	}
	if d2.State().Baselines[1] != d.State().Baselines[1] {
		t.Fatal("estado restaurado difere")
	}
	p := params()
	p.Interval = 30 * time.Second
	if NewDetector(p).Restore(d.State()) {
		t.Fatal("Restore com intervalo diferente deveria ser rejeitado")
	}
}

func TestFormatWindow(t *testing.T) {
	cases := map[time.Duration]string{
		time.Minute: "1m", 15 * time.Minute: "15m", 90 * time.Second: "1m30s",
		time.Hour: "1h", 90 * time.Minute: "1h30m",
	}
	for d, want := range cases {
		if got := FormatWindow(d); got != want {
			t.Errorf("FormatWindow(%v)=%q, esperado %q", d, got, want)
		}
	}
}

func TestRuleDetector(t *testing.T) {
	d := NewRuleDetector(Rule{Op: "equals", Value: 1, Severity: SeverityCritical}, 3, 5*time.Minute)
	ts := time.Unix(0, 0)
	if r := d.Observe(1, ts); r.Anomalous || r.Learning || r.Detector != DetectorRule {
		t.Fatalf("valor esperado não deveria violar a regra: %+v", r)
	}
	var r Result
	for i := 1; i <= 3; i++ {
		r = d.Observe(0, ts.Add(time.Duration(i)*15*time.Second))
		if (i < 3) == r.Emit {
			t.Fatalf("ponto %d: emit=%v", i, r.Emit)
		}
	}
	if r.Severity != SeverityCritical || r.Score != 1 || r.Mean != 1 || r.Rule != "equals 1" {
		t.Fatalf("resultado da regra incorreto: %+v", r)
	}
	if r := d.Observe(0, ts.Add(time.Minute)); r.Emit {
		t.Fatal("cooldown deveria impedir novo evento")
	}
	if r := d.Observe(1, ts.Add(2*time.Minute)); r.Anomalous || r.Streak != 0 {
		t.Fatalf("recuperação deveria zerar a sequência: %+v", r)
	}
}

func TestRuleOps(t *testing.T) {
	cases := []struct {
		r    Rule
		x    float64
		want bool
	}{
		{Rule{Op: "above", Value: 0.9}, 0.95, true}, {Rule{Op: "above", Value: 0.9}, 0.9, false},
		{Rule{Op: "below", Value: 1}, 0, true}, {Rule{Op: "below", Value: 1}, 1, false},
		{Rule{Op: "equals", Value: 1}, 0, true}, {Rule{Op: "equals", Value: 1}, 1, false},
	}
	for _, c := range cases {
		if got := c.r.Violated(c.x); got != c.want {
			t.Errorf("%s com %v: %v, esperado %v", c.r, c.x, got, c.want)
		}
	}
}

func TestAbsoluteStddevFloor(t *testing.T) {
	p := params()
	p.MinStddev = 0.5
	d := NewDetector(p)
	ts := time.Unix(0, 0)
	for i := range 30 {
		d.Observe(4, ts.Add(time.Duration(i)*15*time.Second)) // 4 containers, constante
	}
	if r := d.Evaluate(3, ts); r.Anomalous || r.ZScore != -2 {
		t.Fatalf("1 container a menos com piso 0,5 deveria dar z=-2 sem anomalia: %+v", r)
	}
	if r := d.Evaluate(1, ts); !r.Anomalous {
		t.Fatalf("3 containers a menos deveria ser anômalo: %+v", r)
	}
}
