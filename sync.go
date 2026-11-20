package subtitles

import (
	"fmt"
	"math"
	"sort"
	"sync"

	"github.com/mohaanymo/m314ss/audio"
	"github.com/mohaanymo/m314ss/srt"
	"github.com/mohaanymo/m314ss/vad"
)

const (
	windowLen  = 180.0 // seconds of audio per sampled window
	numWindows = 10

	// below this it's not worth rewriting a good file
	minShift = 0.15
)

// UseFFmpeg points the decoder at a specific ffmpeg binary. Default is
// whatever is on PATH.
func UseFFmpeg(path string) { audio.FFmpeg = path }

// Speech is where a video talks: the moments dialogue starts, plus which parts
// of the runtime were actually decoded.
//
// Building one costs a few seconds of ffmpeg. Matching a subtitle against it
// is well under a second, so build once and test as many candidates as you
// want.
type Speech struct {
	Onsets   []float64
	Windows  [][2]float64
	Duration float64

	path  string // the video, when Listen made this
	all   sync.Once
	whole *Speech // all of the audio, decoded on demand by full
}

// SyncResult is what matching one subtitle concluded.
type SyncResult struct {
	Alignment
	Shifted bool // whether the timings were actually changed
}

// Listen decodes a spread of the video's audio and finds where speech starts.
func Listen(videoPath string) (*Speech, error) {
	duration, err := audio.Duration(videoPath)
	if err != nil {
		return nil, fmt.Errorf("probing video: %w", err)
	}
	return listen(videoPath, duration, pickWindows(duration))
}

// full is the speech of the whole video, for placing cuts: the sampled
// windows leave gaps of several minutes. Decoded once, on first use. nil
// when this Speech didn't come from Listen or the decode failed.
func (s *Speech) full() *Speech {
	if s.path == "" || len(s.Windows) == 1 && s.Windows[0][0] == 0 {
		return nil // nothing to decode, or Listen already decoded it all
	}
	s.all.Do(func() {
		// contiguous windows, so ffmpeg still runs numWindows wide
		windows := make([][2]float64, numWindows)
		chunk := s.Duration / numWindows
		for i := range windows {
			windows[i] = [2]float64{float64(i) * chunk, chunk}
		}
		windows[numWindows-1][1] = s.Duration - windows[numWindows-1][0] + 1
		s.whole, _ = listen(s.path, s.Duration, windows)
		if s.whole != nil {
			s.whole.all.Do(func() {})
			s.whole.whole = s.whole // already all of it
		}
	})
	return s.whole
}

func listen(videoPath string, duration float64, windows [][2]float64) (*Speech, error) {
	var (
		mu     sync.Mutex
		wg     sync.WaitGroup
		onsets []float64
		failed error
	)
	for _, w := range windows {
		wg.Add(1)
		go func(start, length float64) {
			defer wg.Done()
			raw, e := audio.ExtractWindow(videoPath, start, length)
			if e == nil {
				var segments []vad.SpeechSegment
				segments, e = vad.Detect(raw, audio.SampleRate)
				if e == nil {
					mu.Lock()
					defer mu.Unlock()
					for _, seg := range segments {
						onsets = append(onsets, start+seg.Start)
					}
					return
				}
			}
			mu.Lock()
			failed = e
			mu.Unlock()
		}(w[0], w[1])
	}
	wg.Wait()

	if failed != nil {
		return nil, fmt.Errorf("extracting audio: %w", failed)
	}
	if len(onsets) == 0 {
		return nil, fmt.Errorf("no speech found in %s", videoPath)
	}
	sort.Float64s(onsets)
	return &Speech{Onsets: onsets, Windows: windows, Duration: duration, path: videoPath}, nil
}

// Match tests one subtitle against the video and returns it with the timing
// correction applied.
//
// A subtitle that doesn't belong comes back unchanged with Matched false.
// Shifting a wrong subtitle just turns an obviously wrong one into a subtly
// wrong one.
func (s *Speech) Match(data []byte) (SyncResult, []byte, error) {
	cues := srt.Parse(data)
	if len(cues) == 0 {
		return SyncResult{}, data, fmt.Errorf("no cues found (not an SRT?)")
	}
	starts := srt.Starts(cues)
	a := Align(starts, s.Onsets, s.Windows)
	Trace("sampled off=%.3f scale=%.4f sigma=%.2f lift=%.3f windows=%d", a.Offset, a.Scale, a.Sigma(), a.Lift(), len(s.Windows))
	if matched := a.Matched(); matched || a.Sigma() >= MinRescueSigma {
		// Parts are suspected on the sampled audio, then found and gated
		// on all of it. Most subtitles have no parts and never pay for
		// the full decode. One under the gate gets it too: the sampled
		// windows can miss the speech it times.
		suspect := !matched
		if !suspect {
			p, _ := split(starts, a, s.Onsets, s.Windows, CutProbeZ, false)
			suspect = p != nil
		}
		if suspect {
			on := s
			if w := s.full(); w != nil {
				on = w
			}
			Trace("full windows=%d onsets=%d", len(on.Windows), len(on.Onsets))
			if on != s {
				af := Align(starts, on.Onsets, on.Windows)
				Trace("full align off=%.3f scale=%.4f sigma=%.2f lift=%.3f", af.Offset, af.Scale, af.Sigma(), af.Lift())
				if af.Matched() || !matched {
					a, matched = af, af.Matched()
				}
			}
			if parts, sc := split(starts, a, on.Onsets, on.Windows, MinPartSigma, true); parts != nil {
				a.Parts, a.Scale = parts, sc
			}
		}
		a.rescued = !matched && a.Parts != nil
	}
	res := SyncResult{Alignment: a}
	if !a.Matched() || (a.Parts == nil && a.Scale == 1 && math.Abs(a.Offset) < minShift) {
		return res, data, nil
	}
	res.Shifted = true
	return res, a.Retime(data), nil
}

// Retime applies the correction to an SRT: Scale and Offset, or Scale and
// each part's offset.
func (a Alignment) Retime(data []byte) []byte {
	if a.Parts == nil {
		return srt.Retime(data, a.Scale, a.Offset)
	}
	from, shifts := make([]float64, len(a.Parts)), make([]float64, len(a.Parts))
	for i, p := range a.Parts {
		from[i], shifts[i] = p.From, p.Offset
	}
	return srt.RetimeParts(data, a.Scale, from, shifts)
}

// Sync is Listen followed by Match, for when you only have one subtitle.
func Sync(videoPath string, data []byte) (SyncResult, []byte, error) {
	speech, err := Listen(videoPath)
	if err != nil {
		return SyncResult{}, data, err
	}
	return speech.Match(data)
}

// pickWindows spreads sample windows across the runtime, skipping the very
// start and end (intros and credits, usually no dialogue).
func pickWindows(duration float64) [][2]float64 {
	margin := duration * 0.05
	usable := duration - 2*margin

	if usable <= windowLen*numWindows {
		return [][2]float64{{0, duration}} // short enough, decode it all
	}

	windows := make([][2]float64, numWindows)
	stride := (usable - windowLen) / float64(numWindows-1)
	for i := range windows {
		windows[i] = [2]float64{margin + float64(i)*stride, windowLen}
	}
	return windows
}
