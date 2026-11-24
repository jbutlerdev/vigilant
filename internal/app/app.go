// Package app wires Vigilant's components together.
package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os/exec"
	"sync"
	"time"

	"github.com/jbutlerdev/vigilant/internal/config"
	"github.com/jbutlerdev/vigilant/internal/go2rtc"
	"github.com/jbutlerdev/vigilant/internal/motion"
	"github.com/jbutlerdev/vigilant/internal/recorder"
	"github.com/jbutlerdev/vigilant/internal/storage"
)

// App owns the running components.
type App struct {
	Cfg    *config.Config
	Log    *slog.Logger
	Store  *storage.Store
	Go2RTC *go2rtc.Manager
	ffmpeg string

	mu        sync.RWMutex
	detectors map[string]*motion.Detector
	active    map[string]*motion.Event // in-progress events per camera

	bgCtx     context.Context
	cancelCam map[string]context.CancelFunc

	hub *Hub
}

// New prepares an app from config.
func New(cfg *config.Config, log *slog.Logger) (*App, error) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		return nil, fmt.Errorf("ffmpeg is required but was not found in PATH: %w", err)
	}
	store, err := storage.New(cfg, log, ffmpeg, nil)
	if err != nil {
		return nil, err
	}
	return &App{
		Cfg:       cfg,
		Log:       log,
		Store:     store,
		Go2RTC:    go2rtc.New(cfg),
		ffmpeg:    ffmpeg,
		detectors: map[string]*motion.Detector{},
		active:    map[string]*motion.Event{},
		cancelCam: map[string]context.CancelFunc{},
		hub:       NewHub(),
	}, nil
}

// wire store change notification after the app (and its hub) exists
func (a *App) setStoreHooks() {
	if a.Store != nil {
		a.Store.SetOnChange(a.Republish)
	}
}

// Start launches go2rtc, recorders, detectors, and the retention cleaner.
func (a *App) Start(ctx context.Context) error {
	a.setStoreHooks()
	if err := a.Go2RTC.Start(ctx); err != nil {
		return err
	}
	a.mu.Lock()
	a.bgCtx = ctx
	for _, cam := range a.Cfg.Cameras {
		a.startCameraLocked(ctx, cam)
	}
	a.mu.Unlock()
	a.hub.Broadcast(a.State())
	go func() {
		// republish state periodically so SSE clients stay fresh
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				a.hub.Broadcast(a.State())
			}
		}
	}()
	return nil
}

// startCameraLocked launches recorder/detector goroutines for a camera on
// ctx. Callers must hold a.mu.
func (a *App) startCameraLocked(ctx context.Context, cam *config.Camera) {
	if cam.RecordEnabled() {
		src := fmt.Sprintf("%s/%s", a.Go2RTC.RTSPBase(), cam.Name)
		r := recorder.New(cam, src, a.Store.CameraRecordingsDir(cam.Name), a.ffmpeg, a.Log)
		go r.Run(ctx)
	}
	if cam.MotionEnabled() {
		stream := a.Go2RTC.StreamNameForMotion(cam)
		src := fmt.Sprintf("%s/%s", a.Go2RTC.RTSPBase(), stream)
		d := motion.NewDetector(cam, src, a.ffmpeg, a, a.Log)
		a.detectors[cam.Name] = d
		go d.Run(ctx)
	}
}

// RestartCamera tears down and relaunches a camera's pipelines so edited
// settings (frame size, fps, substream URL, record/motion toggles) apply.
func (a *App) RestartCamera(name string) error {
	err := func() error {
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.bgCtx == nil {
			return fmt.Errorf("app not started")
		}
		cam := a.Cfg.Camera(name)
		if cam == nil {
			return fmt.Errorf("camera %q not found", name)
		}
		if cancel, ok := a.cancelCam[name]; ok {
			cancel()
		}
		ctx, cancel := context.WithCancel(a.bgCtx)
		a.cancelCam[name] = cancel
		delete(a.detectors, name)
		if cam.RecordEnabled() || cam.MotionEnabled() {
			a.startCameraLocked(ctx, cam)
		}
		return nil
	}()
	if err != nil {
		return err
	}
	a.Republish()
	return nil
}

// Stop shuts background processes down.
func (a *App) Stop() { a.Go2RTC.Stop() }

// Detector returns a camera's detector if running.
func (a *App) Detector(cam string) *motion.Detector {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.detectors[cam]
}

// --- motion.Handler ---------------------------------------------------------

// OnMotionStart records the in-progress event and grabs a thumbnail.
func (a *App) OnMotionStart(camName string, start time.Time) {
	ev := &motion.Event{
		ID:     storage.NewID(camName, start),
		Camera: camName,
		Start:  start,
		End:    start,
	}
	a.mu.Lock()
	a.active[camName] = ev
	a.mu.Unlock()

	snapCtx, snapCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer snapCancel()
	if data, err := a.Go2RTC.Snapshot(snapCtx, camName); err == nil {
		if err := a.Store.SaveThumbnail(ev, data); err != nil {
			a.Log.Warn("save thumbnail", "err", err)
		}
	}
	_ = a.Store.SaveEvent(ev)
	a.hub.Broadcast(a.State())
}

// OnMotionEnd finalizes an event: copies segments to the motion retention
// area and announces it.
func (a *App) OnMotionEnd(ev motion.Event) {
	a.mu.Lock()
	cur := a.active[ev.Camera]
	delete(a.active, ev.Camera)
	a.mu.Unlock()

	if cur != nil {
		ev.ID = cur.ID
		if !cur.Start.IsZero() && cur.Start.Before(ev.Start) {
			ev.Start = cur.Start
		}
		if ev.Thumbnail == "" {
			ev.Thumbnail = cur.Thumbnail
		}
	}
	if ev.ID == "" {
		ev.ID = storage.NewID(ev.Camera, ev.Start)
	}
	stored := &ev
	if err := a.Store.SaveEvent(stored); err != nil {
		a.Log.Error("save event", "err", err)
		return
	}
	a.hub.Broadcast(a.State())

	if err := a.Store.MakeEventClips(stored); err != nil {
		a.Log.Warn("make event clips", "err", err, "event", ev.ID) // sweeper retries
		a.hub.Broadcast(a.State())
		return
	}
	a.hub.Broadcast(a.State())
}

// Republish broadcasts a fresh state snapshot to SSE clients.
func (a *App) Republish() { go a.hub.Broadcast(a.State()) }

// Subscribe registers an SSE client.
func (a *App) Subscribe() chan string { return a.hub.Subscribe() }

// Unsubscribe removes an SSE client.
func (a *App) Unsubscribe(ch chan string) { a.hub.Unsubscribe(ch) }

// --- state ------------------------------------------------------------------

// State is the aggregate live state served over the API/SSE.
type State struct {
	Now       time.Time        `json:"now"`
	Cameras   []CameraState    `json:"cameras"`
	Retention config.Retention `json:"retention"`
}

// CameraState is live status for one camera.
type CameraState struct {
	Name        string         `json:"name"`
	Recording   bool           `json:"recording"`
	Motion      bool           `json:"motion_enabled"`
	Detector    *motion.Status `json:"detector,omitempty"`
	ActiveEvent *motion.Event  `json:"active_event,omitempty"`
}

// State snapshots the current system state.
func (a *App) State() State {
	st := State{Now: time.Now(), Retention: a.Cfg.Retention}
	for _, cam := range a.Cfg.Cameras {
		cs := CameraState{
			Name:      cam.Name,
			Recording: cam.RecordEnabled(),
			Motion:    cam.MotionEnabled(),
		}
		if d := a.Detector(cam.Name); d != nil {
			s := d.Status()
			cs.Detector = &s
		}
		a.mu.RLock()
		if ev, ok := a.active[cam.Name]; ok {
			cp := *ev
			cp.End = time.Now()
			cs.ActiveEvent = &cp
		}
		a.mu.RUnlock()
		st.Cameras = append(st.Cameras, cs)
	}
	return st
}

// SetMask updates a camera's mask everywhere (config file + live detector).
func (a *App) SetMask(camName string, mask config.Mask) error {
	if a.Cfg.Camera(camName) == nil {
		return fmt.Errorf("camera %q not found", camName)
	}
	if err := a.Cfg.SetMask(camName, mask); err != nil {
		return err
	}
	if d := a.Detector(camName); d != nil {
		d.SetMask(mask)
	}
	a.hub.Broadcast(a.State())
	return nil
}

// --- SSE hub -----------------------------------------------------------------

// Hub fans out JSON state snapshots to SSE clients.
type Hub struct {
	mu      sync.Mutex
	clients map[chan string]bool
}

// NewHub creates a hub.
func NewHub() *Hub { return &Hub{clients: map[chan string]bool{}} }

// Subscribe registers a client channel.
func (h *Hub) Subscribe() chan string {
	ch := make(chan string, 8)
	h.mu.Lock()
	h.clients[ch] = true
	h.mu.Unlock()
	return ch
}

// Unsubscribe removes a client.
func (h *Hub) Unsubscribe(ch chan string) {
	h.mu.Lock()
	delete(h.clients, ch)
	h.mu.Unlock()
	close(ch)
}

// Broadcast sends a JSON-encodable payload to all clients (non-blocking).
func (h *Hub) Broadcast(v interface{}) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	msg := string(data)
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.clients {
		select {
		case ch <- msg:
		default:
		}
	}
}
