package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"math"
	"unicode"
)

// Geometry in points. The PNG is drawn at 2 px/pt and tagged 144 DPI so
// macOS shows it at point size on Retina screens.
const (
	pxPerPt = 2
	ss      = 4 // supersampling factor per output pixel
	hiRes   = pxPerPt * ss

	iconH      = 16.0
	battY      = 2.0
	battW      = 24.0
	battH      = 12.0
	battR      = 3.0
	stroke     = 2.0
	cellInset  = 3.0 // stroke + 1pt gap
	nubW       = 1.5
	nubH       = 5.0
	glyphCell  = 1.0 // pt per font pixel
	letterGap  = 3.0
	accountGap = 7.0

	dimAlpha   = 0.3
	staleAlpha = 0.45
)

var (
	fgDark  = rgba{0xF2, 0xF2, 0xF2, 1}
	fgLight = rgba{0x1C, 0x1C, 0x1C, 1}
	amber   = rgba{0xE0, 0xA0, 0x30, 1}
	red     = rgba{0xE5, 0x53, 0x4B, 1}
)

// gauge is one account's battery. It drains like a real one: the frame
// shows weekly remaining, the cell session remaining. Colours follow usage.
type gauge struct {
	Letter        rune
	Week, Session float64 // percent used, 0-100
	Stale, NoData bool
	NoSession     bool // no 5h limit: frame and cell both show the week
}

// rgba is a straight (non-premultiplied) colour, channels 0-255, alpha 0-1.
type rgba struct{ r, g, b, a float64 }

func (c rgba) fade(k float64) rgba { c.a *= k; return c }

func levelColor(p float64, fg rgba) rgba {
	switch {
	case p >= 90:
		return red
	case p >= 75:
		return amber
	}
	return fg
}

// canvas is a supersampled premultiplied RGBA buffer addressed in points.
type canvas struct {
	w, h int
	pix  []rgba // premultiplied
}

func newCanvas(wPt, hPt float64) *canvas {
	w := int(math.Ceil(wPt*pxPerPt)) * ss
	h := int(math.Ceil(hPt*pxPerPt)) * ss
	return &canvas{w: w, h: h, pix: make([]rgba, w*h)}
}

// paint composites colorAt over every sample inside the box [x0,x1)×[y0,y1)
// (points) for which colorAt reports true.
func (c *canvas) paint(x0, y0, x1, y1 float64, colorAt func(x, y float64) (rgba, bool)) {
	i0, i1 := max(int(x0*hiRes), 0), min(int(math.Ceil(x1*hiRes)), c.w)
	j0, j1 := max(int(y0*hiRes), 0), min(int(math.Ceil(y1*hiRes)), c.h)
	for j := j0; j < j1; j++ {
		y := (float64(j) + 0.5) / hiRes
		for i := i0; i < i1; i++ {
			x := (float64(i) + 0.5) / hiRes
			col, ok := colorAt(x, y)
			if !ok || col.a <= 0 {
				continue
			}
			d := &c.pix[j*c.w+i]
			k := 1 - col.a
			d.r = col.r*col.a + d.r*k
			d.g = col.g*col.a + d.g*k
			d.b = col.b*col.a + d.b*k
			d.a = col.a + d.a*k
		}
	}
}

// image box-filters the samples down to output pixels.
func (c *canvas) image() *image.NRGBA {
	w, h := c.w/ss, c.h/ss
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	n := float64(ss * ss)
	for y := range h {
		for x := range w {
			var s rgba
			for j := range ss {
				for i := range ss {
					p := c.pix[(y*ss+j)*c.w+x*ss+i]
					s.r, s.g, s.b, s.a = s.r+p.r, s.g+p.g, s.b+p.b, s.a+p.a
				}
			}
			if s.a == 0 {
				continue
			}
			img.SetNRGBA(x, y, color.NRGBA{
				R: uint8(math.Round(s.r / s.a)),
				G: uint8(math.Round(s.g / s.a)),
				B: uint8(math.Round(s.b / s.a)),
				A: uint8(math.Round(s.a / n * 255)),
			})
		}
	}
	return img
}

// inRoundRect reports whether (x,y) lies in the rect with corner radius r.
func inRoundRect(x, y, l, t, rr, b, r float64) bool {
	if x < l || x >= rr || y < t || y >= b {
		return false
	}
	dx := max(l+r-x, 0, x-(rr-r))
	dy := max(t+r-y, 0, y-(b-r))
	return dx*dx+dy*dy <= r*r
}

// batteryWidth includes the nub.
const batteryWidth = battW + nubW

func gaugeWidth(letters bool) float64 {
	if letters {
		return glyphW*glyphCell + letterGap + batteryWidth
	}
	return batteryWidth
}

// renderIcon draws the gauges side by side, each optionally led by its letter.
func renderIcon(gs []gauge, dark, letters bool) *image.NRGBA {
	fg := fgLight
	if dark {
		fg = fgDark
	}
	w := 0.0
	if len(gs) > 0 {
		w = float64(len(gs))*gaugeWidth(letters) + float64(len(gs)-1)*accountGap
	}
	c := newCanvas(max(w, 1), iconH)
	x := 0.0
	for _, g := range gs {
		k, lk := 1.0, 1.0 // battery and letter alpha
		if g.Stale {
			k, lk = staleAlpha, staleAlpha
		}
		if g.NoData {
			lk = staleAlpha
		}
		if letters {
			drawGlyph(c, g.Letter, x, battY+(battH-glyphH*glyphCell)/2, glyphCell, fg.fade(lk))
			x += glyphW*glyphCell + letterGap
		}
		drawBattery(c, x, battY, g, fg, k)
		x += batteryWidth + accountGap
	}
	return c.image()
}

func drawBattery(c *canvas, x0, y0 float64, g gauge, fg rgba, k float64) {
	dim := fg.fade(dimAlpha * k)
	week, sess := clampPct(g.Week), clampPct(g.Session)
	weekLeft, sessLeft := 100-week, 100-sess
	if g.NoData {
		weekLeft, sessLeft = 0, 0
	}
	lit := levelColor(week, fg).fade(k)
	litTo := x0 + battW*weekLeft/100
	cell := levelColor(sess, fg).fade(k)
	if g.NoSession && !g.NoData {
		cell, sessLeft = lit, weekLeft
	}

	// Frame: lit up to the weekly remaining mark, dim after it.
	c.paint(x0, y0, x0+battW, y0+battH, func(x, y float64) (rgba, bool) {
		if !inRoundRect(x, y, x0, y0, x0+battW, y0+battH, battR) ||
			inRoundRect(x, y, x0+stroke, y0+stroke, x0+battW-stroke, y0+battH-stroke, battR-stroke) {
			return rgba{}, false
		}
		if x < litTo {
			return lit, true
		}
		return dim, true
	})

	// Nub, lit only while the week is (nearly) untouched.
	nub := dim
	if weekLeft >= 99 {
		nub = lit
	}
	ny := y0 + (battH-nubH)/2
	c.paint(x0+battW, ny, x0+battW+nubW, ny+nubH, func(x, y float64) (rgba, bool) {
		return nub, inRoundRect(x, y, x0+battW-2, ny, x0+battW+nubW, ny+nubH, 0.75)
	})

	// Cell: 5h session remaining.
	cl, ct := x0+cellInset, y0+cellInset
	cr, cb := x0+battW-cellInset, y0+battH-cellInset
	fillTo := cl + (cr-cl)*sessLeft/100
	c.paint(cl, ct, cr, cb, func(x, y float64) (rgba, bool) {
		return cell, x < fillTo && inRoundRect(x, y, cl, ct, cr, cb, 1)
	})

	if g.NoData {
		gy := y0 + (battH-glyphH*glyphCell)/2
		drawGlyph(c, '?', x0+(battW-glyphW*glyphCell)/2, gy, glyphCell, fg.fade(0.6*k))
	}
}

func clampPct(p float64) float64 { return min(max(p, 0), 100) }

func drawGlyph(c *canvas, r rune, x0, y0, cell float64, col rgba) {
	rows, ok := font[unicode.ToUpper(r)]
	if !ok {
		rows = font['?']
	}
	c.paint(x0, y0, x0+glyphW*cell, y0+glyphH*cell, func(x, y float64) (rgba, bool) {
		i, j := int((x-x0)/cell), int((y-y0)/cell)
		if i < 0 || j < 0 || i >= glyphW || j >= glyphH {
			return rgba{}, false
		}
		return col, rows[j][i] == '#'
	})
}

// encodePNG writes img as PNG with a pHYs chunk declaring 144 DPI.
func encodePNG(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return addPHYs(buf.Bytes(), 5669) // 144 DPI in px per metre
}

// addPHYs inserts a pHYs chunk right after IHDR.
func addPHYs(p []byte, ppm uint32) ([]byte, error) {
	const ihdrEnd = 8 + 4 + 4 + 13 + 4 // signature + IHDR chunk
	if len(p) < ihdrEnd || string(p[12:16]) != "IHDR" {
		return nil, errors.New("not a PNG")
	}
	chunk := make([]byte, 0, 21)
	chunk = binary.BigEndian.AppendUint32(chunk, 9)
	chunk = append(chunk, "pHYs"...)
	chunk = binary.BigEndian.AppendUint32(chunk, ppm)
	chunk = binary.BigEndian.AppendUint32(chunk, ppm)
	chunk = append(chunk, 1) // unit: metre
	chunk = binary.BigEndian.AppendUint32(chunk, crc32.ChecksumIEEE(chunk[4:]))
	out := make([]byte, 0, len(p)+len(chunk))
	out = append(out, p[:ihdrEnd]...)
	out = append(out, chunk...)
	return append(out, p[ihdrEnd:]...), nil
}
