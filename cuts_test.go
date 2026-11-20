package subtitles

import (
	"bytes"
	"fmt"
	"math"
	"math/rand"
	"testing"
)

// A subtitle timed on a broadcast with a 75 s ad break at 1400 s: after the
// break every cue is 75 s later than the speech.
func adBreak(rng *rand.Rand, onsets []float64, late, at, gap float64) []float64 {
	var cues []float64
	for _, o := range onsets {
		if rng.Float64() < 0.75 {
			c := o + late + rng.NormFloat64()*0.08
			if o >= at {
				c += gap
			}
			cues = append(cues, c)
		}
	}
	return cues
}

func checkParts(t *testing.T, parts []Part, late, at, gap float64) {
	t.Helper()
	if len(parts) != 2 {
		t.Fatalf("got %d parts %+v, want 2", len(parts), parts)
	}
	if math.Abs(parts[0].Offset+late) > 0.1 || math.Abs(parts[1].Offset+late+gap) > 0.1 {
		t.Fatalf("offsets %+.2f %+.2f, want %+.2f %+.2f",
			parts[0].Offset, parts[1].Offset, -late, -late-gap)
	}
	// the first cue after the break, wherever the subtitle put it
	if parts[1].From < at+late+gap-0.5 || parts[1].From > at+late+gap+10 {
		t.Fatalf("second part from %.1f, want just after %.1f", parts[1].From, at+late+gap)
	}
}

func TestSplitAdBreak(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	onsets := speech(rng, 3000)
	const late, at, gap = 2.3, 1400.0, 75.0
	cues := adBreak(rng, onsets, late, at, gap)

	a := Align(cues, onsets, nil)
	if !a.Matched() {
		t.Fatalf("sigma %.2f, the bigger part alone should pass", a.Sigma())
	}
	checkParts(t, partsOf(split(cues, a, onsets, nil, MinPartSigma, true)), late, at, gap)
}

// With only the sampled windows decoded, the break can fall where no audio
// was looked at. The cue gap the break left still has to be where the
// offset changes: nowhere else can a cue move 75 s earlier without passing
// the one before it.
func TestSplitAdBreakSampled(t *testing.T) {
	rng := rand.New(rand.NewSource(12))
	onsets := speech(rng, 3000)
	windows := pickWindows(3000)
	at := windows[4][0] + windows[4][1] + 60 // between two windows
	const late, gap = -4.1, 75.0
	cues := adBreak(rng, onsets, late, at, gap)

	a := Align(cues, onsets, windows)
	if !a.Matched() {
		t.Fatalf("sigma %.2f, the bigger part alone should pass", a.Sigma())
	}
	checkParts(t, partsOf(split(cues, a, onsets, windows, MinPartSigma, true)), late, at, gap)
}

// A plain shift, a drift and a short subtitle must stay one part, or a good
// file gets chopped up.
func TestSplitLeavesWholeFilesAlone(t *testing.T) {
	rng := rand.New(rand.NewSource(13))
	onsets := speech(rng, 3000)
	var shifted, drifted []float64
	for _, o := range onsets {
		if rng.Float64() < 0.75 {
			shifted = append(shifted, o+37.4+rng.NormFloat64()*0.08)
			drifted = append(drifted, o*24/25+12+rng.NormFloat64()*0.08)
		}
	}
	for name, cues := range map[string][]float64{"shift": shifted, "drift": drifted} {
		a := Align(cues, onsets, nil)
		if parts := partsOf(split(cues, a, onsets, nil, MinPartSigma, true)); parts != nil {
			t.Errorf("%s: got parts %+v", name, parts)
		}
	}
}

// Breaks run longer than the ±120 s the whole file is searched over: parts
// are searched further out.
func TestSplitLongBreak(t *testing.T) {
	rng := rand.New(rand.NewSource(14))
	onsets := speech(rng, 3000)
	const late, at, gap = 3.0, 1200.0, 170.0
	cues := adBreak(rng, onsets, late, at, gap)
	a := Align(cues, onsets, nil)
	checkParts(t, partsOf(split(cues, a, onsets, nil, MinPartSigma, true)), late, at, gap)
}

func toSRT(cues []float64) []byte {
	var b []byte
	for i, c := range cues {
		b = fmt.Appendf(b, "%d\n%s --> %s\nx\n\n", i+1, srtTime(c), srtTime(c+1.5))
	}
	return b
}

func srtTime(t float64) string {
	ms := int(t*1000 + 0.5)
	return fmt.Sprintf("%02d:%02d:%02d,%03d", ms/3600000, ms/60000%60, ms/1000%60, ms%1000)
}

// Under the gate as a whole, a subtitle passes if every part passes on its
// own cues. A wrong subtitle must not, however it is cut up. The gate is
// raised here so that the cut file fails it as a whole.
func TestMatchRescue(t *testing.T) {
	defer func(s float64) { MinSigma = s }(MinSigma)
	MinSigma = 30

	rng := rand.New(rand.NewSource(15))
	onsets := speech(rng, 3000)
	s := &Speech{Onsets: onsets}
	cut := adBreak(rng, onsets, 1.5, 1500, 60)
	res, out, err := s.Match(toSRT(cut))
	if err != nil || !res.Matched() || len(res.Parts) != 2 {
		t.Fatalf("cut file: err %v, matched %v (sigma %.2f), parts %+v",
			err, res.Matched(), res.Sigma(), res.Parts)
	}
	if !res.Shifted || bytes.Equal(out, toSRT(cut)) {
		t.Fatal("cut file was not retimed")
	}

	for seed := int64(100); seed < 105; seed++ {
		junk := speech(rand.New(rand.NewSource(seed)), 3000)
		if res, _, _ := s.Match(toSRT(junk)); res.Matched() {
			t.Fatalf("junk %d rescued: sigma %.2f, parts %+v", seed, res.Sigma(), res.Parts)
		}
	}
}

// A last stretch of cues with no speech under it (credits past the end of
// the audio, say) can come out as its own weak part. It has to join its
// neighbour, not take the real parts down with it.
func TestSplitWeakTailJoins(t *testing.T) {
	rng := rand.New(rand.NewSource(16))
	onsets := speech(rng, 3000)
	const late, at, gap = 0.5, 1500.0, 80.0
	cues := adBreak(rng, onsets, late, at, gap)
	for x := 3000.0; x < 3300; x += 3 + rng.Float64()*4 { // past the audio
		cues = append(cues, x+late+gap)
	}
	a := Align(cues, onsets, nil)
	checkParts(t, partsOf(split(cues, a, onsets, nil, MinPartSigma, true)), late, at, gap)
}

func partsOf(p []Part, _ float64) []Part { return p }

// cut finds the same best gain as trying every cue and every pair of offsets
// MinCutJump to MaxCutJump apart.
func TestCutBruteForce(t *testing.T) {
	defer func(n int, j float64) { partSteps, MaxCutJump = n, j }(partSteps, MaxCutJump)
	partSteps, MaxCutJump = 61, 6
	jmin := int(math.Ceil(MinCutJump/partStep - 1e-9))
	jmax := int(math.Floor(MaxCutJump/partStep + 1e-9))
	rng := rand.New(rand.NewSource(7))
	for trial := range 20 {
		n := 45 + rng.Intn(90)
		g := &grid{n: n, words: (n + 63) / 64, chance: 0.2 + 0.1*rng.Float64()}
		g.cov = make([]uint64, partSteps*g.words)
		g.hit = make([]uint64, partSteps*g.words)
		for k := range partSteps {
			for i := range n {
				if rng.Float64() < 0.8 {
					g.cov[k*g.words+i/64] |= 1 << (i % 64)
					if rng.Float64() < 0.25 {
						g.hit[k*g.words+i/64] |= 1 << (i % 64)
					}
				}
			}
		}
		cues := make([]float64, n) // far apart: no cut reorders them
		for i := range cues {
			cues[i] = float64(i) * 100
		}
		lo, hi := rng.Intn(3), n-rng.Intn(3)
		_, whole := g.best(lo, hi)
		want := math.Inf(-1)
		for x := lo + minPart; x <= hi-minPart; x++ {
			for a := range partSteps {
				for c := range partSteps {
					if d := max(a-c, c-a); d >= jmin && d <= jmax {
						want = max(want, g.excess(a, lo, x)+g.excess(c, x, hi)-whole-CutSwitchCost)
					}
				}
			}
		}
		b, kl, kr, gain := g.cut(cues, lo, hi)
		if math.Abs(gain-want) > 1e-9 {
			t.Fatalf("trial %d: gain %.3f, want %.3f", trial, gain, want)
		}
		if d := max(kl-kr, kr-kl); d < jmin || d > jmax {
			t.Fatalf("trial %d: offsets %d and %d not %d to %d apart", trial, kl, kr, jmin, jmax)
		}
		if got := g.excess(kl, lo, b) + g.excess(kr, b, hi) - whole - CutSwitchCost; math.Abs(got-gain) > 1e-9 {
			t.Fatalf("trial %d: cut at %d explains %.3f, says %.3f", trial, b, got, gain)
		}
	}
}
