package anomaly

import (
	"math"
	"testing"
)

func TestDetectSpike(t *testing.T) {
	// baseline mean 100, stddev 10; usage 135 = 3.5σ above → spike.
	r := Detect(135, 100, 10)
	if !r.Anomalous || r.Kind != KindSpike {
		t.Fatalf("Detect(135,100,10) = %+v, want SPIKE", r)
	}
	if r.Score < SpikeThreshold {
		t.Fatalf("spike score = %v, want >= %v", r.Score, SpikeThreshold)
	}
}

func TestDetectDrop(t *testing.T) {
	r := Detect(65, 100, 10) // 3.5σ below
	if !r.Anomalous || r.Kind != KindDrop {
		t.Fatalf("Detect(65,100,10) = %+v, want DROP", r)
	}
}

func TestDetectWithinBand(t *testing.T) {
	r := Detect(105, 100, 10) // 0.5σ
	if r.Anomalous || r.Kind != KindNone {
		t.Fatalf("Detect(105,100,10) = %+v, want NONE", r)
	}
}

func TestDetectFlatBaselineSpike(t *testing.T) {
	// stddev 0: z-score undefined. A jump from a constant stream IS the anomaly.
	r := Detect(150, 100, 0)
	if !r.Anomalous || r.Kind != KindSpike {
		t.Fatalf("flat-baseline jump = %+v, want SPIKE", r)
	}
	if math.IsInf(r.Score, 0) {
		t.Fatal("flat-baseline score must be finite, not ±Inf")
	}
}

func TestDetectFlatBaselineEqual(t *testing.T) {
	r := Detect(100, 100, 0)
	if r.Anomalous {
		t.Fatalf("point equal to a flat baseline = %+v, want NONE", r)
	}
}

func TestBaselineWelford(t *testing.T) {
	var b Baseline
	for _, x := range []float64{2, 4, 4, 4, 5, 5, 7, 9} {
		b.Add(x)
	}
	// mean 5; sample variance = Σ(x−5)²/(n−1) = (9+3+0+4+16)/7 = 32/7.
	if math.Abs(b.Mean-5) > 1e-9 {
		t.Fatalf("mean = %v, want 5", b.Mean)
	}
	if math.Abs(b.Variance()-32.0/7.0) > 1e-9 {
		t.Fatalf("variance = %v, want 32/7", b.Variance())
	}
	if math.Abs(b.StdDev()-math.Sqrt(32.0/7.0)) > 1e-9 {
		t.Fatalf("stddev = %v, want sqrt(32/7)", b.StdDev())
	}
}

func TestBaselineNeedsTwoSamples(t *testing.T) {
	var b Baseline
	b.Add(5)
	if b.StdDev() != 0 {
		t.Fatal("a one-sample baseline must report stddev 0 (undefined), not NaN")
	}
}

func TestScanSpikeInSeries(t *testing.T) {
	// steady ~100 usage, then a 300 spike as the final point.
	series := []float64{100, 102, 98, 101, 99, 300}
	r := Scan(series)
	if !r.Anomalous || r.Kind != KindSpike {
		t.Fatalf("Scan spike = %+v, want SPIKE", r)
	}
}

func TestScanFlatSeriesIsClean(t *testing.T) {
	r := Scan([]float64{100, 100, 100, 100})
	if r.Anomalous {
		t.Fatalf("a flat series with no final deviation = %+v, want NONE", r)
	}
}
