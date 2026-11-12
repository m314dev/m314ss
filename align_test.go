package subtitles

import (
	"math"
	"math/rand"
	"testing"
)

// irregular gaps on purpose, a periodic timeline ties at every multiple of
// its period
func speech(rng *rand.Rand, until float64) []float64 {
	var onsets []float64
	for x := 5.0; x < until; x += 2 + rng.Float64()*8 {
		onsets = append(onsets, x)
	}
	return onsets
}

func TestAlign(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	onsets := speech(rng, 3000)

	const late = 37.4
	var cues []float64
	for _, o := range onsets {
		if rng.Float64() < 0.75 { // real subtitles don't caption every line
			cues = append(cues, o+late+rng.NormFloat64()*0.08)
		}
	}

	a := Align(cues, onsets, nil)
	if math.Abs(a.Offset-(-late)) > 0.1 {
		t.Fatalf("offset = %+.2f, want %+.2f", a.Offset, -late)
	}
	if !a.Matched() {
		t.Fatalf("sigma %.2f (hit %.2f, base %.2f) should have cleared %.2f",
			a.Sigma(), a.HitRate, a.Baseline, MinSigma)
	}

	// unrelated cues have to fail the gate or a wrong subtitle gets written
	// over a good one. raw hit rate looks fine here, only the lift gives it
	// away.
	junk := speech(rand.New(rand.NewSource(99)), 3000)
	if b := Align(junk, onsets, nil); b.Matched() {
		t.Fatalf("unrelated cues passed: sigma %.2f (hit %.2f, base %.2f)",
			b.Sigma(), b.HitRate, b.Baseline)
	}
}

// Cues outside the sampled windows must not count as misses, we never looked
// at that audio. Getting this wrong scored a known good film at 12% instead
// of 58%.
func TestAlignIgnoresUnsampledAudio(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	onsets := speech(rng, 600)
	windows := [][2]float64{{0, 600}}

	cues := append([]float64(nil), onsets...)
	for x := 700.0; x < 3000; x += 4 { // never sampled
		cues = append(cues, x)
	}

	a := Align(cues, onsets, windows)
	if !a.Matched() {
		t.Fatalf("sigma %.2f: unsampled cues were counted as misses", a.Sigma())
	}
	// exact synthetic hits tie across the whole tolerance band, so anything
	// inside it is right here
	if math.Abs(a.Offset) > tolerance {
		t.Fatalf("offset = %+.2f, want within %.2f of 0", a.Offset, tolerance)
	}
}

// A subtitle timed for a 25fps cut, played on a 24fps one. Every cue is a
// little later than the last, 120s out by the end. A plain offset search
// can't explain that, the rate ratio has to be found.
func TestAlignDrift(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	onsets := speech(rng, 3000)

	const late = 12.0
	var cues []float64
	for _, o := range onsets {
		if rng.Float64() < 0.75 {
			cues = append(cues, o*24/25+late+rng.NormFloat64()*0.08)
		}
	}

	a := Align(cues, onsets, nil)
	if a.Scale != 25.0/24 {
		t.Fatalf("scale = %v (sigma %.2f), want 25/24", a.Scale, a.Sigma())
	}
	if want := -late * 25 / 24; math.Abs(a.Offset-want) > 0.1 {
		t.Fatalf("offset = %+.2f, want %+.2f", a.Offset, want)
	}
	if !a.Matched() {
		t.Fatalf("sigma %.2f should have cleared %.2f", a.Sigma(), MinSigma)
	}
}

func TestToUTF8(t *testing.T) {
	for _, c := range []struct {
		name string
		in   []byte
	}{
		{"windows-1256", []byte{0xC7, 0xE1, 0xD3, 0xE1, 0xC7, 0xE3}},
		{"utf-8", []byte("السلام")},
		{"utf-16le", []byte{0xFF, 0xFE, 0x27, 0x06, 0x44, 0x06, 0x33, 0x06, 0x44, 0x06, 0x27, 0x06, 0x45, 0x06}},
		{"utf-16be", []byte{0xFE, 0xFF, 0x06, 0x27, 0x06, 0x44, 0x06, 0x33, 0x06, 0x44, 0x06, 0x27, 0x06, 0x45}},
	} {
		if got := toUTF8(c.in, "ar"); got != "السلام" {
			t.Errorf("%s: got %q", c.name, got)
		}
	}
}
