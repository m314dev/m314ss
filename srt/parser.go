// Package srt reads and retimes SubRip subtitles.
//
// Only the timestamps are parsed. Cue text passes through untouched, so
// encoding doesn't matter.
package srt

import (
	"bufio"
	"bytes"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Cue is one subtitle interval, in seconds from the start of the video.
type Cue struct {
	Start float64
	End   float64
}

var tsRe = regexp.MustCompile(`(\d+):(\d{2}):(\d{2})[,.](\d{3})`)

// Parse extracts cue timings. Malformed lines are skipped, not fatal.
// Subtitles in the wild are full of small breakage.
func Parse(data []byte) []Cue {
	var cues []Cue
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.Contains(line, "-->") {
			continue
		}
		parts := strings.Split(line, "-->")
		if len(parts) != 2 {
			continue
		}
		s, ok1 := parseTime(parts[0])
		e, ok2 := parseTime(parts[1])
		if ok1 && ok2 {
			cues = append(cues, Cue{Start: s, End: e})
		}
	}
	return cues
}

// Starts returns just the cue start times.
func Starts(cues []Cue) []float64 {
	out := make([]float64, len(cues))
	for i, c := range cues {
		out[i] = c.Start
	}
	return out
}

// Retime turns every timestamp t into t*scale + shift, leaving text alone.
// Negative results clamp to zero.
func Retime(data []byte, scale, shift float64) []byte {
	return tsRe.ReplaceAllFunc(data, func(m []byte) []byte {
		t, _ := parseTime(string(m))
		return stamp(t*scale + shift)
	})
}

// RetimeParts is Retime with a shift per part of the file: a cue starting
// at t takes shifts[i] of the last from[i] <= t (from sorted, from[0] covers
// everything before from[1]). Both of a cue's timestamps move together, and
// only cue timing lines change.
func RetimeParts(data []byte, scale float64, from, shifts []float64) []byte {
	out := make([]byte, 0, len(data))
	for rest := data; len(rest) > 0; {
		n := bytes.IndexByte(rest, '\n') + 1
		if n == 0 {
			n = len(rest)
		}
		line := rest[:n]
		rest = rest[n:]
		parts := strings.Split(string(line), "-->")
		if len(parts) != 2 {
			out = append(out, line...)
			continue
		}
		start, ok1 := parseTime(parts[0])
		_, ok2 := parseTime(parts[1])
		if !ok1 || !ok2 { // same rule as Parse, so cues line up
			out = append(out, line...)
			continue
		}
		i := sort.SearchFloat64s(from, start+1e-9) - 1
		shift := shifts[max(i, 0)]
		out = append(out, tsRe.ReplaceAllFunc(line, func(m []byte) []byte {
			t, _ := parseTime(string(m))
			return stamp(t*scale + shift)
		})...)
	}
	return out
}

// stamp formats t as an SRT timestamp. Negative times clamp to zero.
func stamp(t float64) []byte {
	ms := int(max(t, 0)*1000 + 0.5)
	return fmt.Appendf(nil, "%02d:%02d:%02d,%03d",
		ms/3600000, ms/60000%60, ms/1000%60, ms%1000)
}

func parseTime(s string) (float64, bool) {
	m := tsRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, false
	}
	n := func(x string) float64 { v, _ := strconv.Atoi(x); return float64(v) }
	return n(m[1])*3600 + n(m[2])*60 + n(m[3]) + n(m[4])/1000, true
}
