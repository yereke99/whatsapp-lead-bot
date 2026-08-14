package media

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrFFmpegMissing indicates the transcoder is unavailable, which means voice
// messages cannot be produced from non-Opus source audio.
var ErrFFmpegMissing = errors.New("ffmpeg is not available")

// Transcoder wraps ffmpeg/ffprobe.
type Transcoder struct {
	ffmpeg  string
	ffprobe string
	bitrate string

	once      sync.Once
	available bool
}

func NewTranscoder(ffmpegPath, ffprobePath, bitrate string) *Transcoder {
	if bitrate == "" {
		bitrate = "32k"
	}
	return &Transcoder{ffmpeg: ffmpegPath, ffprobe: ffprobePath, bitrate: bitrate}
}

// Available reports whether ffmpeg can be executed. The probe runs once and
// the result is cached for the process lifetime.
func (t *Transcoder) Available() bool {
	t.once.Do(func() {
		if t.ffmpeg == "" {
			return
		}
		path, err := exec.LookPath(t.ffmpeg)
		if err != nil {
			return
		}
		t.ffmpeg = path
		t.available = true

		if t.ffprobe != "" {
			if probePath, err := exec.LookPath(t.ffprobe); err == nil {
				t.ffprobe = probePath
			} else {
				t.ffprobe = ""
			}
		}
	})
	return t.available
}

// ToVoiceOpus converts any supported audio file into the mono OGG/Opus form
// WhatsApp renders as a voice note, writing the result next to a temporary
// path and returning it.
//
// The caller owns the returned file and must remove it once stored.
func (t *Transcoder) ToVoiceOpus(ctx context.Context, sourcePath string) (string, error) {
	if !t.Available() {
		return "", ErrFFmpegMissing
	}

	outPath := filepath.Join(os.TempDir(),
		fmt.Sprintf("voice-%d-%s.ogg", time.Now().UnixNano(), filepath.Base(sourcePath)))
	outPath = strings.TrimSuffix(outPath, filepath.Ext(outPath)) + ".ogg"

	// Mono 48 kHz Opus in an Ogg container is what the WhatsApp clients expect
	// from a push-to-talk recording; stereo or a raw Opus stream is rendered
	// as an ordinary audio attachment instead.
	args := []string{
		"-hide_banner",
		"-loglevel", "error",
		"-nostdin",
		"-y",
		"-i", sourcePath,
		"-vn",
		"-map_metadata", "-1",
		"-ac", "1",
		"-ar", "48000",
		"-c:a", "libopus",
		"-b:a", t.bitrate,
		"-application", "voip",
		"-f", "ogg",
		outPath,
	}

	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, t.ffmpeg, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		os.Remove(outPath)
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		return "", fmt.Errorf("convert audio to opus: %s", truncate(detail, 400))
	}

	info, err := os.Stat(outPath)
	if err != nil || info.Size() == 0 {
		os.Remove(outPath)
		return "", errors.New("conversion produced an empty file")
	}

	return outPath, nil
}

type probeResult struct {
	Format struct {
		Duration string `json:"duration"`
	} `json:"format"`
	Streams []struct {
		CodecType string `json:"codec_type"`
		CodecName string `json:"codec_name"`
		Width     int    `json:"width"`
		Height    int    `json:"height"`
		Channels  int    `json:"channels"`
	} `json:"streams"`
}

// Probe describes a media file's measurable properties. Missing values stay
// nil rather than defaulting to zero, so the UI can distinguish "unknown".
type Probe struct {
	DurationMS *int
	Width      *int
	Height     *int
	AudioCodec string
	Channels   int
}

// Inspect reads duration and dimensions via ffprobe. It returns a zero Probe
// without error when ffprobe is unavailable, because metadata is a nicety and
// must never block an upload.
func (t *Transcoder) Inspect(ctx context.Context, path string) Probe {
	if !t.Available() || t.ffprobe == "" {
		return Probe{}
	}

	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, t.ffprobe,
		"-hide_banner", "-loglevel", "error",
		"-show_format", "-show_streams",
		"-print_format", "json", path)

	out, err := cmd.Output()
	if err != nil {
		return Probe{}
	}

	var parsed probeResult
	if err := json.Unmarshal(out, &parsed); err != nil {
		return Probe{}
	}

	var probe Probe
	if secs, err := strconv.ParseFloat(parsed.Format.Duration, 64); err == nil && secs > 0 {
		ms := int(secs * 1000)
		probe.DurationMS = &ms
	}
	for _, s := range parsed.Streams {
		switch s.CodecType {
		case "video":
			if s.Width > 0 && probe.Width == nil {
				w, h := s.Width, s.Height
				probe.Width, probe.Height = &w, &h
			}
		case "audio":
			if probe.AudioCodec == "" {
				probe.AudioCodec = s.CodecName
				probe.Channels = s.Channels
			}
		}
	}
	return probe
}

// IsVoiceReady reports whether a file is already OGG/Opus and therefore needs
// no conversion before being sent as a voice note.
func (t *Transcoder) IsVoiceReady(ctx context.Context, path string) bool {
	if strings.ToLower(filepath.Ext(path)) != ".ogg" {
		return false
	}
	probe := t.Inspect(ctx, path)
	if probe.AudioCodec == "" {
		// Without ffprobe the extension is the best signal available.
		return true
	}
	return strings.EqualFold(probe.AudioCodec, "opus")
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
