package audio

import (
	"bytes"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
)

const SampleRate = 16000

// FFmpeg is the binary to run. Default is whatever is on PATH.
var FFmpeg = "ffmpeg"

var durRe = regexp.MustCompile(`Duration: (\d+):(\d{2}):(\d{2})\.(\d{2})`)

// Duration returns the container duration in seconds.
//
// Read off ffmpeg's own banner instead of shelling out to ffprobe. It doesn't
// decode anything (ffmpeg bails right away with "at least one output file
// must be specified") and it saves shipping a second binary.
func Duration(vidPath string) (float64, error) {
	out, _ := exec.Command(FFmpeg, "-hide_banner", "-i", vidPath).CombinedOutput()
	m := durRe.FindSubmatch(out)
	if m == nil {
		return 0, fmt.Errorf("no duration in ffmpeg output for %s", vidPath)
	}
	n := func(b []byte) float64 { v, _ := strconv.Atoi(string(b)); return float64(v) }
	return n(m[1])*3600 + n(m[2])*60 + n(m[3]) + n(m[4])/100, nil
}

// ExtractWindow decodes [start, start+dur) as 16kHz mono s16le PCM, cut to
// the speech band. The VAD only measures energy, so without the cut a
// film's bass and drums count as speech.
// -ss before -i does a keyframe seek, so cost scales with window size not
// file size.
func ExtractWindow(vidPath string, start, dur float64) ([]byte, error) {
	cmd := exec.Command(
		FFmpeg,
		"-v", "error",
		"-ss", fmt.Sprintf("%.3f", start),
		"-t", fmt.Sprintf("%.3f", dur),
		"-i", vidPath,
		"-vn",
		"-af", "aformat=channel_layouts=mono,aresample=16000,highpass=f=200,lowpass=f=3500",
		"-ac", "1",
		"-ar", fmt.Sprint(SampleRate),
		"-f", "s16le",
		"-",
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	raw, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("ffmpeg failed: %w\n%s", err, stderr.String())
	}
	return raw, nil
}
