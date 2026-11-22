package motion

import (
	"sync"

	"github.com/jbutlerdev/vigilant/internal/config"
)

// Mask is a per-pixel ignore bitmap built from the normalized YAML shapes.
// Masked pixels are excluded from motion scoring entirely.
type Mask struct {
	w, h int
	skip []bool // len == w*h, true = ignore pixel
	none bool
}

// BuildMask rasterizes normalized rectangles/polygons into a pixel bitmap.
func BuildMask(w, h int, m config.Mask) *Mask {
	out := &Mask{w: w, h: h, skip: make([]bool, w*h)}
	if m.Empty() {
		out.none = true
		return out
	}
	for _, r := range m.Rectangles {
		x0 := clampi(int(r.X*float64(w)), 0, w)
		y0 := clampi(int(r.Y*float64(h)), 0, h)
		x1 := clampi(int((r.X+r.W)*float64(w)), 0, w)
		y1 := clampi(int((r.Y+r.H)*float64(h)), 0, h)
		for y := y0; y < y1; y++ {
			row := y * w
			for x := x0; x < x1; x++ {
				out.skip[row+x] = true
			}
		}
	}
	for _, p := range m.Polygons {
		if len(p.Points) < 3 {
			continue
		}
		// bounding box to limit the point-in-polygon tests
		minX, minY := 1.0, 1.0
		maxX, maxY := 0.0, 0.0
		for _, pt := range p.Points {
			if pt[0] < minX {
				minX = pt[0]
			}
			if pt[0] > maxX {
				maxX = pt[0]
			}
			if pt[1] < minY {
				minY = pt[1]
			}
			if pt[1] > maxY {
				maxY = pt[1]
			}
		}
		x0 := clampi(int(minX*float64(w)), 0, w-1)
		y0 := clampi(int(minY*float64(h)), 0, h-1)
		x1 := clampi(int(maxX*float64(w))+1, 0, w)
		y1 := clampi(int(maxY*float64(h))+1, 0, h)
		for y := y0; y < y1; y++ {
			py := (float64(y) + 0.5) / float64(h)
			row := y * w
			for x := x0; x < x1; x++ {
				px := (float64(x) + 0.5) / float64(w)
				if pointInPolygon(px, py, p.Points) {
					out.skip[row+x] = true
				}
			}
		}
	}
	return out
}

func pointInPolygon(x, y float64, pts [][2]float64) bool {
	inside := false
	n := len(pts)
	for i, j := 0, n-1; i < n; j, i = i, i+1 {
		xi, yi := pts[i][0], pts[i][1]
		xj, yj := pts[j][0], pts[j][1]
		if (yi > y) != (yj > y) {
			xint := (xj-xi)*(y-yi)/(yj-yi) + xi
			if x < xint {
				inside = !inside
			}
		}
	}
	return inside
}

func clampi(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// None reports whether the mask ignores nothing.
func (m *Mask) None() bool { return m == nil || m.none }

// Ignored reports whether a pixel is masked out.
func (m *Mask) Ignored(i int) bool {
	if m == nil || m.none {
		return false
	}
	return m.skip[i]
}

// ActiveCount returns the number of pixels that are not masked.
func (m *Mask) ActiveCount() int {
	if m == nil || m.none {
		return m.w * m.h
	}
	n := 0
	for _, s := range m.skip {
		if !s {
			n++
		}
	}
	return n
}

// MaskBox allows hot-swapping the mask from the API without stopping the
// detector pipeline.
type MaskBox struct {
	mu sync.RWMutex
	m  *Mask
}

// Set replaces the current mask.
func (b *MaskBox) Set(m *Mask) {
	b.mu.Lock()
	b.m = m
	b.mu.Unlock()
}

// Get returns the current mask.
func (b *MaskBox) Get() *Mask {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.m
}
