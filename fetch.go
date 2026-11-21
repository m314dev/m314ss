// Package subtitles finds a subtitle for a video, checks that it actually
// belongs to that video, and fixes the timing.
//
// Subtitle sites list a lot of files per title, cut for different releases,
// and the episode tagging is not reliable. Metadata can't tell you which file
// is right. The audio can: a subtitle that belongs to a video has cue starts
// that line up with where speech begins. So every candidate is checked against
// the video before it's accepted, and the offset correction comes out of the
// same pass.
//
// The searching itself is github.com/mohaanymo/subapi.
//
//	speech, _ := subtitles.Listen("/media/Breaking.Bad.S01E01.mkv")
//	c := subtitles.Client{Sources: []subapi.Source{
//	    &subapi.SubDL{APIKey: subdlKey},
//	    &subapi.OpenSubtitles{APIKey: osKey, Username: u, Password: p},
//	}, Speech: speech}
//	res, err := c.Fetch(ctx, subapi.Query{TMDBID: 1396, Season: 1, Episode: 1, Lang: "ar"})
package subtitles

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/m314dev/m314ss/srt"
	"github.com/mohaanymo/subapi"
)

// ErrNoMatch means every candidate was either unavailable or failed to align.
var ErrNoMatch = errors.New("no subtitle matched this video")

// Client searches sources in order and returns the first subtitle the audio
// agrees with.
type Client struct {
	Sources []subapi.Source

	// How many candidates to download from one source before moving on.
	// Downloads are the metered thing on opensubtitles. 0 means 3.
	MaxTriesPerSource int

	// What candidates get verified against. Build it with Listen. nil skips
	// verification and just returns the first thing that downloads.
	//
	// It's only onsets and window bounds, no audio, so it's small enough to
	// send over the wire. See Handler.
	Speech *Speech

	// If set, a title with no subtitle in the requested language gets searched
	// in other languages, verified the same way, and machine translated into
	// the one asked for. Costs a credit and a wait, so it's opt in. Needs a
	// SubDL pro key.
	TranslateWith *subapi.SubDL

	// Languages to try for the translation fallback. Empty means english
	// first, then anything.
	FallbackLangs []string

	// Translation tone, see the subapi Tone constants. Empty lets subdl pick.
	Tone string

	// Called with one line per candidate tried, if set.
	Log func(string)
}

// Result is an accepted subtitle.
type Result struct {
	Data    []byte // SRT bytes, already shifted
	Source  string
	Release string
	Sync    SyncResult

	// The source's own id for this subtitle, if it has one.
	ID string

	// Language of Data. Same as the query unless Translated.
	Lang string

	// Set when nothing existed in the requested language and this one was
	// machine translated from FromLang.
	Translated bool
	FromLang   string
}

// Fetch returns the first subtitle the video's audio agrees with.
//
// Sources are tried in order and candidates in the order the source returned
// them, so put the cheapest source first. If nothing matches the error wraps
// ErrNoMatch. That is a normal outcome, not a failure: ship without a
// subtitle rather than attach a wrong one.
func (c *Client) Fetch(ctx context.Context, q subapi.Query) (Result, error) {
	res, err := c.search(ctx, q)
	if err == nil || c.TranslateWith == nil || q.Lang == "" || !errors.Is(err, ErrNoMatch) {
		return res, err
	}
	return c.translateFallback(ctx, q, err)
}

// translateFallback handles a title with no subtitle in the language we want.
//
// A subtitle in some other language is still the right subtitle for this
// video, and the audio proves it just as well since the language has nothing
// to do with when the lines are spoken. So verify one the normal way, then pay
// to translate a file we already know fits. Never translate unverified, a
// wrong subtitle in the right language is still wrong.
//
// Transcribing when there is no subtitle in any language would be the obvious
// next step, but subdl's endpoint wants a media_url its own servers can fetch
// and the video never leaves the caller's machine. See subapi.SubDL.Transcribe
// if you do have a public URL.
func (c *Client) translateFallback(ctx context.Context, q subapi.Query, prev error) (Result, error) {
	langs := c.FallbackLangs
	if len(langs) == 0 {
		langs = []string{"EN", ""}
	}

	errs := []error{prev}
	for _, lang := range langs {
		if strings.EqualFold(lang, q.Lang) {
			continue // already tried
		}
		if ctx.Err() != nil {
			return Result{}, ctx.Err()
		}

		alt := q
		alt.Lang = lang
		c.logf("no %s subtitle; trying %s", q.Lang, langName(lang))
		res, err := c.search(ctx, alt)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if res.ID == "" {
			// translation works by id, the bytes are no use to it
			c.logf("%s: %s: verified but has no id to translate", res.Source, res.Release)
			continue
		}

		out, err := c.translate(ctx, res, q.Lang)
		if err != nil {
			c.logf("translating %s: %v", res.Release, err)
			errs = append(errs, err)
			continue
		}
		return out, nil
	}
	return Result{}, fmt.Errorf("%w (%v)", ErrNoMatch, errors.Join(errs...))
}

func (c *Client) translate(ctx context.Context, res Result, lang string) (Result, error) {
	s := c.TranslateWith
	job, err := s.Translate(ctx, res.ID, strings.ToUpper(lang), c.Tone)
	if err != nil {
		return Result{}, err
	}
	c.logf("translating %s %s to %s (job %s)", langName(res.Lang), res.Release, strings.ToUpper(lang), job.Ref())

	done, err := s.WaitTranslation(ctx, job.Ref(), 0)
	if err != nil {
		return Result{}, err
	}
	data, err := s.DownloadTranslation(ctx, done)
	if err != nil {
		return Result{}, err
	}

	out := res
	out.Data = data
	out.Translated = true
	out.FromLang = res.Lang
	out.Lang = lang

	// The translation is made from the original file, not the shifted one, so
	// it needs the same correction. Re-measuring is cheap and catches a
	// translator that retimed things itself.
	if c.Speech != nil {
		if m, fixed, err := c.Speech.Match(data); err == nil && m.Matched() {
			out.Data, out.Sync = fixed, m
			return out, nil
		}
	}
	// A translated file can be thinner to align against, so it may not pass
	// on its own. The original's correction still holds if its cues are the
	// original's. If they aren't, it is a translation of some other file (a
	// season pack's id can stand for every episode in it), and shipping it
	// would put a wrong subtitle in the right language on the video.
	if res.Sync.Shifted {
		data = res.Sync.Retime(data)
	}
	if !sameTiming(data, res.Data) {
		return Result{}, fmt.Errorf("translation of %s isn't timed like it, not the same file", res.Release)
	}
	out.Data = data
	return out, nil
}

// sameTiming reports whether most cues of a start where a cue of b does: a
// translation keeps the original's timestamps, merging or splitting a few
// lines at most. Another file's cues land near b's only by chance.
func sameTiming(a, b []byte) bool {
	as, bs := srt.Starts(srt.Parse(a)), srt.Starts(srt.Parse(b))
	if len(as) == 0 || len(bs) == 0 {
		return false
	}
	sort.Float64s(bs)
	near := 0
	for _, t := range as {
		i := sort.SearchFloat64s(bs, t-0.1)
		if i < len(bs) && bs[i] <= t+0.1 {
			near++
		}
	}
	return float64(near) >= 0.8*float64(len(as))
}

func (c *Client) logf(format string, args ...any) {
	if c.Log != nil {
		c.Log(fmt.Sprintf(format, args...))
	}
}

func langName(lang string) string {
	if lang == "" {
		return "any language"
	}
	return strings.ToUpper(lang)
}

func (c *Client) search(ctx context.Context, q subapi.Query) (Result, error) {
	tries := c.MaxTriesPerSource
	if tries <= 0 {
		tries = 3
	}
	logf := c.logf

	var errs []error
	for _, src := range c.Sources {
		candidates, err := src.Find(ctx, q)
		if err != nil {
			logf("%s: search failed: %v", src.Name(), err)
			errs = append(errs, fmt.Errorf("%s: %w", src.Name(), err))
			continue
		}
		if len(candidates) == 0 {
			logf("%s: nothing for this title", src.Name())
			continue
		}

		used := 0
		for _, cand := range candidates {
			if used >= tries {
				break
			}
			if ctx.Err() != nil {
				return Result{}, ctx.Err()
			}
			data, err := cand.Fetch(ctx)
			if err != nil {
				// e.g. a season pack without our episode. costs a download
				// but not worth stopping for.
				logf("%s: %s: %v", src.Name(), cand.Release, err)
				errs = append(errs, err)
				used++
				continue
			}
			used++

			if c.Speech == nil {
				return Result{Data: data, Source: cand.Source, Release: cand.Release,
					ID: cand.ID, Lang: q.Lang}, nil
			}
			res, fixed, err := c.Speech.Match(data)
			if err != nil {
				logf("%s: %s: %v", src.Name(), cand.Release, err)
				errs = append(errs, err)
				continue
			}
			if !res.Matched() {
				logf("%s: %s: rejected (sigma %.1f, need %.1f; %.0f%% hit vs %.0f%% by chance)",
					src.Name(), cand.Release, res.Sigma(), MinSigma, 100*res.HitRate, 100*res.Baseline)
				continue
			}
			logf("%s: %s: ok (sigma %.1f, %.0f%% hit vs %.0f%% by chance), %s",
				src.Name(), cand.Release, res.Sigma(), 100*res.HitRate, 100*res.Baseline, res.Correction())
			return Result{Data: fixed, Source: cand.Source, Release: cand.Release,
				Sync: res, ID: cand.ID, Lang: q.Lang}, nil
		}
	}

	if len(errs) > 0 {
		return Result{}, fmt.Errorf("%w (%v)", ErrNoMatch, errors.Join(errs...))
	}
	return Result{}, ErrNoMatch
}
