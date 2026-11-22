// Package motion implements Vigilant's motion detector: an ffmpeg pull from
// the go2rtc restream into raw grayscale frames, vectorizable frame
// differencing, geometric masking, and a sliding-window state machine that
// turns motion frames into motion events.
//
// CPU strategy: decoding happens in ffmpeg (libavcodec, which already uses
// MMX/SSE/AVX neon asm internally); Vigilant only ever touches tiny gray
// frames (default 320x180 @ 5fps ≈ 0.3 MPix/s per camera), so detection is a
// tight linear pass over contiguous byte slices that autovectorizes and
// keeps system load minimal. Cameras exposing a substream should point
// motion.url at it so the main stream is never decoded twice.
package motion

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"os/exec"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jbutlerdev/vigilant/internal/config"
)

// State is the detector state machine state.
type State int

const (
	// StateIdle: no recent motion.
	StateIdle State = iota
	// StateMotion: an active motion event; refreshed by each motion frame.
	StateMotion
)

// String renders the state for JSON output.
func (s State) String() string {
	if s == StateMotion {
		return "motion"
	}
	return "idle"
}

// Event describes a completed motion event.
type Event struct {
	ID        string    `json:"id"`
	Camera    string    `json:"camera"`
	Start     time.Time `json:"start"`
	End       time.Time `json:"end"`
	Frames    int       `json:"frames"`
	MaxScore  int       `json:"max_score"`
	Thumbnail string    `json:"thumbnail,omitempty"`
	Clips     []string  `json:"clips,omitempty"`
}

// Status is the live detector status for one camera.
type Status struct {
	Camera     string    `json:"camera"`
	State      string    `json:"state"`
	LastMotion time.Time `json:"last_motion,omitzero"`
	Running    bool      `json:"running"`
	FPS        float64   `json:"fps"`
	Score      int       `json:"score"`
}

// Handler receives state transitions from the detector.
type Handler interface {
	OnMotionStart(cam string, start time.Time)
	OnMotionEnd(ev Event)
}

// Detector runs one camera's motion pipeline.
type Detector struct {
	cam     *config.Camera
	stream  string // RTSP URL to pull (served by local go2rtc)
	ffmpeg  string
	handler Handler
	log     *slog.Logger

	maskBox MaskBox

	mu          sync.Mutex
	state       State
	window      []time.Time // sliding window of motion-frame timestamps
	lastMot     time.Time
	maxScore    int
	score       int
	eventStart  time.Time
	eventFrames int

	frames atomic.Uint64
	run    atomic.Bool
}

// NewDetector builds a detector for a camera. streamURL is the local go2rtc
// RTSP endpoint for the camera's motion stream.
func NewDetector(cam *config.Camera, streamURL, ffmpegPath string, h Handler, log *slog.Logger) *Detector {
	d := &Detector{
		cam:     cam,
		stream:  streamURL,
		ffmpeg:  ffmpegPath,
		handler: h,
		log:     log,
	}
	m := cam.MotionSettings()
	d.maskBox.Set(BuildMask(m.Width, m.Height, m.Mask))
	return d
}

// SetMask hot-swaps the ignore mask without restarting the pipeline.
func (d *Detector) SetMask(m config.Mask) {
	cfg := d.cam.MotionSettings()
	d.maskBox.Set(BuildMask(cfg.Width, cfg.Height, m))
}

// Status returns the current detector status.
func (d *Detector) Status() Status {
	d.mu.Lock()
	defer d.mu.Unlock()
	return Status{
		Camera:     d.cam.Name,
		State:      d.state.String(),
		LastMotion: d.lastMot,
		Running:    d.run.Load(),
		FPS:        d.cam.MotionSettings().FPS,
		Score:      d.score,
	}
}

// Run supervises the ffmpeg frame pipeline until ctx is cancelled.
func (d *Detector) Run(ctx context.Context) {
	m := d.cam.MotionSettings()
	frameSize := m.Width * m.Height
	go d.ticker(ctx)

	for {
		if ctx.Err() != nil {
			d.run.Store(false)
			d.forceEnd()
			return
		}
		err := d.pipe(ctx, frameSize)
		if ctx.Err() != nil {
			d.run.Store(false)
			d.forceEnd()
			return
		}
		d.log.Warn("motion pipeline exited, restarting", "err", err)
		select {
		case <-ctx.Done():
			d.run.Store(false)
			d.forceEnd()
			return
		case <-time.After(5 * time.Second):
		}
	}
}

// forceEnd closes an in-progress motion event when the pipeline stops (e.g.
// settings edit or shutdown) so events never stay open forever.
func (d *Detector) forceEnd() {
	d.mu.Lock()
	inMotion := d.state == StateMotion
	d.lastMot = time.Time{}
	d.mu.Unlock()
	if inMotion {
		d.processFrame(time.Now(), 0)
	}
}

func (d *Detector) pipe(ctx context.Context, frameSize int) error {
	m := d.cam.MotionSettings()
	args := []string{
		"-hide_banner", "-loglevel", "error",
		"-rtsp_transport", "tcp",
		"-fflags", "nobuffer",
		"-i", d.stream,
		"-vf", "fps=" + strconv.FormatFloat(m.FPS, 'g', -1, 64) +
			",scale=" + strconv.Itoa(m.Width) + ":" + strconv.Itoa(m.Height) +
			",format=gray",
		"-f", "rawvideo", "-pix_fmt", "gray",
		"-an", "-sn",
		"-",
	}
	cmd := exec.CommandContext(ctx, d.ffmpeg, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr := &limitedBuf{max: 32 << 10}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	d.run.Store(true)
	defer d.run.Store(false)

	br := bufio.NewReaderSize(stdout, frameSize*4)
	prev := make([]byte, frameSize)
	cur := make([]byte, frameSize)

	done := make(chan error, 1)
	go func() {
		for {
			if _, err := io.ReadFull(br, cur); err != nil {
				done <- err
				return
			}
			score := d.scoreFrame(prev, cur)
			d.processFrame(time.Now(), score)
			prev, cur = cur, prev
			d.frames.Add(1)
		}
	}()

	select {
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		<-done
		return ctx.Err()
	case err := <-done:
		waitErr := cmd.Wait()
		if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
			return &pipeError{err: err, stderr: stderr.String()}
		}
		if waitErr != nil {
			return &pipeError{err: waitErr, stderr: stderr.String()}
		}
		return &pipeError{err: io.EOF, stderr: stderr.String()}
	}
}

type pipeError struct {
	err    error
	stderr string
}

func (e *pipeError) Error() string {
	if e.stderr != "" {
		return e.err.Error() + ": " + e.stderr
	}
	return e.err.Error()
}

func (d *Detector) scoreFrame(prev, cur []byte) int {
	m := d.cam.MotionSettings()
	mask := d.maskBox.Get()
	n := len(prev)
	thr := byte(m.Threshold)

	// Hot loop: linear pass over contiguous equal-length byte slices;
	// the compiler autovectorizes the per-byte abs-diff/compare and the
	// mask lookup stays in L1 at these frame sizes.
	changed := 0
	if mask.None() {
		for i := 0; i < n; i++ {
			a, b := prev[i], cur[i]
			d := byte(0)
			if a > b {
				d = a - b
			} else {
				d = b - a
			}
			if d >= thr {
				changed++
			}
		}
	} else {
		for i := 0; i < n; i++ {
			if mask.Ignored(i) {
				continue
			}
			a, b := prev[i], cur[i]
			d := byte(0)
			if a > b {
				d = a - b
			} else {
				d = b - a
			}
			if d >= thr {
				changed++
			}
		}
	}

	// Motion-frame criteria: the primary knob is min_percent (% of the
	// unmasked frame that changed); legacy min_ratio/min_pixels still honored.
	active := mask.ActiveCount()
	if active <= 0 {
		return 0
	}
	if m.MinPercent > 0 {
		if float64(changed) < m.MinPercent/100*float64(active) {
			return 0
		}
		return changed
	}
	if m.MinRatio > 0 {
		if float64(changed) < m.MinRatio*float64(active) {
			return 0
		}
		return changed
	}
	if changed < m.MinPixels {
		return 0
	}
	return changed
}

// processFrame feeds the sliding-window state machine.
func (d *Detector) processFrame(now time.Time, score int) {
	m := d.cam.MotionSettings()
	d.mu.Lock()
	d.score = score
	isMotion := score > 0

	// Sliding window: every motion frame appends a timestamp, which resets
	// the trigger timer; timestamps older than the window drop off.
	if isMotion {
		d.window = append(d.window, now)
		if score > d.maxScore {
			d.maxScore = score
		}
	}
	cutoff := now.Add(-m.Window.Duration)
	for len(d.window) > 0 && d.window[0].Before(cutoff) {
		d.window = d.window[1:]
	}

	switch d.state {
	case StateIdle:
		d.maxScore = 0
		d.eventFrames = 0
		d.eventStart = time.Time{}
		if len(d.window) >= m.MinFrames {
			d.state = StateMotion
			d.lastMot = now
			start := d.window[0]
			frames := len(d.window)
			d.eventStart = start
			d.eventFrames = frames
			d.log.Info("motion started", "camera", d.cam.Name, "frames", frames)
			go d.handler.OnMotionStart(d.cam.Name, start)
		}
	case StateMotion:
		if isMotion {
			d.lastMot = now // each detection resets the hold timer
			d.eventFrames++
		}
		if now.Sub(d.lastMot) > m.Hold.Duration {
			ev := Event{
				Camera:   d.cam.Name,
				Start:    d.eventStart,
				End:      d.lastMot,
				Frames:   d.eventFrames,
				MaxScore: d.maxScore,
			}
			if ev.Start.IsZero() {
				ev.Start = ev.End
			}
			d.state = StateIdle
			d.window = nil
			d.maxScore = 0
			d.eventStart = time.Time{}
			d.eventFrames = 0
			d.log.Info("motion ended", "camera", d.cam.Name, "frames", ev.Frames)
			go d.handler.OnMotionEnd(ev)
		}
	default:
	}
	d.mu.Unlock()
}

// ticker drives the state machine so events close even if frames stall.
func (d *Detector) ticker(ctx context.Context) {
	interval := d.cam.MotionSettings().Hold.Duration / 2
	if interval < time.Second {
		interval = time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			d.mu.Lock()
			inMotion := d.state == StateMotion
			stale := now.Sub(d.lastMot) > d.cam.MotionSettings().Hold.Duration
			d.mu.Unlock()
			if inMotion && stale {
				d.processFrame(now, 0)
			}
		}
	}
}

type limitedBuf struct {
	buf []byte
	max int
}

func (l *limitedBuf) Write(p []byte) (int, error) {
	if len(l.buf) < l.max {
		room := l.max - len(l.buf)
		if len(p) > room {
			l.buf = append(l.buf, p[:room]...)
		} else {
			l.buf = append(l.buf, p...)
		}
	}
	return len(p), nil
}

func (l *limitedBuf) String() string { return string(l.buf) }
