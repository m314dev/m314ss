// Command m314ss finds a subtitle for a video, checks it belongs to that
// video, and writes it out in sync.
//
//	m314ss -tmdb 1396 -s 1 -e 1 -lang ar -video Breaking.Bad.S01E01.mkv
//
// Already have a subtitle and only want it checked and retimed (no API key
// needed):
//
//	m314ss -srt my.srt -video Breaking.Bad.S01E01.mkv
//
// No ids? The filename gets resolved to a title first (subdl pro):
//
//	m314ss -lang ar -video "that.movie.2021.WEBRip.mkv"
//
// AI stuff, all subdl pro:
//
//	m314ss -translate <nId> -lang fa -tone faithful
//	m314ss -transcribe https://host/episode.mp3 -lang en
//	m314ss -identify "that.movie.2021.WEBRip.mkv"
//
// As a service, so callers need the video but not the API keys:
//
//	m314ss -serve :8081
//
// Keys come from the environment: SUBDL_API_KEY, OPENSUBTITLES_API_KEY,
// OPENSUBTITLES_USER, OPENSUBTITLES_PASS. Either source alone works. The AI
// subcommands need subdl.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	subtitles "github.com/m314dev/m314ss"
	"github.com/mohaanymo/subapi"
)

func main() {
	var (
		serve   = flag.String("serve", "", "run as an HTTP service on this address, e.g. :8081")
		tmdb    = flag.Int("tmdb", 0, "TMDB id (series id for TV)")
		imdb    = flag.String("imdb", "", "IMDb id, e.g. tt0903747")
		title   = flag.String("title", "", "title text, when no id is known")
		year    = flag.Int("year", 0, "release year, with -title")
		season  = flag.Int("s", 0, "season number")
		episode = flag.Int("e", 0, "episode number")
		lang    = flag.String("lang", "ar", "language code (target language for -translate)")
		video   = flag.String("video", "", "video to verify and sync against (strongly recommended)")
		srt     = flag.String("srt", "", "local subtitle to sync against -video instead of searching")
		out     = flag.String("o", "", "output path (default: <video>.<lang>.srt)")
		tries   = flag.Int("tries", 3, "candidates to try per source")
		ffmpeg  = flag.String("ffmpeg", "", "path to ffmpeg (default: PATH)")

		translate  = flag.String("translate", "", "AI-translate this subtitle nId into -lang (SubDL Pro)")
		tone       = flag.String("tone", "", "translation tone: faithful, formal, casual, cinematic_action, comedic_fun, anime")
		transcribe = flag.String("transcribe", "", "AI-transcribe this media URL (SubDL Pro, beta)")
		identify   = flag.String("identify", "", "AI-resolve a release filename to a title and print it (SubDL Pro, beta)")
	)
	flag.Parse()

	if *ffmpeg != "" {
		subtitles.UseFFmpeg(*ffmpeg)
	}
	if *srt != "" {
		if *video == "" {
			fail("-srt needs -video")
		}
		data, err := os.ReadFile(*srt)
		if err != nil {
			fail(err.Error())
		}
		res, fixed, err := subtitles.Sync(*video, data)
		if err != nil {
			fail(err.Error())
		}
		if !res.Matched() {
			fail(fmt.Sprintf("subtitle does not fit this video (sigma %.1f, need %.1f)", res.Sigma(), subtitles.MinSigma))
		}
		dst := destination(*out, *video, *lang)
		if err := os.WriteFile(dst, fixed, 0o644); err != nil {
			fail(err.Error())
		}
		fmt.Printf("%s  (sigma %.1f, %s)\n", dst, res.Sigma(), res.Correction())
		return
	}
	// subdl first, it's unmetered. opensubtitles backs it up for the stuff
	// subdl doesn't have.
	var subdl *subapi.SubDL
	var sources []subapi.Source
	if k := os.Getenv("SUBDL_API_KEY"); k != "" {
		subdl = &subapi.SubDL{APIKey: k}
		sources = append(sources, subdl)
	}
	if k := os.Getenv("OPENSUBTITLES_API_KEY"); k != "" {
		sources = append(sources, &subapi.OpenSubtitles{
			APIKey:    k,
			Username:  os.Getenv("OPENSUBTITLES_USER"),
			Password:  os.Getenv("OPENSUBTITLES_PASS"),
			UserAgent: "m314ss/1.0",
		})
	}
	if len(sources) == 0 {
		fail("no source configured: set SUBDL_API_KEY and/or OPENSUBTITLES_API_KEY")
	}
	ctx := context.Background()

	switch {
	case *serve != "":
		logger := log.New(os.Stderr, "", log.LstdFlags)
		logger.Printf("listening on %s", *serve)
		if err := http.ListenAndServe(*serve, subtitles.Handler(sources, logger)); err != nil {
			logger.Fatal(err)
		}
		return

	case *identify != "":
		requireSubDL(subdl)
		m, err := subdl.FilenameSearch(ctx, *identify)
		if err != nil {
			fail(err.Error())
		}
		os.Stdout.Write(append(indent(m.Raw), '\n'))
		return

	case *translate != "":
		requireSubDL(subdl)
		job, err := subdl.Translate(ctx, *translate, strings.ToUpper(*lang), *tone)
		if err != nil {
			fail(err.Error())
		}
		fmt.Fprintf(os.Stderr, "translating (%s), waiting...\n", job.Ref())
		writeSRT(waitAndFetch(ctx, subdl, job), destination(*out, *video, *lang))
		return

	case *transcribe != "":
		requireSubDL(subdl)
		job, err := subdl.Transcribe(ctx, *transcribe, *lang)
		if err != nil {
			fail(err.Error())
		}
		// no documented status endpoint for transcriptions, so just print
		// whatever the create call gave us
		if job.DownloadURL == "" {
			fmt.Fprintf(os.Stderr, "queued as %s; no download link yet\n", job.Ref())
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			enc.Encode(job)
			return
		}
		data, err := subdl.DownloadTranslation(ctx, job)
		if err != nil {
			fail(err.Error())
		}
		writeSRT(data, destination(*out, *video, *lang))
		return
	}

	q := subapi.Query{
		TMDBID: *tmdb, IMDBID: *imdb, Title: *title, Year: *year,
		Season: *season, Episode: *episode, Lang: *lang,
	}
	if *tmdb == 0 && *imdb == "" && *title == "" {
		if *video == "" {
			fmt.Fprintln(os.Stderr, "need one of -tmdb, -imdb, -title or -video (or -serve)")
			flag.Usage()
			os.Exit(2)
		}
		// nothing to search by but the filename
		requireSubDL(subdl)
		m, err := subdl.FilenameSearch(ctx, filepath.Base(*video))
		if err != nil {
			fail("cannot identify " + filepath.Base(*video) + ": " + err.Error())
		}
		q = m.Query(*lang)
		fmt.Fprintf(os.Stderr, "identified: %s (%d) tmdb %d\n", m.Title, m.Year, m.TMDBID)
	}

	c := subtitles.Client{
		Sources:           sources,
		MaxTriesPerSource: *tries,
		Log:               func(s string) { fmt.Fprintln(os.Stderr, s) },
	}
	if *video != "" {
		speech, err := subtitles.Listen(*video)
		if err != nil {
			fail(err.Error())
		}
		c.Speech = speech
	}

	res, err := c.Fetch(ctx, q)
	if err != nil {
		if errors.Is(err, subtitles.ErrNoMatch) {
			fail("no subtitle matched this video")
		}
		fail(err.Error())
	}

	dst := destination(*out, *video, *lang)
	if err := os.WriteFile(dst, res.Data, 0o644); err != nil {
		fail(err.Error())
	}
	fmt.Printf("%s  (%s: %s, sigma %.1f, %s)\n",
		dst, res.Source, res.Release, res.Sync.Sigma(), res.Sync.Correction())
}

// waitAndFetch blocks until an AI job is ready. Jobs usually finish inside a
// minute, the ceiling is generous because giving up early means redoing paid
// work.
func waitAndFetch(ctx context.Context, s *subapi.SubDL, job subapi.Job) []byte {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()

	done, err := s.WaitTranslation(ctx, job.Ref(), 5*time.Second)
	if err != nil {
		fail(err.Error())
	}
	data, err := s.DownloadTranslation(ctx, done)
	if err != nil {
		fail(err.Error())
	}
	return data
}

func destination(out, video, lang string) string {
	if out != "" {
		return out
	}
	if video == "" {
		return "out." + lang + ".srt"
	}
	return strings.TrimSuffix(video, filepath.Ext(video)) + "." + lang + ".srt"
}

func writeSRT(data []byte, dst string) {
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		fail(err.Error())
	}
	fmt.Println(dst)
}

func indent(raw []byte) []byte {
	if b, err := json.MarshalIndent(json.RawMessage(raw), "", "  "); err == nil {
		return b
	}
	return raw
}

// the AI subcommands are subdl features. say so instead of nil deref.
func requireSubDL(s *subapi.SubDL) {
	if s == nil {
		fail("this needs a subdl key: set SUBDL_API_KEY")
	}
}

func fail(msg string) {
	fmt.Fprintln(os.Stderr, msg)
	os.Exit(1)
}
