// Package go2rtc manages the go2rtc restreaming child process. All camera
// consumption (recording, motion detection, live viewing) goes through
// go2rtc so Vigilant keeps exactly one connection per camera source.
package go2rtc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/jbutlerdev/vigilant/internal/config"
)

// Manager owns the go2rtc child process.
type Manager struct {
	cfg     *config.Config
	streams map[string][]string // stream name -> source URLs

	mu         sync.Mutex
	cmd        *exec.Cmd
	downloaded string // path of an auto-downloaded binary
}

// New creates a manager for the given config.
func New(cfg *config.Config) *Manager {
	return &Manager{cfg: cfg, streams: map[string][]string{}}
}

// BuildStreams derives the go2rtc stream table from camera definitions.
// Each camera publishes "<name>" (main stream, recorded/viewed) and, when a
// dedicated motion substream is configured, "<name>_motion".
func (m *Manager) BuildStreams() {
	m.streams = map[string][]string{}
	for _, cam := range m.cfg.Cameras {
		if cam.RecordEnabled() || !cam.MotionEnabled() {
			m.streams[cam.Name] = []string{cam.URL}
		}
		if cam.MotionEnabled() && cam.MotionSourceURL() != cam.URL {
			m.streams[cam.Name+"_motion"] = []string{cam.MotionSourceURL()}
		}
	}
}

// StreamNameForMotion returns the go2rtc stream the motion detector pulls.
func (m *Manager) StreamNameForMotion(cam *config.Camera) string {
	if cam.MotionEnabled() && cam.MotionSourceURL() != cam.URL {
		return cam.Name + "_motion"
	}
	return cam.Name
}

// StreamNameForView returns the go2rtc stream the web UI should show.
func (m *Manager) StreamNameForView(cam *config.Camera) string { return cam.Name }

// RTSPBase is the internal RTSP endpoint streams are pulled from.
func (m *Manager) RTSPBase() string { return "rtsp://" + m.cfg.Go2RTC.RTSPListen }

// APIBase is the internal go2rtc HTTP API endpoint.
func (m *Manager) APIBase() string { return "http://" + m.cfg.Go2RTC.APIListen }

func (m *Manager) writeConfig() (string, error) {
	conf := map[string]interface{}{
		"api":  map[string]interface{}{"listen": m.cfg.Go2RTC.APIListen},
		"rtsp": map[string]interface{}{"listen": m.cfg.Go2RTC.RTSPListen},
		"log":  "info",
	}
	if len(m.streams) > 0 {
		conf["streams"] = m.streams
	}
	data, err := yaml.Marshal(conf)
	if err != nil {
		return "", err
	}
	path := filepath.Join(m.cfg.DataDir, "go2rtc.yaml")
	if err := os.MkdirAll(m.cfg.DataDir, 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// ResolveBinary locates the go2rtc binary, auto-downloading the official
// release for this platform into the data directory when not present.
func (m *Manager) ResolveBinary(ctx context.Context) (string, error) {
	name := m.cfg.Go2RTC.Binary
	if p, err := exec.LookPath(name); err == nil {
		return p, nil
	}
	if filepath.IsAbs(name) {
		if _, err := os.Stat(name); err == nil {
			return name, nil
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.downloaded != "" {
		if _, err := os.Stat(m.downloaded); err == nil {
			return m.downloaded, nil
		}
	}
	path, err := m.downloadLatest(ctx)
	if err != nil {
		return "", err
	}
	m.downloaded = path
	return path, nil
}

func (m *Manager) downloadLatest(ctx context.Context) (string, error) {
	asset := fmt.Sprintf("go2rtc_%s_%s", runtime.GOOS, normalizeArch(runtime.GOARCH))
	rel, err := m.fetchLatestRelease(ctx)
	if err != nil {
		return "", fmt.Errorf("locate go2rtc release: %w", err)
	}
	dl := fmt.Sprintf("https://github.com/AlexxIT/go2rtc/releases/download/%s/%s", rel, asset)
	slog.Info("downloading go2rtc", "release", rel, "asset", asset)
	httpResp, err := m.httpGet(ctx, dl)
	if err != nil {
		return "", err
	}
	defer httpResp.Body.Close()
	if httpResp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("go2rtc download status %d for %s", httpResp.StatusCode, dl)
	}
	if err := os.MkdirAll(m.cfg.DataDir, 0o755); err != nil {
		return "", err
	}
	binPath := filepath.Join(m.cfg.DataDir, "go2rtc")
	f, err := os.OpenFile(binPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := io.Copy(f, httpResp.Body); err != nil {
		os.Remove(binPath)
		return "", err
	}
	return binPath, nil
}

func normalizeArch(arch string) string {
	switch arch {
	case "arm":
		return "armv6"
	}
	return arch
}

type ghRelease struct {
	TagName string `json:"tag_name"`
}

func (m *Manager) fetchLatestRelease(ctx context.Context) (string, error) {
	resp, err := m.httpGet(ctx, "https://api.github.com/repos/AlexxIT/go2rtc/releases/latest")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("github api status %d", resp.StatusCode)
	}
	var rel ghRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return "", err
	}
	if rel.TagName == "" {
		return "", fmt.Errorf("no latest release found")
	}
	return rel.TagName, nil
}

func (m *Manager) httpGet(ctx context.Context, u string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "vigilant")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	// follow redirects manually so the body is drained properly
	if resp.StatusCode >= 301 && resp.StatusCode <= 308 {
		loc := resp.Header.Get("Location")
		resp.Body.Close()
		if loc != "" {
			return m.httpGet(ctx, loc)
		}
	}
	return resp, nil
}

// Start launches go2rtc and keeps it running until ctx is cancelled.
// It restarts the child with backoff on unexpected exits.
func (m *Manager) Start(ctx context.Context) error {
	m.BuildStreams()
	confPath, err := m.writeConfig()
	if err != nil {
		return err
	}
	go m.supervise(ctx, confPath)
	return m.WaitReady(ctx, 60*time.Second)
}

func (m *Manager) supervise(ctx context.Context, confPath string) {
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		bin, err := m.ResolveBinary(ctx)
		if err != nil {
			slog.Error("go2rtc binary unavailable", "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
			continue
		}
		cmd := exec.CommandContext(ctx, bin, "-c", confPath)
		w := &logWriter{prefix: "go2rtc"}
		cmd.Stdout = w
		cmd.Stderr = w
		m.mu.Lock()
		m.cmd = cmd
		m.mu.Unlock()
		start := time.Now()
		err = cmd.Run()
		if ctx.Err() != nil {
			return // shutting down
		}
		slog.Warn("go2rtc exited, restarting", "err", err, "uptime", time.Since(start).String())
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > 30*time.Second {
			backoff = 30 * time.Second
		}
	}
}

// WaitReady blocks until the go2rtc HTTP API answers.
func (m *Manager) WaitReady(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 2 * time.Second}
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		resp, err := client.Get(m.APIBase() + "/api/streams")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("go2rtc API at %s not ready", m.APIBase())
}

// Stop terminates the child process.
func (m *Manager) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cmd != nil && m.cmd.Process != nil {
		_ = m.cmd.Process.Kill()
	}
}

// Snapshot fetches a JPEG frame for a stream from the go2rtc API.
func (m *Manager) Snapshot(ctx context.Context, stream string) ([]byte, error) {
	u := fmt.Sprintf("%s/api/frame.jpeg?src=%s", m.APIBase(), url.QueryEscape(stream))
	resp, err := m.httpGet(ctx, u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("frame.jpeg status %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 16<<20))
}

// StreamStatus returns the raw go2rtc stream table (for diagnostics).
func (m *Manager) StreamStatus(ctx context.Context) (map[string]interface{}, error) {
	resp, err := m.httpGet(ctx, m.APIBase()+"/api/streams")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		Streams map[string]interface{} `json:"streams"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Streams, nil
}

type logWriter struct{ prefix string }

func (w *logWriter) Write(p []byte) (int, error) {
	line := strings.TrimSpace(string(p))
	if line != "" {
		slog.Debug(w.prefix + ": " + line)
	}
	return len(p), nil
}
