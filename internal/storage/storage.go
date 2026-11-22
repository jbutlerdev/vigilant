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
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jbutlerdev/vigilant/internal/config"
	"github.com/jbutlerdev/vigilant/internal/motion"
)

// segmentTimeFormat matches the recorder's strftime pattern.
const segmentTimeFormat = "2006-01-02_15"

// Store manages the data directory and the event index.
type Store struct {
	cfg    *config.Config
	log    *slog.Logger
	mu     sync.RWMutex
	events map[string]*motion.Event
}

// New creates a store rooted at cfg.DataDir.
func New(cfg *config.Config, log *slog.Logger) (*Store, error) {
	s := &Store{cfg: cfg, log: log, events: map[string]*motion.Event{}}
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
	t, err := time.ParseInLocation(segmentTimeFormat, base, time.Local)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
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

// segmentsForRange returns segment file names whose hour window overlaps
// [from, to].
func (s *Store) segmentsForRange(cam string, from, to time.Time) []string {
	var names []string
	for _, r := range s.ListRecordings(cam, from, to) {
		names = append(names, r.File)
	}
	return names
}

// CopyEventSegments copies the continuous segments overlapping an event
// (plus a configured buffer on each side) into the event directory under
// the motion retention policy.
func (s *Store) CopyEventSegments(ev *motion.Event) error {
	m := s.cfg.Camera(ev.Camera).MotionSettings()
	from := ev.Start.Add(-m.Buffer.Duration)
	to := ev.End.Add(m.Buffer.Duration)
	files := s.segmentsForRange(ev.Camera, from, to)
	if len(files) == 0 {
		return fmt.Errorf("no segments found for event %s", ev.ID)
	}
	dir := s.EventDir(ev)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	src := s.CameraRecordingsDir(ev.Camera)
	var clips []string
	for _, f := range files {
		dst := filepath.Join(dir, f)
		if err := copyFile(filepath.Join(src, f), dst); err != nil {
			s.log.Error("copy segment", "err", err, "file", f)
			continue
		}
		clips = append(clips, f)
	}
	if len(clips) == 0 {
		return fmt.Errorf("all segment copies failed for %s", ev.ID)
	}
	ev.Clips = clips
	return s.SaveEvent(ev)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
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
	return removed
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

// RunCleaner prunes media on startup and then hourly.
func (s *Store) RunCleaner(ctx context.Context) {
	s.CleanContinuous()
	s.CleanEvents()
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.CleanContinuous()
			s.CleanEvents()
		}
	}
}
