package podcastjob

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

type ProbeResult struct {
	DurationMS         int64
	Codec, ContentType string
}
type ProbeFunc func(context.Context, *os.File) (ProbeResult, error)

// LocalProbe permits only local file input and audio container demuxers. It never
// passes publisher tags or stderr back to jobs, feeds or logs.
func LocalProbe(program string) ProbeFunc {
	return func(parent context.Context, f *os.File) (ProbeResult, error) {
		ctx, cancel := context.WithTimeout(parent, time.Minute)
		defer cancel()
		resolvedProgram := program
		if resolvedProgram == "" {
			resolvedProgram = "ffprobe"
		}
		if _, err := exec.LookPath(resolvedProgram); err != nil {
			return ProbeResult{}, fail("probe_unavailable", "The audio probe is unavailable.")
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return ProbeResult{}, err
		}
		cmd := exec.CommandContext(ctx, resolvedProgram, "-v", "error", "-protocol_whitelist", "file", "-format_whitelist", "mp3,mov,ogg,flac", "-show_entries", "format=format_name,duration:stream=codec_type,codec_name,duration", "-of", "json") //nolint:gosec // operator-configured executable, fixed arguments and local descriptor
		cmd.Args = append(cmd.Args, probeInput(cmd, f))
		cmd.Stderr = io.Discard
		out := &boundedOutput{}
		cmd.Stdout = out
		if err := cmd.Run(); err != nil {
			return ProbeResult{}, fail("invalid_audio", "The episode is not supported audio.")
		}
		var data struct {
			Format struct {
				Name     string `json:"format_name"`
				Duration string `json:"duration"`
			} `json:"format"`
			Streams []struct {
				Type     string `json:"codec_type"`
				Codec    string `json:"codec_name"`
				Duration string `json:"duration"`
			} `json:"streams"`
		}
		if json.Unmarshal(out.b, &data) != nil {
			return ProbeResult{}, fail("invalid_audio", "The episode is not supported audio.")
		}
		result := ProbeResult{}
		duration, _ := strconv.ParseFloat(data.Format.Duration, 64)
		for _, s := range data.Streams {
			if s.Type != "audio" {
				continue
			}
			switch s.Codec {
			case "mp3", "aac", "alac", "vorbis", "opus", "flac":
			default:
				continue
			}
			result.Codec = s.Codec
			if duration <= 0 {
				duration, _ = strconv.ParseFloat(s.Duration, 64)
			}
			break
		}
		switch {
		case data.Format.Name == "mp3":
			result.ContentType = "audio/mpeg"
		case strings.Contains(data.Format.Name, "mov"):
			result.ContentType = "audio/mp4"
		case data.Format.Name == "ogg":
			result.ContentType = "audio/ogg"
		case data.Format.Name == "flac":
			result.ContentType = "audio/flac"
		}
		if result.Codec == "" || result.ContentType == "" || math.IsNaN(duration) || math.IsInf(duration, 0) || duration <= 0 || duration > float64(math.MaxInt64/1000) {
			return ProbeResult{}, fail("invalid_audio", "The episode is not supported audio.")
		}
		result.DurationMS = int64(math.Round(duration * 1000))
		if result.DurationMS <= 0 {
			return ProbeResult{}, fail("invalid_audio", "The episode is not supported audio.")
		}
		return result, nil
	}
}

type boundedOutput struct{ b []byte }

func (b *boundedOutput) Write(p []byte) (int, error) {
	if len(p) > (1<<20)-len(b.b) {
		return 0, errors.New("probe output exceeds limit")
	}
	b.b = append(b.b, p...)
	return len(p), nil
}
