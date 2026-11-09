package main

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

// Pixel rows (2x) of the battery's top frame stroke and of the cell.
var (
	frameTopRows = [2]int{battY * pxPerPt, (battY + stroke) * pxPerPt}
	cellRows     = [2]int{(battY + cellInset) * pxPerPt, (battY + battH - cellInset) * pxPerPt}
)

func countOpaque(img *image.NRGBA, rows [2]int, x0, x1 int) int {
	n := 0
	for y := rows[0]; y < rows[1]; y++ {
		for x := x0; x < x1; x++ {
			if img.NRGBAAt(x, y).A > 200 {
				n++
			}
		}
	}
	return n
}

func single(week, sess float64) *image.NRGBA {
	return renderIcon([]gauge{{Letter: 'M', Week: week, Session: sess}}, false, false)
}

func TestIconSize(t *testing.T) {
	img := renderIcon([]gauge{{Letter: 'T'}, {Letter: 'M'}}, true, true)
	wantW := int((2*gaugeWidth(true) + accountGap) * pxPerPt)
	if b := img.Bounds(); b.Dx() != wantW || b.Dy() != iconH*pxPerPt {
		t.Errorf("size = %v, want %dx%d", b, wantW, int(iconH*pxPerPt))
	}
	if b := single(0, 0).Bounds(); b.Dx() != int(batteryWidth*pxPerPt) {
		t.Errorf("single size = %v", b)
	}
}

func TestPHYs(t *testing.T) {
	p, err := encodePNG(single(50, 50))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := png.Decode(bytes.NewReader(p)); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// pHYs follows IHDR: len(4) type(4) data(9) crc(4).
	c := p[33:]
	if string(c[4:8]) != "pHYs" || binary.BigEndian.Uint32(c[:4]) != 9 {
		t.Fatalf("no pHYs after IHDR: %q", c[4:8])
	}
	if x, y := binary.BigEndian.Uint32(c[8:12]), binary.BigEndian.Uint32(c[12:16]); x != 5669 || y != 5669 || c[16] != 1 {
		t.Errorf("pHYs = %d %d unit %d", x, y, c[16])
	}
	if crc := binary.BigEndian.Uint32(c[17:21]); crc != crc32.ChecksumIEEE(c[4:17]) {
		t.Error("bad pHYs CRC")
	}
}

func TestFrameDrainsWithWeek(t *testing.T) {
	w := int(battW * pxPerPt)
	prev := 1 << 30
	for _, p := range []float64{0, 10, 35, 60, 93, 100} {
		n := countOpaque(single(p, 0), frameTopRows, 0, w)
		if n >= prev {
			t.Errorf("week %v%% used: lit frame px %d not < %d", p, n, prev)
		}
		prev = n
	}
	if n := countOpaque(single(100, 0), frameTopRows, 0, w); n != 0 {
		t.Errorf("week 100%% used: %d lit px", n)
	}
}

func TestCellDrainsWithSession(t *testing.T) {
	x0, x1 := int(cellInset*pxPerPt), int((battW-cellInset)*pxPerPt)
	prev := 1 << 30
	for _, p := range []float64{0, 10, 35, 60, 93, 100} {
		n := countOpaque(single(0, p), cellRows, x0, x1)
		if n >= prev {
			t.Errorf("session %v%% used: cell px %d not < %d", p, n, prev)
		}
		prev = n
	}
	if n := countOpaque(single(0, 0), cellRows, x0, x1); n < (x1-x0)*(cellRows[1]-cellRows[0])*9/10 {
		t.Errorf("unused cell only %d px", n)
	}
	if n := countOpaque(single(0, 100), cellRows, x0, x1); n != 0 {
		t.Errorf("used-up cell has %d px", n)
	}
}

func TestNub(t *testing.T) {
	x, y := int(battW*pxPerPt)+1, int((battY+battH/2)*pxPerPt)
	if a := single(0, 0).NRGBAAt(x, y).A; a != 255 {
		t.Errorf("unused week: nub alpha %d", a)
	}
	if a := single(5, 0).NRGBAAt(x, y).A; a > 100 {
		t.Errorf("used week: nub alpha %d", a)
	}
}

func TestThresholdColors(t *testing.T) {
	near := func(c [3]uint8, want rgba) bool {
		d := func(a uint8, b float64) bool { return float64(a) > b-3 && float64(a) < b+3 }
		return d(c[0], want.r) && d(c[1], want.g) && d(c[2], want.b)
	}
	at := func(img *image.NRGBA, x, y int) [3]uint8 {
		c := img.NRGBAAt(x, y)
		return [3]uint8{c.R, c.G, c.B}
	}
	// Left frame stroke and left end of the cell: lit while anything is left.
	frameX, frameY := 2, int((battY+battH/2)*pxPerPt)
	cellX, cellY := int(cellInset*pxPerPt)+1, cellRows[0]+4
	cases := []struct {
		week, sess       float64
		frame, cellColor rgba
	}{
		{50, 50, fgLight, fgLight},
		{80, 20, amber, fgLight},
		{90, 80, red, amber},
		{10, 92, fgLight, red},
	}
	for _, c := range cases {
		img := single(c.week, c.sess)
		if got := at(img, frameX, frameY); !near(got, c.frame) {
			t.Errorf("week %v: frame colour %v", c.week, got)
		}
		if got := at(img, cellX, cellY); !near(got, c.cellColor) {
			t.Errorf("session %v: cell colour %v", c.sess, got)
		}
	}
}

func TestStaleIsFaded(t *testing.T) {
	g := gauge{Week: 0, Session: 0}
	fresh := renderIcon([]gauge{g}, true, false)
	g.Stale = true
	stale := renderIcon([]gauge{g}, true, false)
	x, y := 8*pxPerPt, frameTopRows[0]+1
	if a, b := fresh.NRGBAAt(x, y).A, stale.NRGBAAt(x, y).A; a != 255 || b > 130 {
		t.Errorf("alpha fresh %d stale %d", a, b)
	}
}

// TestWriteSamples writes sample icons for eyeballing when ICON_SAMPLES is set.
func TestWriteSamples(t *testing.T) {
	dir := os.Getenv("ICON_SAMPLES")
	if dir == "" {
		t.Skip("set ICON_SAMPLES=dir to write sample icons")
	}
	sets := map[string][]gauge{
		"mix":   {{Letter: 'T', Week: 35, Session: 93}, {Letter: 'M', Week: 0, Session: 0}},
		"full":  {{Letter: 'T', Week: 100, Session: 100}, {Letter: 'M', Week: 80, Session: 50}},
		"low":   {{Letter: 'A', Week: 10, Session: 35}, {Letter: 'Z', Week: 60, Session: 76}},
		"stale": {{Letter: 'T', Week: 35, Session: 93, Stale: true}, {Letter: 'M', NoData: true}},
		"no5h":  {{Letter: 'A', Week: 2, NoSession: true}, {Letter: 'B', Week: 60, NoSession: true}, {Letter: 'C', Week: 95, NoSession: true}},
	}
	for name, gs := range sets {
		for _, dark := range []bool{true, false} {
			mode := "light"
			if dark {
				mode = "dark"
			}
			p, err := encodePNG(renderIcon(gs, dark, true))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, name+"-"+mode+".png"), p, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestNoSessionStyle(t *testing.T) {
	w := int(battW * pxPerPt)
	x0, x1 := int(cellInset*pxPerPt), int((battW-cellInset)*pxPerPt)
	// Frame and cell both drain with the week.
	prevF, prevC := 1<<30, 1<<30
	for _, p := range []float64{0, 10, 60, 95, 100} {
		img := renderIcon([]gauge{{Week: p, NoSession: true}}, false, false)
		f, c := countOpaque(img, frameTopRows, 0, w), countOpaque(img, cellRows, x0, x1)
		if f != countOpaque(single(p, 0), frameTopRows, 0, w) {
			t.Errorf("week %v: frame px %d differs from normal battery", p, f)
		}
		if c != countOpaque(single(0, p), cellRows, x0, x1) {
			t.Errorf("week %v: cell px %d, want week left", p, c)
		}
		if f >= prevF || c >= prevC {
			t.Errorf("week %v: frame %d / cell %d not shrinking", p, f, c)
		}
		prevF, prevC = f, c
	}
	x, y := int(battW*pxPerPt)+1, int((battY+battH/2)*pxPerPt)
	if a := renderIcon([]gauge{{Week: 0, NoSession: true}}, false, false).NRGBAAt(x, y).A; a != 255 {
		t.Errorf("unused week: nub alpha %d", a)
	}
	if a := renderIcon([]gauge{{Week: 2, NoSession: true}}, false, false).NRGBAAt(x, y).A; a > 100 {
		t.Errorf("used week: nub alpha %d", a)
	}
	// Frame and cell colours follow week usage.
	for _, tc := range []struct {
		week float64
		want rgba
	}{{60, fgLight}, {80, amber}, {92, red}} {
		img := renderIcon([]gauge{{Week: tc.week, Session: 95, NoSession: true}}, false, false)
		for _, pt := range [][2]int{{x0 + 1, cellRows[0] + 4}, {2, int((battY + battH/2) * pxPerPt)}} {
			c := img.NRGBAAt(pt[0], pt[1])
			if c.A < 200 || int(c.R) != int(tc.want.r) || int(c.G) != int(tc.want.g) {
				t.Errorf("week %v at %v: colour %v", tc.week, pt, c)
			}
		}
	}
}
