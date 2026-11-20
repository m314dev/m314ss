package subtitles

import (
	"fmt"
	"testing"
)

func TestSameTiming(t *testing.T) {
	file := func(starts ...float64) []byte {
		var b []byte
		for i, s := range starts {
			ts := func(x float64) string {
				ms := int(x*1000 + 0.5)
				return fmt.Sprintf("%02d:%02d:%02d,%03d", ms/3600000, ms/60000%60, ms/1000%60, ms%1000)
			}
			b = fmt.Appendf(b, "%d\n%s --> %s\nline\n\n", i+1, ts(s), ts(s+1.5))
		}
		return b
	}
	var orig, merged, other []float64
	for i := range 100 {
		orig = append(orig, 10+7.3*float64(i))
		if i%10 != 3 { // a translator merging every tenth line into the one before
			merged = append(merged, 10+7.3*float64(i))
		}
		other = append(other, 12+6.1*float64(i)) // another episode
	}
	if !sameTiming(file(merged...), file(orig...)) {
		t.Error("a translation with merged lines should keep the original's timing")
	}
	if sameTiming(file(other...), file(orig...)) {
		t.Error("another file's cues passed as the original's")
	}
	if sameTiming(nil, file(orig...)) {
		t.Error("an empty translation passed")
	}
}
