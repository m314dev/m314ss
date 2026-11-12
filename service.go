package subtitles

import (
	"cmp"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/mohaanymo/subapi"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/transform"
)

// Request is the body of POST /subtitle.
//
// Onsets and Windows come from the caller running Listen over its own video.
// Sending those few hundred floats instead of the video is what lets this
// service hold the API keys while the caller, who has the file, still decides
// what counts as a match.
type Request struct {
	TMDBID  int    `json:"tmdb_id,omitempty"`
	IMDBID  string `json:"imdb_id,omitempty"`
	Title   string `json:"title,omitempty"`
	Year    int    `json:"year,omitempty"`
	Season  int    `json:"season,omitempty"`
	Episode int    `json:"episode,omitempty"`
	Lang    string `json:"lang"`

	Onsets  []float64    `json:"onsets,omitempty"`
	Windows [][2]float64 `json:"windows,omitempty"`

	MaxTries int `json:"max_tries,omitempty"`

	// Enable the translation fallback. Off by default since it costs a
	// subdl pro credit.
	Translate bool `json:"translate,omitempty"`

	// Tone for that translation. Empty lets subdl pick.
	Tone string `json:"tone,omitempty"`
}

// Response is the body of a successful POST /subtitle.
type Response struct {
	SRT      string  `json:"srt"` // already shifted
	Source   string  `json:"source"`
	Release  string  `json:"release"`
	Offset   float64 `json:"offset"`
	Scale    float64 `json:"scale"` // 1 unless the subtitle drifted
	HitRate  float64 `json:"hit_rate"`
	Baseline float64 `json:"baseline"`
	Sigma    float64 `json:"sigma"`
	Verified bool    `json:"verified"` // false when no onsets were sent

	Lang       string `json:"lang"`
	Translated bool   `json:"translated,omitempty"`
	FromLang   string `json:"from_lang,omitempty"`
}

// Handler serves POST /subtitle over the given sources.
//
// No auth here. Put it behind whatever already authenticates your callers.
func Handler(sources []subapi.Source, logger *log.Logger) http.Handler {
	mux := http.NewServeMux()

	// AI routes are subdl's, so they only exist if one is configured.
	var pro *subapi.SubDL
	for _, s := range sources {
		if sd, ok := s.(*subapi.SubDL); ok {
			pro = sd
			break
		}
	}

	mux.HandleFunc("POST /subtitle", func(w http.ResponseWriter, r *http.Request) {
		var req Request
		if !decode(w, r, &req) {
			return
		}
		if req.Lang == "" {
			httpError(w, http.StatusBadRequest, "lang is required")
			return
		}

		c := Client{Sources: sources, MaxTriesPerSource: req.MaxTries}
		if len(req.Onsets) > 0 {
			c.Speech = &Speech{Onsets: req.Onsets, Windows: req.Windows}
		}
		if req.Translate && pro != nil {
			c.TranslateWith = pro
			c.Tone = req.Tone
		}
		if logger != nil {
			c.Log = func(s string) { logger.Println(s) }
		}

		res, err := c.Fetch(r.Context(), subapi.Query{
			TMDBID: req.TMDBID, IMDBID: req.IMDBID, Title: req.Title, Year: req.Year,
			Season: req.Season, Episode: req.Episode, Lang: req.Lang,
		})
		if err != nil {
			if errors.Is(err, ErrNoMatch) {
				httpError(w, http.StatusNotFound, err.Error())
				return
			}
			httpError(w, http.StatusBadGateway, err.Error())
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(Response{
			SRT: toUTF8(res.Data, cmp.Or(res.Lang, req.Lang)), Source: res.Source, Release: res.Release,
			Offset:   res.Sync.Offset,
			Scale:    cmp.Or(res.Sync.Scale, 1),
			HitRate:  res.Sync.HitRate,
			Baseline: res.Sync.Baseline,
			Sigma:    res.Sync.Sigma(),
			Verified: c.Speech != nil,

			Lang:       cmp.Or(res.Lang, req.Lang),
			Translated: res.Translated,
			FromLang:   res.FromLang,
		})
	})

	if pro != nil {
		mountAI(mux, pro)
	}

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		names := make([]string, len(sources))
		for i, s := range sources {
			names[i] = s.Name()
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "sources": names, "ai": pro != nil})
	})

	return mux
}

// TranslateRequest is the body of POST /ai/translations.
type TranslateRequest struct {
	NID            string `json:"n_id"`
	TargetLanguage string `json:"target_language"`
	Tone           string `json:"tone,omitempty"`
}

// TranscribeRequest is the body of POST /ai/transcriptions.
type TranscribeRequest struct {
	MediaURL string `json:"media_url"`
	Language string `json:"language,omitempty"`
}

// mountAI proxies subdl's AI endpoints under the same paths, so their docs
// apply here too. Jobs aren't waited on: a request that blocks for a minute
// dies to somebody's proxy timeout. Poll GET /ai/translations/{id} and then
// hit /download.
func mountAI(mux *http.ServeMux, s *subapi.SubDL) {
	mux.HandleFunc("POST /ai/translations", func(w http.ResponseWriter, r *http.Request) {
		var req TranslateRequest
		if !decode(w, r, &req) {
			return
		}
		job, err := s.Translate(r.Context(), req.NID, req.TargetLanguage, req.Tone)
		writeJSON(w, job, err)
	})

	mux.HandleFunc("GET /ai/translations", func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		jobs, err := s.Translations(r.Context(), page)
		writeJSON(w, jobs, err)
	})

	mux.HandleFunc("GET /ai/translations/{id}", func(w http.ResponseWriter, r *http.Request) {
		job, err := s.Translation(r.Context(), r.PathValue("id"))
		writeJSON(w, job, err)
	})

	mux.HandleFunc("GET /ai/translations/{id}/download", func(w http.ResponseWriter, r *http.Request) {
		job, err := s.Translation(r.Context(), r.PathValue("id"))
		if err != nil {
			httpError(w, http.StatusBadGateway, err.Error())
			return
		}
		if !job.DownloadReady {
			httpError(w, http.StatusAccepted, "not ready (status "+job.Status+")")
			return
		}
		data, err := s.DownloadTranslation(r.Context(), job)
		if err != nil {
			httpError(w, http.StatusBadGateway, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/x-subrip")
		w.Write(data)
	})

	mux.HandleFunc("POST /ai/transcriptions", func(w http.ResponseWriter, r *http.Request) {
		var req TranscribeRequest
		if !decode(w, r, &req) {
			return
		}
		job, err := s.Transcribe(r.Context(), req.MediaURL, req.Language)
		writeJSON(w, job, err)
	})

	mux.HandleFunc("GET /ai/filename-search", func(w http.ResponseWriter, r *http.Request) {
		m, err := s.FilenameSearch(r.Context(), r.URL.Query().Get("filename"))
		if err != nil {
			httpError(w, http.StatusBadGateway, err.Error())
			return
		}
		// pass the raw payload through, the endpoint is beta and may have
		// fields FilenameMatch doesn't know about
		w.Header().Set("Content-Type", "application/json")
		w.Write(m.Raw)
	})
}

func decode(w http.ResponseWriter, r *http.Request, out any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(out); err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, v any, err error) {
	if err != nil {
		httpError(w, http.StatusBadGateway, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// legacy code pages for subtitles that aren't UTF-8; JSON would turn their
// bytes into U+FFFD and lose the text for good. Unlisted languages use 1252.
var legacy = map[string]*charmap.Charmap{
	"AR": charmap.Windows1256, "FA": charmap.Windows1256, "UR": charmap.Windows1256,
	"HE": charmap.Windows1255, "TR": charmap.Windows1254, "EL": charmap.Windows1253,
	"RU": charmap.Windows1251, "UK": charmap.Windows1251, "BG": charmap.Windows1251,
	"PL": charmap.Windows1250, "CS": charmap.Windows1250, "HU": charmap.Windows1250, "RO": charmap.Windows1250,
}

// toUTF8 returns b as UTF-8: valid UTF-8 as is, UTF-16 by its byte order
// mark, anything else in the language's legacy code page.
func toUTF8(b []byte, lang string) string {
	if utf8.Valid(b) {
		return string(b)
	}
	cm := legacy[strings.ToUpper(lang)]
	if cm == nil {
		cm = charmap.Windows1252
	}
	out, _, err := transform.Bytes(unicode.BOMOverride(cm.NewDecoder()), b)
	if err != nil {
		return strings.ToValidUTF8(string(b), "?")
	}
	return string(out)
}
