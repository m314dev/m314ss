package subtitles

import (
	"math"
	"math/rand"
	"testing"
)

// Every offset within the tolerance of the true one explains the same cues,
// so the best hit rate is a plateau. Its first offset is up to a tolerance
// early, the answer is its middle.
func TestAlignPlateauMiddle(t *testing.T) {
	onsets := speech(rand.New(rand.NewSource(5)), 3000)

	const late = 4.7
	cues := make([]float64, len(onsets))
	for i, o := range onsets {
		cues[i] = o + late // exact, so the plateau is the whole tolerance band
	}

	a := Align(cues, onsets, nil)
	if math.Abs(a.Offset-(-late)) > step/2+1e-6 {
		t.Fatalf("offset = %+.3f, want %+.3f", a.Offset, -late)
	}
}
