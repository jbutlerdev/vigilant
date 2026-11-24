// Package storage lays out and prunes Vigilant's on-disk media:
//
//	data/recordings/<camera>/2006-01-02_15.mp4   continuous 1h segments
//	data/events/<camera>/<event-id>/             motion event copies + meta.json
package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jbutlerdev/vigilant/internal/config"
	"github.com/jbutlerdev/vigilant/internal/motion"
)

// Segment file name layouts: current recorder names segments by their
// exact start time; the legacy layout was hour-granular.
const (
	segmentTimeFormat       = "2006-01-02-15-04-05"
	segmentTimeFormatLegacy = "2006-01-02_15"
)

// Store manages the data directory and the event index.
type Store struct {
	cfg        *config.Config
	log        *slog.Logger
	ffmpegPath string
	onChange   func()
	mu         sync.RWMutex
	events     map[string]*motion.Event
}

// New creates a store rooted at cfg.DataDir. ffmpegPath is used to extract
// event clips; onChange is invoked after clips become ready (SSE republish).
func New(cfg *config.Config, log *slog.Logger, ffmpegPath string, onChange func()) (*Store, error) {
	s := &Store{cfg: cfg, log: log, ffmpegPath: ffmpegPath, onChange: onChange, events: map[string]*motion.Event{}}
	for _, dir := range []string{s.RecordingsRoot(), s.EventsRoot()} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	if err := s.loadEvents(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) hub() {
	if s.onChange != nil {
		s.onChange()
	}
}

// SetOnChange registers a callback invoked after event data changes in
// ways clients should refresh for (e.g. clips became ready).
func (s *Store) SetOnChange(f func()) { s.onChange = f }

// RecordingsRoot is the root directory of continuous recordings.
func (s *Store) RecordingsRoot() string { return filepath.Join(s.cfg.DataDir, "recordings") }

// EventsRoot is the root directory of motion events.
func (s *Store) EventsRoot() string { return filepath.Join(s.cfg.DataDir, "events") }

// CameraRecordingsDir returns (creating if needed) the segments dir for a camera.
func (s *Store) CameraRecordingsDir(cam string) string {
	return filepath.Join(s.RecordingsRoot(), cam)
}

// EventDir returns the directory for one event.
func (s *Store) EventDir(ev *motion.Event) string {
	return filepath.Join(s.EventsRoot(), ev.Camera, ev.ID)
}

// NewID mints a unique, sortable event ID.
func NewID(cam string, t time.Time) string {
	return fmt.Sprintf("%s-%s", cam, t.UTC().Format("20060102T150405.000"))
}

// --- event index -----------------------------------------------------------

func (s *Store) loadEvents() error {
	root := s.EventsRoot()
	cams, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	for _, c := range cams {
		if !c.IsDir() {
			continue
		}
		evs, err := os.ReadDir(filepath.Join(root, c.Name()))
		if err != nil {
			continue
		}
		for _, e := range evs {
			if !e.IsDir() {
				continue
			}
			data, err := os.ReadFile(filepath.Join(root, c.Name(), e.Name(), "meta.json"))
			if err != nil {
				continue
			}
			var ev motion.Event
			if err := json.Unmarshal(data, &ev); err != nil {
				s.log.Warn("bad event meta", "path", e.Name(), "err", err)
				continue
			}
			s.events[ev.ID] = &ev
		}
	}
	return nil
}

// SaveEvent upserts event metadata to disk and the index.
func (s *Store) SaveEvent(ev *motion.Event) error {
	dir := s.EventDir(ev)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(ev, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, "meta.json.tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(dir, "meta.json")); err != nil {
		return err
	}
	s.mu.Lock()
	s.events[ev.ID] = ev
	s.mu.Unlock()
	return nil
}

// GetEvent returns one event by ID.
func (s *Store) GetEvent(id string) (*motion.Event, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ev, ok := s.events[id]
	return ev, ok
}

// ListEvents returns events, optionally filtered, newest first.
func (s *Store) ListEvents(camera string, from, to time.Time, limit int) []*motion.Event {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*motion.Event
	for _, ev := range s.events {
		if camera != "" && ev.Camera != camera {
			continue
		}
		if !from.IsZero() && ev.End.Before(from) {
			continue
		}
		if !to.IsZero() && ev.Start.After(to) {
			continue
		}
		out = append(out, ev)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start.After(out[j].Start) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// DeleteEvent removes an event directory and its index entry.
func (s *Store) DeleteEvent(id string) error {
	s.mu.RLock()
	ev, ok := s.events[id]
	s.mu.RUnlock()
	if !ok {
		return fmt.Errorf("event %q not found", id)
	}
	if err := os.RemoveAll(s.EventDir(ev)); err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.events, id)
	s.mu.Unlock()
	return nil
}

// --- recordings ------------------------------------------------------------

// SegmentTime parses a segment file name into its start time.
func SegmentTime(name string) (time.Time, bool) {
	base := strings.TrimSuffix(name, ".mp4")
	for _, layout := range []string{segmentTimeFormat, segmentTimeFormatLegacy} {
		if t, err := time.ParseInLocation(layout, base, time.Local); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// Recording is one continuous segment, exposed via the API.
type Recording struct {
	Camera string    `json:"camera"`
	File   string    `json:"file"`
	Start  time.Time `json:"start"`
	End    time.Time `json:"end"`
	Size   int64     `json:"size"`
}

// ListRecordings lists segments for a camera, optionally within a time range.
func (s *Store) ListRecordings(cam string, from, to time.Time) []Recording {
	dir := s.CameraRecordingsDir(cam)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []Recording
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".mp4") {
			continue
		}
		start, ok := SegmentTime(e.Name())
		if !ok {
			continue
		}
		end := start.Add(time.Hour)
		if !from.IsZero() && end.Before(from) {
			continue
		}
		if !to.IsZero() && start.After(to) {
			continue
		}
		var size int64
		if fi, err := e.Info(); err == nil {
			size = fi.Size()
		}
		out = append(out, Recording{Camera: cam, File: e.Name(), Start: start, End: end, Size: size})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out
}

// segmentsForRange returns segment files whose hour window overlaps
// [from, to], with computed boundaries.
func (s *Store) segmentsForRange(cam string, from, to time.Time) []Recording {
	return s.ListRecordings(cam, from, to)
}

// segmentClosed reports whether the recorder has finished (rotated past) a
// segment. A segment is closed once its hour end has passed — an open
// segment has no moov atom and cannot be read by ffmpeg.
func segmentClosed(r Recording, now time.Time) bool {
	return !r.End.After(now)
}

// MakeEventClips extracts precise stream-copy clips of [start-buffer,
// end+buffer] from the underlying continuous segments into the event
// directory. If any needed segment is still being recorded, the event is
// flagged ClipsPending and the per-minute sweeper retries until the hour
// completes. Clips are small (event length, stream copy) and playable.
// ErrUnrecoverable indicates a clip source segment is closed (not being
// written) yet unreadable — retrying will never help.
var ErrUnrecoverable = errors.New("unrecoverable clip source")

func (s *Store) MakeEventClips(ev *motion.Event) error {
	cam := s.cfg.Camera(ev.Camera)
	if cam == nil {
		return fmt.Errorf("unknown camera %q", ev.Camera)
	}
	m := cam.MotionSettings()
	from := ev.Start.Add(-m.Buffer.Duration)
	to := ev.End.Add(m.Buffer.Duration)
	segs := s.segmentsForRange(ev.Camera, from, to)
	if len(segs) == 0 {
		return fmt.Errorf("%w: no segments for event %s", ErrUnrecoverable, ev.ID)
	}
	var clips []string
	// A healthy clip covers the event plus buffers; anything dramatically
	// smaller is truncated/corrupt and gets re-extracted.
	expectSecs := (ev.End.Sub(ev.Start) + 2*m.Buffer.Duration) / time.Second
	if expectSecs < 5 {
		expectSecs = 5
	}
	minClipSize := int64(expectSecs) * (20 << 10) // ≥ ~160 kbps floor
	for _, seg := range segs {
		clipName := "clip-" + strings.TrimSuffix(seg.File, ".mp4") + ".mp4"
		dst := filepath.Join(s.EventDir(ev), clipName)
		if fi, err := os.Stat(dst); err == nil && fi.Size() >= minClipBytes && fi.Size() >= minClipSize {
			clips = append(clips, clipName)
			continue
		}
		sliceFrom := from
		if sliceFrom.Before(seg.Start) {
			sliceFrom = seg.Start
		}
		sliceTo := to
		if sliceTo.After(seg.End) {
			sliceTo = seg.End
		}
		if err := s.extractClip(filepath.Join(s.CameraRecordingsDir(ev.Camera), seg.File), dst, sliceFrom.Sub(seg.Start), sliceTo.Sub(seg.Start), minClipSize); err != nil {
			if segmentClosed(seg, time.Now()) {
				// segment finished but unreadable (orphaned by a pre-fragmented
				// MP4 restart) or already rotated away — retrying is pointless
				return fmt.Errorf("%w: %v", ErrUnrecoverable, err)
			}
			s.log.Debug("extract clip failed", "err", err, "segment", seg.File)
			continue
		}
		clips = append(clips, clipName)
	}
	if len(clips) == 0 {
		ev.ClipsPending = true
		_ = s.SaveEvent(ev)
		return fmt.Errorf("no clips extracted for %s yet", ev.ID)
	}
	ev.Clips = clips
	ev.ClipsPending = false
	return s.SaveEvent(ev)
}

// extractClip stream-copies [from,to] of a source segment. Output smaller
// than minSize is treated as failure (empty/garbage seek result).
func (s *Store) extractClip(src, dst string, from, to time.Duration, minSize int64) error {
	if from < 0 {
		from = 0
	}
	if to <= from {
		return fmt.Errorf("empty clip range")
	}
	tmp := dst + ".tmp.mp4"
	// -ss before -i seeks to the nearest preceding keyframe; -t (not -to)
	// limits how much is read from the input after that seek.
	dur := to - from
	if dur < 0 {
		dur = 0
	}
	args := []string{
		"-hide_banner", "-loglevel", "error",
		"-ss", secondsArg(from), "-t", secondsArg(dur + 2*time.Second),
		"-i", src,
		"-map", "0", "-dn",
		"-c", "copy",
		"-avoid_negative_ts", "make_zero",
		"-movflags", "+faststart",
		"-y", tmp,
	}
	cmd := exec.Command(s.ffmpegPath, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("ffmpeg clip: %v: %s", err, firstN(string(out), 300))
	}
	if fi, err := os.Stat(tmp); err != nil || fi.Size() < minSize {
		os.Remove(tmp)
		return fmt.Errorf("clip output too small (seek past end of source?)")
	}
	return os.Rename(tmp, dst)
}

func secondsArg(d time.Duration) string {
	return fmt.Sprintf("%.2f", d.Seconds())
}

func firstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// SweepPendingClips retries clip extraction for recent events still waiting
// on their hour segment to complete, converts legacy full-hour copies
// (pre-clip-extraction events) into precise clips, and drops junk events
// that never produced clips.
func (s *Store) SweepPendingClips() {
	now := time.Now()
	for _, ev := range s.ListEvents("", time.Time{}, time.Time{}, 0) {
		if ev.ClipsPending {
			if now.Sub(ev.End) > 26*time.Hour {
				_ = s.DeleteEvent(ev.ID) // source segments will never become readable
				continue
			}
			if err := s.MakeEventClips(ev); err != nil {
				if errors.Is(err, ErrUnrecoverable) {
					s.log.Info("dropping event with unreadable segments", "event", ev.ID)
					_ = s.DeleteEvent(ev.ID)
					continue
				}
				if s.noteClipAttempt(ev.ID) > 10 {
					s.log.Info("dropping event: clips never became ready", "event", ev.ID)
					_ = s.DeleteEvent(ev.ID)
					continue
				}
				s.log.Debug("pending clips", "event", ev.ID, "err", err)
				continue
			}
			s.forgetClipAttempt(ev.ID)
			s.log.Info("event clips ready", "event", ev.ID, "clips", len(ev.Clips))
			s.hub()
			continue
		}
		if len(ev.Clips) == 0 {
			if now.Sub(ev.End) > 24*time.Hour {
				_ = s.DeleteEvent(ev.ID) // never produced clips: junk
				continue
			}
			if err := s.MakeEventClips(ev); err != nil {
				if errors.Is(err, ErrUnrecoverable) {
					s.log.Info("dropping event with unreadable segments", "event", ev.ID)
					_ = s.DeleteEvent(ev.ID)
				}
				continue
			}
			s.log.Info("event clips ready", "event", ev.ID, "clips", len(ev.Clips))
			s.hub()
			continue
		}
		// legacy migration: clips that are full-hour copies (huge) → precise
		if s.hasLegacyHourClips(ev) {
			s.log.Info("migrating legacy event clips", "event", ev.ID)
			old := append([]string(nil), ev.Clips...)
			if err := s.MakeEventClips(ev); err == nil {
				for _, c := range old {
					if !strings.HasPrefix(c, "clip-") && !strings.Contains(c, "..") && filepath.Base(c) == c {
						_ = os.Remove(filepath.Join(s.EventDir(ev), c))
					}
				}
				s.hub()
			} else if errors.Is(err, ErrUnrecoverable) || now.Sub(ev.End) > 26*time.Hour {
				s.log.Info("dropping unwatchable legacy event", "event", ev.ID)
				_ = s.DeleteEvent(ev.ID)
			} else {
				s.log.Debug("legacy migration waiting", "event", ev.ID, "err", err)
			}
			continue
		}
		// repair truncated/undersized clips for recent events
		if now.Sub(ev.End) < 24*time.Hour && s.hasUndersizedClips(ev) {
			if s.noteClipAttempt(ev.ID) > 10 {
				s.log.Info("dropping event with unrepairable clips", "event", ev.ID)
				_ = s.DeleteEvent(ev.ID)
				continue
			}
			if err := s.MakeEventClips(ev); err != nil {
				if errors.Is(err, ErrUnrecoverable) {
					s.log.Info("dropping event with unreadable segments", "event", ev.ID)
					_ = s.DeleteEvent(ev.ID)
				} else {
					s.log.Debug("clip repair", "event", ev.ID, "err", err)
				}
			} else if !s.hasUndersizedClips(ev) {
				s.forgetClipAttempt(ev.ID)
				s.hub()
			}
		}
	}
}

var clipAttempts sync.Map // eventID -> int

func (s *Store) noteClipAttempt(id string) int {
	v, _ := clipAttempts.LoadOrStore(id, 0)
	n := v.(int) + 1
	clipAttempts.Store(id, n)
	return n
}

func (s *Store) forgetClipAttempt(id string) { clipAttempts.Delete(id) }

func (s *Store) hasUndersizedClips(ev *motion.Event) bool {
	cam := s.cfg.Camera(ev.Camera)
	if cam == nil {
		return false
	}
	m := cam.MotionSettings()
	expectSecs := (ev.End.Sub(ev.Start) + 2*m.Buffer.Duration) / time.Second
	if expectSecs < 5 {
		expectSecs = 5
	}
	minSize := int64(expectSecs) * (20 << 10)
	dir := s.EventDir(ev)
	for _, c := range ev.Clips {
		if fi, err := os.Stat(filepath.Join(dir, c)); err != nil || fi.Size() < minSize {
			return true
		}
	}
	return false
}

const legacyClipMinBytes = 40 << 20 // a precise clip is minutes-scale; hour copies are hundreds of MB

// minClipBytes: clips smaller than this are treated as corrupt/truncated
// and re-extracted (a buffered event clip at any sane bitrate is larger).
const minClipBytes = 40 << 10

func (s *Store) hasLegacyHourClips(ev *motion.Event) bool {
	dir := s.EventDir(ev)
	for _, c := range ev.Clips {
		if !strings.HasPrefix(c, "clip-") {
			if fi, err := os.Stat(filepath.Join(dir, c)); err == nil && fi.Size() > legacyClipMinBytes {
				return true
			}
		}
	}
	return false
}

// SaveThumbnail stores an event thumbnail JPEG.
func (s *Store) SaveThumbnail(ev *motion.Event, data []byte) error {
	dir := s.EventDir(ev)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	ev.Thumbnail = "thumbnail.jpg"
	return os.WriteFile(filepath.Join(dir, "thumbnail.jpg"), data, 0o644)
}

// --- retention ---------------------------------------------------------------

// CleanContinuous deletes continuous segments older than the configured
// retention window.
func (s *Store) CleanContinuous() int {
	cutoff := time.Now().AddDate(0, 0, -s.cfg.Retention.ContinuousDays)
	var removed int
	for _, cam := range s.cfg.Cameras {
		dir := s.CameraRecordingsDir(cam.Name)
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".mp4") {
				continue
			}
			start, ok := SegmentTime(e.Name())
			if !ok {
				continue
			}
			if start.Add(time.Hour).Before(cutoff) {
				if err := os.Remove(filepath.Join(dir, e.Name())); err == nil {
					removed++
				}
			}
		}
	}
	if removed > 0 {
		s.log.Info("retention: removed continuous segments", "count", removed)
	}
	s.enforceDiskBudget()
	return removed
}

// enforceDiskBudget prunes the oldest continuous segments beyond the
// configured max_total_gb so a high-bitrate camera cannot fill the disk.
func (s *Store) enforceDiskBudget() {
	budget := int64(s.cfg.Retention.MaxTotalGB) << 30
	if budget <= 0 {
		return
	}
	type seg struct {
		path  string
		start time.Time
		size  int64
	}
	var all []seg
	var total int64
	for _, cam := range s.cfg.Cameras {
		dir := s.CameraRecordingsDir(cam.Name)
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".mp4") {
				continue
			}
			start, ok := SegmentTime(e.Name())
			if !ok {
				continue
			}
			fi, err := e.Info()
			if err != nil {
				continue
			}
			all = append(all, seg{filepath.Join(dir, e.Name()), start, fi.Size()})
			total += fi.Size()
		}
	}
	if total <= budget {
		s.enforceEventBudget(budget - total)
		return
	}
	sort.Slice(all, func(i, j int) bool { return all[i].start.Before(all[j].start) })
	var removed int
	for _, sg := range all {
		if total <= budget {
			break
		}
		if err := os.Remove(sg.path); err == nil {
			total -= sg.size
			removed++
		}
	}
	if removed > 0 {
		s.log.Warn("retention: disk budget hit, pruned oldest segments",
			"count", removed, "budget_gb", s.cfg.Retention.MaxTotalGB)
	}
	s.enforceEventBudget(budget - total)
}

// enforceEventBudget prunes the oldest motion events (dir tree size)
// beyond the bytes remaining in the disk budget. Long events at high
// bitrates can otherwise accumulate unbounded over the 30-day policy.
func (s *Store) enforceEventBudget(remaining int64) {
	if remaining < 0 {
		remaining = 0
	}
	type edir struct {
		path  string
		start time.Time
		size  int64
	}
	var all []edir
	var total int64
	for _, ev := range s.ListEvents("", time.Time{}, time.Time{}, 0) {
		p := s.EventDir(ev)
		var sz int64
		_ = filepath.Walk(p, func(_ string, fi os.FileInfo, err error) error {
			if err == nil && !fi.IsDir() {
				sz += fi.Size()
			}
			return nil
		})
		all = append(all, edir{p, ev.Start, sz})
		total += sz
	}
	if total <= remaining {
		return
	}
	sort.Slice(all, func(i, j int) bool { return all[i].start.Before(all[j].start) })
	var removed int
	for _, e := range all {
		if total <= remaining {
			break
		}
		if err := os.RemoveAll(e.path); err == nil {
			s.mu.Lock()
			delete(s.events, filepath.Base(e.path))
			s.mu.Unlock()
			total -= e.size
			removed++
		}
	}
	if removed > 0 {
		s.log.Warn("retention: disk budget hit, pruned oldest events",
			"count", removed, "budget_gb", s.cfg.Retention.MaxTotalGB)
		s.hub()
	}
}

// CleanEvents deletes motion events older than the configured motion
// retention window.
func (s *Store) CleanEvents() int {
	cutoff := time.Now().AddDate(0, 0, -s.cfg.Retention.MotionDays)
	var removed int
	for _, ev := range s.ListEvents("", time.Time{}, time.Time{}, 0) {
		if ev.End.Before(cutoff) {
			if err := s.DeleteEvent(ev.ID); err == nil {
				removed++
			}
		}
	}
	if removed > 0 {
		s.log.Info("retention: removed motion events", "count", removed)
	}
	return removed
}

// RunCleaner prunes media on startup and then hourly; clip extraction
// pending-queue sweeps run every minute.
func (s *Store) RunCleaner(ctx context.Context) {
	s.CleanContinuous()
	s.CleanEvents()
	s.SweepPendingClips()
	clipTicker := time.NewTicker(time.Minute)
	defer clipTicker.Stop()
	hourly := time.NewTicker(time.Hour)
	defer hourly.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-clipTicker.C:
			s.SweepPendingClips()
		case <-hourly.C:
			s.CleanContinuous()
			s.CleanEvents()
		}
	}
}
