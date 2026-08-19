// Package anomaly implements usage anomaly detection — 异常用量检测 (deferred
// from phase 2 per 09§4.0/§4.4 B7 "异常用量检测未在二期实装, 落三期"; phase-3
// D-2). It is the signal svc-metering surfaces to alert-center so a tenant's
// usage spike/drop reaches the customer through the same channel as any other
// alert.
//
// The detector is a z-score over a rolling baseline: a point whose distance
// from the trailing window's mean exceeds SpikeThreshold standard deviations is
// anomalous — a spike above, a drop below. Two things are non-obvious and are
// therefore encoded, not left to the caller:
//
//   - A FLAT baseline (stddev 0) makes a z-score undefined (division by zero).
//     A jump from a perfectly constant stream is precisely the anomaly signal,
//     so any deviation from a flat baseline is reported as anomalous, with the
//     score capped at a sentinel rather than ±Inf.
//   - The baseline must never contain the point under test: testing a point
//     against a window that already includes it would dilute its own signal
//     (a spike raises the mean it is compared against). The caller feeds the
//     window, the detector tests the point.
package anomaly

import (
	"math"
)

// SpikeThreshold is the z-score magnitude that marks a point anomalous. 3σ is
// the conventional outlier threshold; the sign separates a spike (+) from a
// drop (−).
const SpikeThreshold = 3.0

// flatBaselineScore is the sentinel score for a deviation from a flat baseline
// (stddev 0), where a z-score is mathematically undefined. A point that differs
// from a constant stream is the clearest anomaly there is, so it always reports
// anomalous, with a finite score instead of ±Inf.
const flatBaselineScore = 100.0

// Kind classifies an anomaly.
type Kind string

const (
	// KindSpike is usage above baseline (突增).
	KindSpike Kind = "SPIKE"
	// KindDrop is usage below baseline (突降).
	KindDrop Kind = "DROP"
	// KindNone is within the baseline band.
	KindNone Kind = "NONE"
)

// Result is a detection verdict.
type Result struct {
	Anomalous bool
	Kind      Kind
	// Score is the z-score (spikes positive, drops negative); capped at
	// flatBaselineScore for the flat-baseline case so callers never see ±Inf.
	Score float64
}

// Detect tests one usage point against a trailing baseline (mean, stddev).
// The baseline is the WINDOW, never including the point under test.
func Detect(usage, mean, stddev float64) Result {
	if stddev <= 0 {
		// Flat baseline: a z-score is undefined. Any difference is an anomaly.
		if usage > mean {
			return Result{Anomalous: true, Kind: KindSpike, Score: flatBaselineScore}
		}
		if usage < mean {
			return Result{Anomalous: true, Kind: KindDrop, Score: -flatBaselineScore}
		}
		return Result{Anomalous: false, Kind: KindNone, Score: 0}
	}
	z := (usage - mean) / stddev
	switch {
	case z >= SpikeThreshold:
		return Result{Anomalous: true, Kind: KindSpike, Score: z}
	case z <= -SpikeThreshold:
		return Result{Anomalous: true, Kind: KindDrop, Score: z}
	default:
		return Result{Anomalous: false, Kind: KindNone, Score: z}
	}
}

// Baseline is a streaming mean/variance accumulator (Welford's algorithm), so a
// metering worker can keep a per-tenant baseline without storing the whole
// window. It is safe for a single goroutine; share it via a lock, not by hope.
type Baseline struct {
	N    int
	Mean float64
	M2   float64 // sum of squared differences from the mean (Welford)
}

// Add folds one sample into the baseline.
func (b *Baseline) Add(x float64) {
	b.N++
	delta := x - b.Mean
	b.Mean += delta / float64(b.N)
	b.M2 += delta * (x - b.Mean)
}

// Variance returns the sample variance (M2 / (N−1)); 0 when fewer than 2 samples.
func (b Baseline) Variance() float64 {
	if b.N < 2 {
		return 0
	}
	return b.M2 / float64(b.N-1)
}

// StdDev returns the sample standard deviation; 0 when fewer than 2 samples.
func (b Baseline) StdDev() float64 { return math.Sqrt(b.Variance()) }

// Scan tests a series against the baseline formed by the series itself, and
// returns the detection for the LAST point (the convention: the final point is
// "now", everything before it is the window). It is the convenience a scan
// endpoint uses: feed the recent window + the latest sample, get a verdict.
func Scan(series []float64) Result {
	if len(series) < 2 {
		return Result{}
	}
	window := series[:len(series)-1]
	latest := series[len(series)-1]
	var b Baseline
	for _, x := range window {
		b.Add(x)
	}
	return Detect(latest, b.Mean, b.StdDev())
}
