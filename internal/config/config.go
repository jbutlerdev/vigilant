// Package config handles loading, defaulting, and persisting Vigilant's
// YAML configuration.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration wraps time.Duration so it can be written as "5s" in YAML.
type Duration struct{ time.Duration }

// UnmarshalYAML parses a Go duration string ("5s", "1h30m", ...).
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	d.Duration = parsed
	return nil
}

// MarshalYAML renders the duration as a string.
func (d Duration) MarshalYAML() (interface{}, error) { return d.Duration.String(), nil }

// MarshalJSON renders the duration as a Go duration string ("5s").
func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.Duration.String()) }

// UnmarshalJSON accepts a duration string ("5s") or nanoseconds.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		parsed, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("invalid duration %q: %w", s, err)
		}
		d.Duration = parsed
		return nil
	}
	var n int64
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	d.Duration = time.Duration(n)
	return nil
}

// Rect is a normalized (0..1) rectangle in frame coordinates.
type Rect struct {
	X float64 `yaml:"x" json:"x"`
	Y float64 `yaml:"y" json:"y"`
	W float64 `yaml:"w" json:"w"`
	H float64 `yaml:"h" json:"h"`
}

// Polygon is a normalized closed polygon, a list of [x, y] points.
type Polygon struct {
	Points [][2]float64 `yaml:"points" json:"points"`
}

// Mask describes regions of the frame that motion detection should ignore.
type Mask struct {
	Rectangles []Rect    `yaml:"rectangles,omitempty" json:"rectangles,omitempty"`
	Polygons   []Polygon `yaml:"polygons,omitempty" json:"polygons,omitempty"`
}

// Empty reports whether the mask ignores nothing.
func (m Mask) Empty() bool { return len(m.Rectangles) == 0 && len(m.Polygons) == 0 }

// MotionConfig tunes the per-camera motion detector.
type MotionConfig struct {
	Enabled    *bool    `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	URL        string   `yaml:"url,omitempty" json:"url,omitempty"` // substream; falls back to camera URL
	Width      int      `yaml:"width,omitempty" json:"width,omitempty"`
	Height     int      `yaml:"height,omitempty" json:"height,omitempty"`
	FPS        float64  `yaml:"fps,omitempty" json:"fps,omitempty"`
	Threshold  int      `yaml:"threshold,omitempty" json:"threshold,omitempty"`     // per-pixel luma delta considered changed
	MinPercent float64  `yaml:"min_percent,omitempty" json:"min_percent,omitempty"` // % of unmasked frame that must change (primary knob)
	MinPixels  int      `yaml:"min_pixels,omitempty" json:"min_pixels,omitempty"`   // legacy: absolute pixel count
	MinRatio   float64  `yaml:"min_ratio,omitempty" json:"min_ratio,omitempty"`     // legacy: fraction of unmasked area
	Window     Duration `yaml:"window,omitempty" json:"window,omitempty"`           // sliding window duration
	MinFrames  int      `yaml:"min_frames,omitempty" json:"min_frames,omitempty"`   // motion frames inside window to trigger
	Hold       Duration `yaml:"hold,omitempty" json:"hold,omitempty"`               // keep motion state this long after last motion frame
	Buffer     Duration `yaml:"buffer,omitempty" json:"buffer,omitempty"`           // clip padding around events
	Mask       Mask     `yaml:"mask,omitempty" json:"mask,omitempty"`
}

// Camera is one monitored camera.
type Camera struct {
	Name   string        `yaml:"name" json:"name"`
	URL    string        `yaml:"url" json:"url"`
	Record *bool         `yaml:"record,omitempty" json:"record,omitempty"`
	Motion *MotionConfig `yaml:"motion,omitempty" json:"motion,omitempty"`

	// resolved at load time
	record bool
	motion MotionConfig
}

// RecordEnabled reports whether continuous recording is on for this camera.
func (c *Camera) RecordEnabled() bool { return c.record }

// MotionEnabled reports whether motion detection is on for this camera.
func (c *Camera) MotionEnabled() bool { return c.motion.Enabled != nil && *c.motion.Enabled }

// MotionSourceURL is the stream the motion detector should pull.
func (c *Camera) MotionSourceURL() string {
	if c.motion.URL != "" {
		return c.motion.URL
	}
	return c.URL
}

// Go2RTC configures the bundled go2rtc restreamer child process.
type Go2RTC struct {
	Binary     string `yaml:"binary,omitempty" json:"binary,omitempty"`           // path or name on PATH; auto-downloaded when empty/missing
	RTSPListen string `yaml:"rtsp_listen,omitempty" json:"rtsp_listen,omitempty"` // internal restream listen address
	APIListen  string `yaml:"api_listen,omitempty" json:"api_listen,omitempty"`   // go2rtc web API listen address
}

// Retention configures how long media is kept.
type Retention struct {
	ContinuousDays int `yaml:"continuous_days,omitempty" json:"continuous_days,omitempty"` // full-time recordings
	MotionDays     int `yaml:"motion_days,omitempty" json:"motion_days,omitempty"`         // motion event clips
}

// Config is the whole application configuration.
type Config struct {
	Server struct {
		Listen string `yaml:"listen,omitempty" json:"listen,omitempty"`
	} `yaml:"server,omitempty" json:"server,omitempty"`
	DataDir   string    `yaml:"data_dir,omitempty" json:"data_dir,omitempty"`
	Go2RTC    Go2RTC    `yaml:"go2rtc,omitempty" json:"go2rtc,omitempty"`
	Retention Retention `yaml:"retention,omitempty" json:"retention,omitempty"`
	Cameras   []*Camera `yaml:"cameras" json:"cameras"`

	path string // file it was loaded from, used by Save
}

// Path returns the file this config was loaded from.
func (c *Config) Path() string { return c.path }

// Camera returns a camera by name.
func (c *Config) Camera(name string) *Camera {
	for _, cam := range c.Cameras {
		if cam.Name == name {
			return cam
		}
	}
	return nil
}

// MotionDefaults exposes resolved motion defaults (after applyDefaults).
func (c *Camera) MotionSettings() MotionConfig { return c.motion }

func boolPtr(b bool) *bool { return &b }

// fillMotionDefaults applies defaults to any unset field.
func fillMotionDefaults(m *MotionConfig) {
	if m.Enabled == nil {
		m.Enabled = boolPtr(true)
	}
	if m.Width <= 0 {
		m.Width = 320
	}
	if m.Height <= 0 {
		m.Height = 180
	}
	if m.FPS <= 0 {
		m.FPS = 5
	}
	if m.Threshold <= 0 {
		m.Threshold = 25
	}
	if m.MinPercent <= 0 && m.MinPixels <= 0 && m.MinRatio <= 0 {
		m.MinPercent = 0.3 // ~0.3% of the unmasked frame
	}
	if m.Window.Duration <= 0 {
		m.Window = Duration{5 * time.Second}
	}
	if m.MinFrames <= 0 {
		m.MinFrames = 3
	}
	if m.Hold.Duration <= 0 {
		m.Hold = Duration{10 * time.Second}
	}
	if m.Buffer.Duration <= 0 {
		m.Buffer = Duration{10 * time.Second}
	}
}

func (c *Config) applyDefaults() error {
	if c.Server.Listen == "" {
		c.Server.Listen = ":8080"
	}
	if c.DataDir == "" {
		c.DataDir = "data"
	}
	if abs, err := filepath.Abs(c.DataDir); err == nil {
		c.DataDir = abs
	}
	if c.Go2RTC.Binary == "" {
		c.Go2RTC.Binary = "go2rtc"
	}
	if c.Go2RTC.RTSPListen == "" {
		c.Go2RTC.RTSPListen = "127.0.0.1:8554"
	}
	if c.Go2RTC.APIListen == "" {
		c.Go2RTC.APIListen = "127.0.0.1:1984"
	}
	if c.Retention.ContinuousDays <= 0 {
		c.Retention.ContinuousDays = 7
	}
	if c.Retention.MotionDays <= 0 {
		c.Retention.MotionDays = 30
	}
	seen := map[string]bool{}
	for _, cam := range c.Cameras {
		if cam.Name == "" || seen[cam.Name] {
			return fmt.Errorf("camera with empty or duplicate name")
		}
		seen[cam.Name] = true
		if cam.URL == "" {
			return fmt.Errorf("camera %q: url is required", cam.Name)
		}
		if cam.Record == nil {
			cam.Record = boolPtr(true)
		}
		cam.record = *cam.Record
		m := &MotionConfig{}
		if cam.Motion != nil {
			*m = *cam.Motion
		}
		fillMotionDefaults(m)
		cam.motion = *m
		cam.Motion = &cam.motion
	}
	return nil
}

// SetCameraSettings merges edited fields into a camera's configuration,
// re-applies defaults, and persists the YAML. Only non-zero fields in the
// patch are applied; mask is managed separately via SetMask. The caller is
// responsible for restarting the camera's pipelines (App.RestartCamera).
func (c *Config) SetCameraSettings(name string, record *bool, url string, patch *MotionConfig) error {
	cam := c.Camera(name)
	if cam == nil {
		return fmt.Errorf("camera %q not found", name)
	}
	if record != nil {
		cam.Record = record
		cam.record = *record
	}
	if url != "" {
		cam.URL = url
	}
	if patch != nil {
		m := cam.motion // start from the resolved settings
		if patch.Enabled != nil {
			m.Enabled = patch.Enabled
		}
		if patch.URL != "" {
			m.URL = patch.URL
		}
		if patch.Width > 0 {
			m.Width = patch.Width
		}
		if patch.Height > 0 {
			m.Height = patch.Height
		}
		if patch.FPS > 0 {
			m.FPS = patch.FPS
		}
		if patch.Threshold > 0 {
			m.Threshold = patch.Threshold
		}
		if patch.MinPercent > 0 {
			m.MinPercent = patch.MinPercent
			m.MinPixels = 0
			m.MinRatio = 0
		}
		if patch.MinPixels > 0 {
			m.MinPixels = patch.MinPixels
			m.MinPercent = 0
			m.MinRatio = 0
		}
		if patch.MinRatio > 0 {
			m.MinRatio = patch.MinRatio
			m.MinPercent = 0
			m.MinPixels = 0
		}
		if patch.MinFrames > 0 {
			m.MinFrames = patch.MinFrames
		}
		if patch.Window.Duration > 0 {
			m.Window = patch.Window
		}
		if patch.Hold.Duration > 0 {
			m.Hold = patch.Hold
		}
		if patch.Buffer.Duration > 0 {
			m.Buffer = patch.Buffer
		}
		fillMotionDefaults(&m)
		cam.motion = m
		cam.Motion = &cam.motion
	}
	return c.Save()
}

// SetRetention updates the retention policy and persists the YAML.
func (c *Config) SetRetention(continuousDays, motionDays int) error {
	if continuousDays < 1 || continuousDays > 3650 || motionDays < 1 || motionDays > 3650 {
		return fmt.Errorf("retention days must be between 1 and 3650")
	}
	c.Retention.ContinuousDays = continuousDays
	c.Retention.MotionDays = motionDays
	return c.Save()
}

// Load reads, defaults, and validates the YAML config at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg := &Config{}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := cfg.applyDefaults(); err != nil {
		return nil, err
	}
	cfg.path = path
	return cfg, nil
}

// Save writes the config back to its original path atomically.
func (c *Config) Save() error {
	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, c.path)
}

// SetMask updates a camera's motion mask in memory and persists the config.
func (c *Config) SetMask(camName string, mask Mask) error {
	cam := c.Camera(camName)
	if cam == nil {
		return fmt.Errorf("camera %q not found", camName)
	}
	if cam.Motion == nil {
		cam.Motion = &cam.motion
	}
	cam.Motion.Mask = mask
	cam.motion.Mask = mask
	return c.Save()
}
