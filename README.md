# m314ss

**Does this subtitle actually match this video?**

`m314ss` is a Go library and CLI for verifying and synchronizing subtitles against the **actual audio of a video**.

Give it a video and an SRT and it will try to determine whether the subtitle is really synchronized with that video. If it is, m314ss also calculates the timing offset and fixes it automatically.

It doesn't need filenames, release names, hashes, or metadata to make that decision.

It listens to the video.

## The idea

A subtitle that belongs to a video should have a relationship with its audio: subtitle cues tend to begin around moments where speech begins.

m314ss extracts speech onsets from the video's audio and compares them with the subtitle cue timings.

But simply counting how many cues land near speech isn't reliable. Different movies have very different amounts of dialogue, music, noise, overlapping speech, etc.

Instead, m314ss searches thousands of possible timing offsets and looks at the **shape of the entire search**.

A real match tends to produce a clear spike around one particular offset.

A wrong subtitle tends to produce noise without a convincing winner.

m314ss measures how far the winning offset stands out from the normal background (`sigma`) together with how much it improves over chance (`lift`). Both have to pass the acceptance threshold before the subtitle is considered a match.

If it passes, the winning offset is also the amount needed to synchronize the subtitle.

So the same algorithm answers both questions:

> **Does this subtitle match the video?**
> **If yes, how much should it be shifted?**

## Quick start

The only external dependency is **FFmpeg** available on `PATH`.

```bash
go install github.com/m314dev/m314ss/cmd/m314ss@latest
```

Then:

```bash
m314ss -srt subtitle.srt -video movie.mkv
```

If the subtitle matches, m314ss applies the detected offset and writes the synchronized subtitle.

If it doesn't match, it is rejected.

No subtitle provider or API key is needed for this mode.

## Finding subtitles automatically

m314ss can also search for subtitles and run every candidate through the same audio verification.

Currently supported:

* SubDL
* OpenSubtitles

For example:

```bash
export SUBDL_API_KEY=...

m314ss \
  -tmdb 1396 \
  -s 1 \
  -e 1 \
  -lang ar \
  -video Breaking.Bad.S01E01.mkv
```

If a candidate passes verification, m314ss writes:

```text
Breaking.Bad.S01E01.ar.srt
```

already synchronized to the video.

You can also search using IMDb:

```bash
m314ss \
  -imdb tt0903747 \
  -s 1 \
  -e 1 \
  -lang en \
  -video episode.mkv
```

or title/year:

```bash
m314ss \
  -title "Movie Name" \
  -year 2024 \
  -lang en \
  -video movie.mkv
```

SubDL and OpenSubtitles are intentionally just **sources of candidates**.

They help m314ss find subtitles, but they are not what decides whether a subtitle is correct.

The audio matcher does.

## How matching works

m314ss samples the video's audio using FFmpeg, keeps the speech band (200 to 3500 Hz) and extracts sparse **speech onset timestamps** using an energy-based VAD.

For longer videos it samples multiple windows across the runtime instead of decoding the entire file.

For each subtitle candidate:

1. Parse its cue start timestamps.
2. Slide those timestamps from **-120s to +120s**.
3. Test offsets in **50ms steps**.
4. Count cue starts that fall within **350ms** of a detected speech onset.
5. Measure the score across every tested offset.
6. Find the strongest offset, then fine-tune it by the median gap between the cues and the speech onsets nearest them.
7. Compare that peak against the background distribution.
8. Repeat with the timestamps scaled by each framerate ratio (23.976, 24, 25) and keep the sharpest peak.
9. Look for parts that need another offset (see below).

The result contains:

```text
Scale
Offset
HitRate
Baseline
StdDev
Sigma
Lift
```

The important part is that m314ss doesn't trust `HitRate` by itself.

A noisy or dialogue-heavy video can produce lots of accidental matches even with the wrong subtitle.

Instead:

```text
sigma = (best hit rate - baseline) / standard deviation
```

A genuine match should produce an offset that stands out clearly from all the alternatives.

The current matcher requires both:

```text
Sigma >= 6.5
Lift  >= 0.05
```

before accepting the subtitle.

## What m314ss can fix

m314ss corrects **constant timing offsets** and **framerate drift**.

If the entire subtitle is consistently 4.7 seconds late, the matcher detects that and shifts every cue accordingly.

If the subtitle was timed for a 25 fps cut and the video is 24 fps, it starts in sync and ends minutes out. That is not an offset problem, so the matcher also tries every ratio of 23.976, 24 and 25 fps and keeps whichever gives the sharpest peak. The cue times are scaled by that ratio before the offset is applied.

It also fixes subtitles timed for **another cut** of the video: an ad break in the broadcast it was timed on, or a scene this video doesn't have, moves everything after it. Such a subtitle is right in parts, each part with its own offset:

1. The subtitle is cut in two at every cue, and each side is aligned on its own cues, up to ±10 minutes out (breaks can be long). The cut where the two sides explain the most cues beyond what one offset does is the break. Each side is then cut again the same way, up to 4 parts.
2. A cut has to pay for itself in extra hits, and the offsets either side of it have to be 3 to 400 seconds apart, so a good subtitle stays in one piece.
3. Both sides of every cut have to stand out on their own cues (sigma 5, judged against the offsets within 120 seconds of their own), and the strongest part has to pass the gate a whole subtitle does. That keeps a wrong subtitle from being stitched together out of lucky stretches, and lets a cut subtitle through even when no single offset fits enough of it.

Parts are only suspected on the sampled audio. When there is a suspect, the whole audio is decoded once (per video) to place them, so most subtitles never pay for it. A subtitle just under the gate gets the same second look, since the sampled windows can miss the speech it times.

Tested on 893 titles that were never used for tuning, with a copy of each title's own subtitle shifted, stretched, or cut by an inserted ad break. On the 758 titles whose own subtitle matched, 91.5% of shifted copies, 91.7% of stretched copies and 88.4% of ad-break copies came out usable (90% of the cues within 0.5 s of the original), and none of 1602 subtitles of other titles got through.

## Use as a Go library

```bash
go get github.com/m314dev/m314ss
```

The package name is `subtitles`:

```go
import subtitles "github.com/m314dev/m314ss"
```

To verify and synchronize an existing SRT:

```go
result, fixed, err := subtitles.Sync(videoPath, srtBytes)
if err != nil {
    // subtitle did not match, or another error occurred
}
```

You can also analyze the video once:

```go
speech, err := subtitles.Listen("movie.mkv")
```

and then test many subtitle candidates against it:

```go
for _, candidate := range candidates {
    result, fixed, err := speech.Match(candidate)
    if err != nil {
        continue
    }

    if result.Matched() {
        // this subtitle matches the video
    }
}
```

Audio extraction is the expensive part. Once the video has been analyzed, testing additional subtitle candidates is cheap.

## Using subtitle sources

The SubDL and OpenSubtitles clients live in their own package, [subapi](https://github.com/mohaanymo/subapi):

```go
import "github.com/mohaanymo/subapi"
```

```go
speech, err := subtitles.Listen("episode.mkv")
if err != nil {
    log.Fatal(err)
}

client := subtitles.Client{
    Sources: []subapi.Source{
        &subapi.SubDL{
            APIKey: subdlKey,
        },
        &subapi.OpenSubtitles{
            APIKey:   osKey,
            Username: username,
            Password: password,
        },
    },

    Speech: speech,
}

result, err := client.Fetch(ctx, subapi.Query{
    TMDBID:  1396,
    Season:  1,
    Episode: 1,
    Lang:    "ar",
})

if errors.Is(err, subtitles.ErrNoMatch) {
    // candidates were found, but none convincingly matched the video
}
```

Additional subtitle providers can be added by implementing the `subapi.Source` interface.

## HTTP service

m314ss can also run as an HTTP service:

```bash
m314ss -serve :8081
```

Endpoints:

```text
POST /subtitle
GET  /health
```

The client can analyze the video locally and send only the resulting onset/window data to the server.

This means the machine holding the video still performs the audio analysis, while the server can handle subtitle providers and keep their credentials centralized.

## Current limitations

m314ss is still young and the matcher will continue to be tested and tuned against more material.

Currently:

* SRT subtitles are supported.
* Constant offsets, framerate drift and cuts (up to 4 parts) are corrected.
* The offset search is limited to ±120 seconds for the whole file, ±10 minutes for a part.
* FFmpeg is required.
* Difficult audio can make matching harder.

If you find a case where:

* a correct subtitle is rejected,
* a wrong subtitle is accepted,
* or the calculated offset is wrong,

please open an issue and include as much information about the video/subtitle pair as possible.

Those cases are especially useful for improving the matcher.

## Why `m314ss`?

Because subtitle metadata can tell you what a file *claims* to be.

**m314ss tries to determine whether its timing actually fits the video.**

## License

MIT

