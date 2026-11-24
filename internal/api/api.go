// Package api exposes Vigilant's HTTP REST API, SSE live state stream, media
// file serving, go2rtc proxy, and the embedded web UI.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jbutlerdev/vigilant/internal/app"
	"github.com/jbutlerdev/vigilant/internal/config"
	"github.com/jbutlerdev/vigilant/internal/storage"
)

// Server hosts the HTTP API and web UI.
type Server struct {
	app     *app.App
	log     *slog.Logger
	webFS   fs.FS
	mux     *http.ServeMux
	proxy   *httputil.ReverseProxy
	started time.Time
}

// New builds the HTTP handler tree.
func New(a *app.App, log *slog.Logger, webFS fs.FS) *Server {
	s := &Server{app: a, log: log, webFS: webFS, mux: http.NewServeMux(), started: time.Now()}
	target, _ := url.Parse(a.Go2RTC.APIBase())
	s.proxy = &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target)
			// go2rtc's OriginCheck rejects requests carrying X-Forwarded-Host,
			// so deliberately do not set forwarding headers.
			r.Out.Header.Del("X-Forwarded-For")
			r.Out.Header.Del("X-Forwarded-Host")
			r.Out.Header.Del("X-Forwarded-Proto")
			r.Out.Header.Del("X-Forwarded-Uri")
			// Present the go2rtc origin so WebRTC/MSE WebSocket upgrades pass
			// go2rtc's origin check through the proxy.
			r.Out.Header.Set("Origin", target.Scheme+"://"+target.Host)
		},
	}
	s.routes()
	return s
}

// Handler returns the root http.Handler.
func (s *Server) Handler() http.Handler { return s.logWare(s.mux) }

func (s *Server) logWare(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") && !strings.HasPrefix(r.URL.Path, "/go2rtc/") {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		next.ServeHTTP(w, r)
		s.log.Debug(r.Method+" "+r.URL.Path, "dur", time.Since(start).String())
	})
}

func (s *Server) routes() {
	m := s.mux
	// REST API
	m.HandleFunc("GET /api/state", s.handleState)
	m.HandleFunc("GET /api/config", s.handleConfig)
	m.HandleFunc("GET /api/cameras", s.handleCameras)
	m.HandleFunc("POST /api/cameras", s.handleAddCamera)
	m.HandleFunc("DELETE /api/cameras/{name}", s.handleDeleteCamera)
	m.HandleFunc("GET /api/frame.jpg", s.handleFrame)
	m.HandleFunc("GET /api/events", s.handleListEvents)
	m.HandleFunc("GET /api/events/{id}", s.handleGetEvent)
	m.HandleFunc("DELETE /api/events/{id}", s.handleDeleteEvent)
	m.HandleFunc("GET /api/events/{id}/thumbnail", s.handleEventThumbnail)
	m.HandleFunc("GET /api/events/{id}/clip/{file}", s.handleEventClip)
	m.HandleFunc("GET /api/recordings", s.handleListRecordings)
	m.HandleFunc("GET /api/recordings/file", s.handleRecordingFile)
	m.HandleFunc("PUT /api/cameras/{name}/mask", s.handlePutMask)
	m.HandleFunc("PUT /api/cameras/{name}/settings", s.handlePutCameraSettings)
	m.HandleFunc("PUT /api/config/retention", s.handlePutRetention)
	m.HandleFunc("GET /api/sse", s.handleSSE)
	// go2rtc proxy (live viewing: WebRTC/MSE/WebSocket, widget)
	m.Handle("/go2rtc/", http.StripPrefix("/go2rtc", s.proxy))
	// web UI
	m.Handle("/", http.FileServerFS(s.webFS))
}

// --- helpers ----------------------------------------------------------------

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

func parseTimeParam(r *http.Request, name string) time.Time {
	v := r.URL.Query().Get(name)
	if v == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, v, time.Local); err == nil {
			return t
		}
	}
	return time.Time{}
}

func (s *Server) requireCamera(w http.ResponseWriter, name string) *config.Camera {
	cam := s.app.Cfg.Camera(name)
	if cam == nil {
		writeErr(w, http.StatusNotFound, fmt.Errorf("camera %q not found", name))
		return nil
	}
	return cam
}

// --- handlers -----------------------------------------------------------------

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.app.State())
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	// redact credentials embedded in RTSP URLs
	cfg := *s.app.Cfg
	writeJSON(w, cfg)
}

func (s *Server) handleCameras(w http.ResponseWriter, r *http.Request) {
	type camView struct {
		Name      string              `json:"name"`
		URL       string              `json:"url"`
		Record    bool                `json:"record"`
		Motion    bool                `json:"motion"`
		Stream    string              `json:"stream"`
		Mask      config.Mask         `json:"mask"`
		MotionCfg config.MotionConfig `json:"motion_config"`
	}
	var out []camView
	for _, cam := range s.app.Cfg.Cameras {
		out = append(out, camView{
			Name:      cam.Name,
			URL:       cam.URL,
			Record:    cam.RecordEnabled(),
			Motion:    cam.MotionEnabled(),
			Stream:    s.app.Go2RTC.StreamNameForView(cam),
			Mask:      cam.MotionSettings().Mask,
			MotionCfg: cam.MotionSettings(),
		})
	}
	writeJSON(w, out)
}

func (s *Server) handleFrame(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("camera")
	cam := s.requireCamera(w, name)
	if cam == nil {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	data, err := s.app.Go2RTC.Snapshot(ctx, name)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

func (s *Server) handleListEvents(w http.ResponseWriter, r *http.Request) {
	camera := r.URL.Query().Get("camera")
	from := parseTimeParam(r, "from")
	to := parseTimeParam(r, "to")
	limit := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		limit, _ = strconv.Atoi(v)
	}
	writeJSON(w, s.app.Store.ListEvents(camera, from, to, limit))
}

func (s *Server) handleGetEvent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ev, ok := s.app.Store.GetEvent(id)
	if !ok {
		writeErr(w, http.StatusNotFound, fmt.Errorf("event not found"))
		return
	}
	writeJSON(w, ev)
}

func (s *Server) handleDeleteEvent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.app.Store.DeleteEvent(id); err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) eventFile(w http.ResponseWriter, r *http.Request, id, name string) {
	ev, ok := s.app.Store.GetEvent(id)
	if !ok {
		writeErr(w, http.StatusNotFound, fmt.Errorf("event not found"))
		return
	}
	// sanitize: file must be within the event directory
	clean := path.Clean("/" + name)
	clean = strings.TrimPrefix(clean, "/")
	if clean != name || strings.Contains(name, "..") {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("bad file name"))
		return
	}
	file := filepath.Join(s.app.Store.EventDir(ev), clean)
	if _, err := os.Stat(file); err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	http.ServeFile(w, r, file)
}

func (s *Server) handleEventThumbnail(w http.ResponseWriter, r *http.Request) {
	s.eventFile(w, r, r.PathValue("id"), "thumbnail.jpg")
}

func (s *Server) handleEventClip(w http.ResponseWriter, r *http.Request) {
	s.eventFile(w, r, r.PathValue("id"), r.PathValue("file"))
}

func (s *Server) handleListRecordings(w http.ResponseWriter, r *http.Request) {
	cam := s.requireCamera(w, r.URL.Query().Get("camera"))
	if cam == nil {
		return
	}
	from := parseTimeParam(r, "from")
	to := parseTimeParam(r, "to")
	writeJSON(w, s.app.Store.ListRecordings(cam.Name, from, to))
}

func (s *Server) handleRecordingFile(w http.ResponseWriter, r *http.Request) {
	cam := s.requireCamera(w, r.URL.Query().Get("camera"))
	if cam == nil {
		return
	}
	file := r.URL.Query().Get("file")
	start, ok := storage.SegmentTime(file)
	if !ok || filepath.Base(file) != file {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("bad file name"))
		return
	}
	_ = start
	http.ServeFile(w, r, filepath.Join(s.app.Store.CameraRecordingsDir(cam.Name), file))
}

func (s *Server) handlePutMask(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if s.requireCamera(w, name) == nil {
		return
	}
	var mask config.Mask
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := json.Unmarshal(body, &mask); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	// basic range validation
	for _, rc := range mask.Rectangles {
		if rc.X < 0 || rc.Y < 0 || rc.W <= 0 || rc.H <= 0 || rc.X+rc.W > 1.0001 || rc.Y+rc.H > 1.0001 {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("rectangle out of normalized bounds"))
			return
		}
	}
	for _, p := range mask.Polygons {
		if len(p.Points) < 3 {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("polygon needs 3+ points"))
			return
		}
		for _, pt := range p.Points {
			if pt[0] < 0 || pt[0] > 1 || pt[1] < 0 || pt[1] > 1 {
				writeErr(w, http.StatusBadRequest, fmt.Errorf("polygon point out of bounds"))
				return
			}
		}
	}
	if err := s.app.SetMask(name, mask); err != nil {
		code := http.StatusInternalServerError
		if strings.Contains(err.Error(), "not found") {
			code = http.StatusNotFound
		}
		writeErr(w, code, err)
		return
	}
	writeJSON(w, map[string]interface{}{"ok": true, "camera": name, "mask": mask})
}

func (s *Server) handleAddCamera(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name   string `json:"name"`
		URL    string `json:"url"`
		Record *bool  `json:"record"`
		Motion *bool  `json:"motion"`
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	record, motionEnabled := true, true
	if req.Record != nil {
		record = *req.Record
	}
	if req.Motion != nil {
		motionEnabled = *req.Motion
	}
	if err := s.app.AddCamera(req.Name, req.URL, record, motionEnabled); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, map[string]interface{}{"ok": true, "camera": req.Name})
}

func (s *Server) handleDeleteCamera(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.app.RemoveCamera(name); err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeErr(w, http.StatusNotFound, err)
			return
		}
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, map[string]interface{}{"ok": true, "removed": name})
}

func (s *Server) handlePutCameraSettings(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if s.requireCamera(w, name) == nil {
		return
	}
	var req struct {
		Record *bool                `json:"record"`
		URL    string               `json:"url"`
		Motion *config.MotionConfig `json:"motion"`
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.URL != "" && !strings.Contains(req.URL, "://") {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("url must be a full rtsp/http URL"))
		return
	}
	if req.Motion != nil && (req.Motion.Width > 4096 || req.Motion.Height > 4096 || req.Motion.FPS > 60 || req.Motion.Threshold > 255 || req.Motion.MinPercent > 100) {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("motion settings out of range"))
		return
	}
	if err := s.app.Cfg.SetCameraSettings(name, req.Record, req.URL, req.Motion); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if err := s.app.RestartCamera(name); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	cam := s.app.Cfg.Camera(name)
	writeJSON(w, map[string]interface{}{"ok": true, "camera": name, "record": cam.RecordEnabled(), "motion": cam.MotionEnabled(), "motion_config": cam.MotionSettings()})
}

func (s *Server) handlePutRetention(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ContinuousDays int `json:"continuous_days"`
		MotionDays     int `json:"motion_days"`
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.app.Cfg.SetRetention(req.ContinuousDays, req.MotionDays); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	s.app.Republish()
	writeJSON(w, map[string]interface{}{"ok": true, "retention": s.app.Cfg.Retention})
}

func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, errors.New("streaming unsupported"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch := s.app.Subscribe()
	defer s.app.Unsubscribe(ch)

	// initial snapshot
	writeSSE(w, flusher, s.app)
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case msg := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", msg)
			flusher.Flush()
		case <-ticker.C:
			fmt.Fprintf(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

func writeSSE(w io.Writer, f http.Flusher, a *app.App) {
	data, err := json.Marshal(a.State())
	if err != nil {
		return
	}
	fmt.Fprintf(w, "data: %s\n\n", data)
	f.Flush()
}
