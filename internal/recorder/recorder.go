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
	"strings"
	"time"

	"github.com/jbutlerdev/vigilant/internal/config"
	"github.com/jbutlerdev/vigilant/internal/proc"
	"github.com/jbutlerdev/vigilant/internal/storage"
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
		r.pruneStubs()
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

// pruneStubs removes tiny leftover fragments from previous recorder
// sessions (a restart inside the current hour produces a short file named
// from its actual start). They clutter the recordings list and contain
// seconds of footage.
func (r *Recorder) pruneStubs() {
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".mp4") {
			continue
		}
		fi, err := e.Info()
		if err != nil || fi.Size() > 10<<20 {
			continue
		}
		start, ok := storage.SegmentTime(e.Name())
		if !ok || time.Since(start) > 3*time.Hour || time.Since(start) < 2*time.Minute {
			continue
		}
		if err := os.Remove(filepath.Join(r.dir, e.Name())); err == nil {
			r.log.Info("pruned stub segment", "camera", r.cam.Name, "file", e.Name())
		}
	}
}

func (r *Recorder) session(ctx context.Context) error {
	// -segment_atclocktime 1 cuts segments on the top of every hour. The
	// strftime pattern names each file by its actual start time, so a
	// mid-hour session restart creates an extra file (e.g.
	// 2026-11-23-18-54-30.mp4) instead of overwriting the hour's file.
	pattern := filepath.Join(r.dir, "%Y-%m-%d-%H-%M-%S.mp4")
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
		// Fragmented MP4: playable and ffmpeg-readable while still being
		// written (moov-at-end MP4s are unreadable until the muxer closes
		// them), and far more crash-tolerant.
		"-segment_format_options", "movflags=+frag_keyframe+default_base_moof",
		"-reset_timestamps", "1",
		"-strftime", "1",
		pattern,
	}
	cmd := exec.CommandContext(ctx, r.ffmpeg, args...)
	cmd.SysProcAttr = proc.ChildProcAttr()
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
