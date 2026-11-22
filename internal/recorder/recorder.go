// Package recorder writes continuous 1-hour MP4 segments, split on the
// clock hour, by pulling from the local go2rtc restream with ffmpeg
// (stream copy, no transcode).
package recorder

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/jbutlerdev/vigilant/internal/config"
)

// Recorder supervises one ffmpeg segment-muxer per camera.
type Recorder struct {
	cam    *config.Camera
	source string // rtsp URL of the local go2rtc stream
	dir    string // recordings output directory
	ffmpeg string
	log    *slog.Logger
}

// New builds a recorder for a camera. source is the internal go2rtc RTSP
// URL; dir receives files named 2006-01-02_15.mp4.
func New(cam *config.Camera, source, dir, ffmpegPath string, log *slog.Logger) *Recorder {
	return &Recorder{cam: cam, source: source, dir: dir, ffmpeg: ffmpegPath, log: log}
}

// Run keeps the segmenter alive until ctx is cancelled.
func (r *Recorder) Run(ctx context.Context) {
	if err := os.MkdirAll(r.dir, 0o755); err != nil {
		r.log.Error("create recordings dir", "err", err)
		return
	}
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		start := time.Now()
		err := r.session(ctx)
		if ctx.Err() != nil {
			return
		}
		r.log.Warn("recorder exited, restarting", "camera", r.cam.Name, "err", err)
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > 30*time.Second {
			backoff = 30 * time.Second
		}
	}
}

func (r *Recorder) session(ctx context.Context) error {
	// -segment_atclocktime 1 splits on the top of every hour; the pattern is
	// an strftime template evaluated per segment (needs -strftime 1).
	pattern := filepath.Join(r.dir, "%Y-%m-%d_%H.mp4")
	args := []string{
		"-hide_banner", "-loglevel", "error",
		"-rtsp_transport", "tcp",
		"-i", r.source,
		"-map", "0", "-dn",
		"-c", "copy",
		"-f", "segment",
		"-segment_time", "3600",
		"-segment_atclocktime", "1",
		"-segment_format", "mp4",
		"-reset_timestamps", "1",
		"-strftime", "1",
		pattern,
	}
	cmd := exec.CommandContext(ctx, r.ffmpeg, args...)
	stderr := &tailBuf{max: 32 << 10}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	err := cmd.Wait()
	if err != nil && stderr.String() != "" {
		return &runError{err: err, stderr: stderr.String()}
	}
	return err
}

type runError struct {
	err    error
	stderr string
}

func (e *runError) Error() string { return e.err.Error() + ": " + e.stderr }

type tailBuf struct {
	buf []byte
	max int
}

func (t *tailBuf) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(p), nil
}

func (t *tailBuf) String() string { return string(t.buf) }
