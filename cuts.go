package subtitles

import (
	"fmt"
	"math"
	"math/bits"
	"runtime"
	"sort"
	"sync"
)

// A subtitle timed on another cut of the video is right in parts: an ad
// break in the broadcast it was timed on, or a scene this video doesn't
// have, moves everything after it by the length of that part. One offset
// fixes the biggest part and leaves the rest wrong, so split looks for the
// parts.
//
// The parts are contiguous runs of cues, so split cuts the subtitle in two
// at every cue and aligns each side on its own cues, over every offset in
// partRange: the cut where the two sides explain the most cues beyond what
// one offset does is the break. Each side is then cut again the same way,
// up to maxParts. Aligning a side on its own cues is what makes a part
// show: searched among all the cues, a part a third of the subtitle long is
// a third of a peak.
//
// A cut costs CutSwitchCost hits, twice if it moves a cue before the one
// ahead of it. So a cut to an earlier offset (the broadcast had an ad
// break) goes to the gap the break left in the subtitle.
//
// Each side of a cut has to pass MinPartSigma on its own cues and the
// strongest part MinTopPartSigma, which is what keeps a wrong subtitle from
// being stitched together out of lucky stretches.

// Part is one stretch of a subtitle with its own offset: cues starting at
// From or later (subtitle time, before scaling) move by Offset. Sigma is how
// far that offset stands out for this part's cues alone, like
// Alignment.Sigma.
type Part struct {
	From   float64
	Offset float64
	Sigma  float64
}

// The cut search knobs. Vars so they can be retuned, like the gates.
var (
	// CutProbeZ: on the sampled audio, two sides that each stand this far
	// out on their own cues are enough to suspect parts and decode the rest
	// of the audio.
	CutProbeZ = 2.5

	// CutSwitchCost is how many hits beyond chance a cut has to earn.
	CutSwitchCost = 8.0

	// MinPartSigma: both sides of a cut have to stand out this far on
	// their own cues.
	MinPartSigma = 5.0

	// MinTopPartSigma: and the strongest part this far, the gate a whole
	// subtitle has to pass.
	MinTopPartSigma = 6.5

	// MinCutJump, MaxCutJump: how far apart, in seconds, the offsets either
	// side of a cut may be. Under a few seconds is drift, not a cut; an ad
	// break runs a few minutes at most.
	MinCutJump = 3.0
	MaxCutJump = 400.0

	// MinRescueSigma: a subtitle under the gate is still searched for
	// parts from this sigma up.
	MinRescueSigma = 3.5

	// PartWindow: a part's sigma is judged against the offsets this many
	// seconds either side of its own, as wide as Align searches. Across all
	// of partRange the cues of a short video slide in and out of the audio
	// and over denser and sparser speech, which spreads the rates and pulls
	// a real part's sigma down. 0 judges against all of partRange.
	PartWindow = 120.0
)

const (
	maxParts = 4  // offsets per subtitle
	minPart  = 20 // cues in the shortest part

	// parts may sit further out than maxOffset: an ad break alone can run
	// two minutes on top of the subtitle's own offset. Searched coarser,
	// then settled.
	partRange = 600.0
	partStep  = 0.2
)

var partSteps = int(math.Round(2*partRange/partStep)) + 1

func partAt(k int) float64 { return -partRange + float64(k)*partStep }

// split returns the parts of a subtitle aligned as a, and the scale they
// hold at, or nil when one offset is the best answer. cues are the cue
// starts as written. strict asks for the full gates (MinPartSigma and
// MinTopPartSigma); without it two sides at minZ are
// enough, which is all the sampled audio can show and all it has to decide.
//
// A cut also throws off the frame rate search (half the cues fit no offset,
// and a ratio that smears them fits a little better), so a ratio is tried
// against scale 1, whole or in parts.
func split(cues []float64, a Alignment, onsets []float64, windows [][2]float64, minZ float64, strict bool) ([]Part, float64) {
	orig := append([]float64(nil), cues...)
	sort.Float64s(orig)
	scales := []float64{a.Scale}
	if a.Scale != 1 {
		scales = append(scales, 1)
	}
	var best []Part
	bestScale, bestScore := a.Scale, math.Inf(-1)
	for _, sc := range scales {
		scaled := make([]float64, len(orig))
		for i, c := range orig {
			scaled[i] = c * sc
		}
		parts, score := segment(orig, scaled, onsets, windows, minZ, strict)
		Trace("split scale=%.4f strict=%v score=%.1f parts%s", sc, strict, score, fmtParts(parts))
		// a stretched subtitle cut into a staircase at scale 1 still loses
		// to its own ratio whole
		if score > bestScore {
			best, bestScale, bestScore = parts, sc, score
		}
	}
	return best, bestScale
}

func fmtParts(parts []Part) string {
	s := ""
	for _, p := range parts {
		s += fmt.Sprintf(" %.0f/%.3f/%.2f", p.From, p.Offset, p.Sigma)
	}
	return s
}

// grid holds, for every offset in partRange, which cues land in decoded
// audio and which of those land on a speech start: one bit per cue.
type grid struct {
	n, words int
	cov, hit []uint64 // row k is [k*words, (k+1)*words)
	chance   float64  // hits per judged cue over every offset
}

func newGrid(cues, onsets []float64, windows [][2]float64) *grid {
	n := len(cues)
	g := &grid{n: n, words: (n + 63) / 64}
	g.cov = make([]uint64, partSteps*g.words)
	g.hit = make([]uint64, partSteps*g.words)
	var mu sync.Mutex
	var wg sync.WaitGroup
	hits, judged := 0, 0
	workers := runtime.GOMAXPROCS(0)
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h, j := 0, 0
			for k := w; k < partSteps; k += workers {
				row := k * g.words
				for i, c := range cues {
					t := c + partAt(k)
					if !covered(t, windows) {
						continue
					}
					g.cov[row+i/64] |= 1 << (i % 64)
					j++
					if hit(onsets, t) {
						g.hit[row+i/64] |= 1 << (i % 64)
						h++
					}
				}
			}
			mu.Lock()
			hits, judged = hits+h, judged+j
			mu.Unlock()
		}()
	}
	wg.Wait()
	if judged > 0 {
		g.chance = float64(hits) / float64(judged)
	}
	return g
}

// count is the judged cues and hits among cues [lo, hi) at offset k.
func (g *grid) count(k, lo, hi int) (judged, hits int) {
	row := k * g.words
	for lo < hi {
		w, b := lo/64, lo%64
		m := ^uint64(0) << b
		if hi < (w+1)*64 {
			m &= ^uint64(0) >> (64 - hi%64)
		}
		judged += bits.OnesCount64(g.cov[row+w] & m)
		hits += bits.OnesCount64(g.hit[row+w] & m)
		lo = (w + 1) * 64
	}
	return
}

// excess is hits beyond chance, the score a part is judged by: unlike the
// hit rate it adds up across parts, unlike plain hits it doesn't reward an
// offset for landing more cues in decoded audio.
func (g *grid) excess(k, lo, hi int) float64 {
	j, h := g.count(k, lo, hi)
	return float64(h) - g.chance*float64(j)
}

// best is the offset with the most excess for cues [lo, hi).
func (g *grid) best(lo, hi int) (int, float64) {
	bk, be := 0, math.Inf(-1)
	for k := range partSteps {
		if e := g.excess(k, lo, hi); e > be {
			bk, be = k, e
		}
	}
	return bk, be
}

// sigma is how far offset k stands out for cues [lo, hi) against the
// offsets within PartWindow seconds of it (all of partRange when 0), like
// Alignment.Sigma.
func (g *grid) sigma(k, lo, hi int) float64 {
	minJudged := max((hi-lo)/10, minPart)
	a, b := 0, partSteps-1
	if PartWindow > 0 {
		w := int(math.Round(PartWindow / partStep))
		a, b = max(k-w, 0), min(k+w, partSteps-1)
	}
	rates := make([]float64, b-a+1)
	for i := range rates {
		j, h := g.count(a+i, lo, hi)
		rates[i] = math.NaN()
		if j >= minJudged {
			rates[i] = float64(h) / float64(j)
		}
	}
	mean, sd := spread(rates)
	if math.IsNaN(rates[k-a]) || sd == 0 {
		return 0
	}
	return (rates[k-a] - mean) / sd
}

// cut is the best place to cut cues [lo, hi) in two: the first cue of the
// second side, both sides' offsets and the excess gained over one offset
// after the cost of the cut. b is -1 when there's no room.
//
// The sides' offsets are MinCutJump to MaxCutJump apart. Closer is drift
// that refine handles, further a lucky stretch far out, and either as the
// best cut would hide a real one behind it.
func (g *grid) cut(cues []float64, lo, hi int) (b, kl, kr int, gain float64) {
	jmin := int(math.Ceil(MinCutJump/partStep - 1e-9))
	jmax := min(int(math.Floor(MaxCutJump/partStep+1e-9)), partSteps-1)
	if hi-lo < 2*minPart || jmin > jmax {
		return -1, 0, 0, 0
	}
	tot := make([]float64, partSteps) // excess of all cues [lo, hi) at offset k
	whole := math.Inf(-1)
	for k := range tot {
		tot[k] = g.excess(k, lo, hi)
		whole = max(whole, tot[k])
	}
	pre := make([]float64, partSteps) // excess of cues [lo, c]
	cov := make([]uint64, partSteps)  // cue c's word of every row
	hit := make([]uint64, partSteps)
	right := make([]float64, partSteps) // excess of cues after c
	dq := make([]int, partSteps)        // sliding window maximum of right
	b, gain = -1, math.Inf(-1)
	gap := 0.0
	for c := lo; c < hi-minPart; c++ {
		if c == lo || c%64 == 0 {
			for k := range cov {
				cov[k], hit[k] = g.cov[k*g.words+c/64], g.hit[k*g.words+c/64]
			}
		}
		s := uint(c % 64)
		for k := range pre {
			if cov[k]>>s&1 == 1 {
				pre[k] -= g.chance
				if hit[k]>>s&1 == 1 {
					pre[k]++
				}
			}
		}
		x := c + 1 // first cue of the right side
		if x-lo < minPart {
			continue
		}
		for k := range right {
			right[k] = tot[k] - pre[k]
		}
		// best left offset k with the best right one jmin to jmax steps
		// above it, then below it
		bv, bl, br := math.Inf(-1), 0, 0
		h, t, next := 0, 0, jmin
		for k := range pre {
			for ; next <= k+jmax && next < partSteps; next++ {
				for t > h && right[dq[t-1]] <= right[next] {
					t--
				}
				dq[t], t = next, t+1
			}
			for t > h && dq[h] < k+jmin {
				h++
			}
			if t > h && pre[k]+right[dq[h]] > bv {
				bv, bl, br = pre[k]+right[dq[h]], k, dq[h]
			}
		}
		h, t = 0, 0
		for k := range pre {
			if j := k - jmin; j >= 0 {
				for t > h && right[dq[t-1]] <= right[j] {
					t--
				}
				dq[t], t = j, t+1
			}
			for t > h && dq[h] < k-jmax {
				h++
			}
			if t > h && pre[k]+right[dq[h]] > bv {
				bv, bl, br = pre[k]+right[dq[h]], k, dq[h]
			}
		}
		cost := CutSwitchCost
		if cues[x]+partAt(br) < cues[x-1]+partAt(bl) {
			cost += CutSwitchCost // it moves a cue before the last one
		}
		gi := bv - whole - cost
		// on a tie (no speech either side can claim) cut at the widest gap
		if gi > gain+1e-9 || gi > gain-1e-9 && cues[x]-cues[x-1] > gap {
			b, kl, kr, gain, gap = x, bl, br, gi, cues[x]-cues[x-1]
		}
	}
	return b, kl, kr, gain
}

// segment cuts cues (sorted, scaled) into parts. score is the excess the
// parts explain after the cost of the cuts, one offset's when there are no
// parts.
func segment(orig, scaled, onsets []float64, windows [][2]float64, minZ float64, strict bool) ([]Part, float64) {
	if len(scaled) < 2*minPart {
		return nil, math.Inf(-1)
	}
	g := newGrid(scaled, onsets, windows)
	// a stretch and the best cut of it, gates passed or not (ok)
	type seg struct {
		lo, hi, k int
		b, kl, kr int
		gain      float64
		ok        bool
	}
	gate := minZ
	if strict {
		gate = max(minZ, MinPartSigma)
	}
	mk := func(lo, hi, k int) seg {
		s := seg{lo: lo, hi: hi, k: k}
		b, kl, kr, gain := g.cut(scaled, lo, hi)
		if b < 0 {
			return s
		}
		if jump := math.Abs(partAt(kl) - partAt(kr)); gain <= 0 || jump < MinCutJump || jump > MaxCutJump {
			Trace("no cut %d-%d at %d: %.3f|%.3f gain=%.1f", lo, hi, b, partAt(kl), partAt(kr), gain)
			return s
		}
		zl, zr := g.sigma(kl, lo, b), g.sigma(kr, b, hi)
		Trace("cut %d-%d at %d: %.3f|%.3f gain=%.1f z=%.2f|%.2f", lo, hi, b, partAt(kl), partAt(kr), gain, zl, zr)
		s.b, s.kl, s.kr, s.gain, s.ok = b, kl, kr, gain, zl >= gate && zr >= gate
		return s
	}
	k0, whole := g.best(0, len(scaled))
	score := whole
	segs := []seg{mk(0, len(scaled), k0)}
	for len(segs) < maxParts {
		bi := -1
		for i, s := range segs {
			if s.ok && (bi < 0 || s.gain > segs[bi].gain) {
				bi = i
			}
		}
		if bi < 0 {
			break
		}
		s := segs[bi]
		segs = append(segs[:bi], append([]seg{mk(s.lo, s.b, s.kl), mk(s.b, s.hi, s.kr)}, segs[bi+1:]...)...)
		score += s.gain
	}
	if len(segs) < 2 {
		return nil, whole
	}
	parts := make([]Part, len(segs))
	top := 0.0
	for i, s := range segs {
		c := scaled[s.lo:s.hi]
		// the grid is coarse, settle on the part's own cues
		o := refine(c, settle(c, partAt(s.k), onsets, windows), onsets, windows)
		parts[i] = Part{From: orig[s.lo], Offset: o, Sigma: g.sigma(s.k, s.lo, s.hi)}
		top = max(top, parts[i].Sigma)
	}
	parts[0].From = 0
	if strict && top < MinTopPartSigma {
		return nil, whole
	}
	return parts, score
}

// spread is the mean and standard deviation of the rates that aren't NaN.
func spread(rates []float64) (float64, float64) {
	sum, sumSq, n := 0.0, 0.0, 0.0
	for _, r := range rates {
		if !math.IsNaN(r) {
			sum, sumSq, n = sum+r, sumSq+r*r, n+1
		}
	}
	if n == 0 {
		return 0, 0
	}
	mean := sum / n
	return mean, math.Sqrt(math.Max(sumSq/n-mean*mean, 0))
}

// settle returns the middle of the best plateau within a tolerance of
// offset, judging only cues.
func settle(cues []float64, offset float64, onsets []float64, windows [][2]float64) float64 {
	n := int(math.Ceil(tolerance / step))
	best, first, last, inRun := -1, offset, offset, false
	for k := -n; k <= n; k++ {
		off := offset + float64(k)*step
		hits := 0
		for _, c := range cues {
			if covered(c+off, windows) && hit(onsets, c+off) {
				hits++
			}
		}
		prev := inRun
		switch {
		case hits > best:
			best, first, last, inRun = hits, off, off, true
		case hits == best && prev:
			last = off
		default:
			inRun = false
		}
	}
	return (first + last) / 2
}
