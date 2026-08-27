// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"fmt"
	"os"
	"strings"
)

// Terminal control sequences.
const (
	csi = "\x1b["

	sgrReset     = csi + "0m"
	sgrBold      = csi + "1m"
	sgrDim       = csi + "2m"
	sgrItalic    = csi + "3m"
	sgrUnderline = csi + "4m"
	sgrReverse   = csi + "7m"

	altScreenOn  = csi + "?1049h"
	altScreenOff = csi + "?1049l"
	cursorHide   = csi + "?25l"
	cursorShow   = csi + "?25h"
	cursorHome   = csi + "H"
	clearToEOL   = csi + "K"
	clearBelow   = csi + "J"
	clearAll     = csi + "2J"
)

// rgb is a 24-bit color. Terminals that only claim 256 colors still almost
// always understand truecolor SGR; the ones that don't degrade to something
// readable rather than garbage, so we don't bother downsampling.
type rgb struct{ r, g, b uint8 }

func (c rgb) fg() string { return fmt.Sprintf("%s38;2;%d;%d;%dm", csi, c.r, c.g, c.b) }
func (c rgb) bg() string { return fmt.Sprintf("%s48;2;%d;%d;%dm", csi, c.r, c.g, c.b) }

// paint wraps s in c's foreground color.
func (c rgb) paint(s string) string { return c.fg() + s + sgrReset }

// paintf is paint with formatting.
func (c rgb) paintf(format string, a ...any) string {
	return c.fg() + fmt.Sprintf(format, a...) + sgrReset
}

// A deliberately loud synthwave palette. Routing tables are boring; this is
// the one chance we get to make them not boring.
var (
	colMagenta = rgb{0xff, 0x2e, 0x97}
	colPink    = rgb{0xff, 0x71, 0xce}
	colCyan    = rgb{0x05, 0xd9, 0xe8}
	colBlue    = rgb{0x53, 0x9b, 0xf5}
	colPurple  = rgb{0xb9, 0x6b, 0xff}
	colGreen   = rgb{0x3d, 0xf5, 0x8b}
	colYellow  = rgb{0xff, 0xe5, 0x5c}
	colOrange  = rgb{0xff, 0x9f, 0x1c}
	colRed     = rgb{0xff, 0x49, 0x4f}
	colWhite   = rgb{0xf2, 0xf2, 0xf7}
	colGray    = rgb{0x8a, 0x8a, 0x9e}
	colDark    = rgb{0x4a, 0x4a, 0x5e}

	// Backgrounds for header/footer chrome.
	bgHeader = rgb{0x24, 0x11, 0x3d}
	bgFooter = rgb{0x1a, 0x1a, 0x2e}
)

// gradient returns the color at position t (0..1) along a magenta->cyan ramp,
// used for the banner and progress bars.
func gradient(t float64) rgb {
	if t < 0 {
		t = 0
	}
	if t > 1 {
		t = 1
	}
	stops := []rgb{colMagenta, colPurple, colBlue, colCyan}
	x := t * float64(len(stops)-1)
	i := int(x)
	if i >= len(stops)-1 {
		return stops[len(stops)-1]
	}
	f := x - float64(i)
	lerp := func(a, b uint8) uint8 { return uint8(float64(a) + (float64(b)-float64(a))*f) }
	return rgb{
		lerp(stops[i].r, stops[i+1].r),
		lerp(stops[i].g, stops[i+1].g),
		lerp(stops[i].b, stops[i+1].b),
	}
}

// ansiState tracks position within an ANSI escape sequence while scanning a
// string. Escapes must be skipped without being counted or split, or padding
// and truncation silently corrupt every colored line.
type ansiState int

const (
	ansiNormal ansiState = iota
	ansiEsc              // just saw ESC
	ansiCSI              // inside ESC [ ... final
)

// step advances the scanner by one rune and reports whether that rune is
// actually painted on screen.
func (s *ansiState) step(r rune) bool {
	switch *s {
	case ansiNormal:
		if r == 0x1b {
			*s = ansiEsc
			return false
		}
		return true
	case ansiEsc:
		// ESC [ starts a CSI sequence; anything else is a two-byte escape.
		if r == '[' {
			*s = ansiCSI
		} else {
			*s = ansiNormal
		}
		return false
	default: // ansiCSI: parameter and intermediate bytes, then a final in @..~
		if r >= '@' && r <= '~' {
			*s = ansiNormal
		}
		return false
	}
}

// dispWidth reports the printed column width of s, ignoring ANSI escape
// sequences and counting wide runes (emoji, CJK) as two columns.
func dispWidth(s string) int {
	w := 0
	var st ansiState
	for _, r := range s {
		if st.step(r) {
			w += runeWidth(r)
		}
	}
	return w
}

// wideSymbols are the few code points in the "miscellaneous symbols" block
// that terminals render with emoji presentation, and therefore two columns
// wide. The rest of that block — ✔ ✖ ✚ ⚠ and friends — is text presentation
// and one column, so the block as a whole must not be treated as wide.
var wideSymbols = map[rune]bool{
	0x26a1: true, // ⚡
	0x2705: true, // ✅
	0x274c: true, // ❌
	0x2b50: true, // ⭐
}

func runeWidth(r rune) int {
	switch {
	case r < 0x1100:
		return 1
	case r >= 0x2600 && r <= 0x27bf:
		if wideSymbols[r] {
			return 2
		}
		return 1
	case r >= 0x1f300 && r <= 0x1faff, // emoji & pictographs
		r >= 0x1100 && r <= 0x115f, // hangul jamo
		r >= 0x2e80 && r <= 0xa4cf, // CJK
		r >= 0xac00 && r <= 0xd7a3, // hangul syllables
		r >= 0xf900 && r <= 0xfaff,
		r >= 0xfe30 && r <= 0xfe6f,
		r >= 0xff00 && r <= 0xff60,
		r >= 0xffe0 && r <= 0xffe6:
		return 2
	}
	return 1
}

// padRight pads s with spaces to w display columns.
func padRight(s string, w int) string {
	d := dispWidth(s)
	if d >= w {
		return s
	}
	return s + strings.Repeat(" ", w-d)
}

// padLeft right-aligns s within w display columns.
func padLeft(s string, w int) string {
	d := dispWidth(s)
	if d >= w {
		return s
	}
	return strings.Repeat(" ", w-d) + s
}

// cell renders s in exactly w display columns for use as a table column:
// long values are truncated rather than pushing the next column sideways, and
// at least one space is always left before whatever follows.
func cell(s string, w int) string {
	if w <= 0 {
		return ""
	}
	return padRight(truncate(s, w-1), w)
}

// truncate shortens s to at most w display columns, appending an ellipsis when
// it has to cut. ANSI escapes are preserved and never counted or split.
func truncate(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if dispWidth(s) <= w {
		return s
	}
	var b strings.Builder
	used := 0
	var st ansiState
	for _, r := range s {
		if !st.step(r) {
			b.WriteRune(r)
			continue
		}
		rw := runeWidth(r)
		if used+rw > w-1 {
			break
		}
		b.WriteRune(r)
		used += rw
	}
	b.WriteString(sgrReset)
	b.WriteString("…")
	return b.String()
}

// bar renders a horizontal meter of the given width, filled to frac (0..1),
// colored along the gradient so a full bar sweeps magenta to cyan.
func bar(frac float64, width int) string {
	if width <= 0 {
		return ""
	}
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	// Eighth-block partial fill gives a smooth edge at low widths.
	const blocks = " ▏▎▍▌▋▊▉█"
	exact := frac * float64(width)
	full := int(exact)
	rem := exact - float64(full)

	var b strings.Builder
	runes := []rune(blocks)
	for i := 0; i < full && i < width; i++ {
		b.WriteString(gradient(float64(i) / float64(width)).fg())
		b.WriteRune('█')
	}
	if full < width {
		idx := int(rem * 8)
		if idx > 0 {
			b.WriteString(gradient(float64(full) / float64(width)).fg())
			b.WriteRune(runes[idx])
			full++
		}
	}
	if full < width {
		b.WriteString(colDark.fg())
		b.WriteString(strings.Repeat("░", width-full))
	}
	b.WriteString(sgrReset)
	return b.String()
}

// sparkline renders values as a compact unicode chart, scaled to the largest
// value present. Negative entries render as gaps (used for missing samples).
func sparkline(vals []float64, c rgb) string {
	if len(vals) == 0 {
		return ""
	}
	runes := []rune("▁▂▃▄▅▆▇█")
	max := 0.0
	for _, v := range vals {
		if v > max {
			max = v
		}
	}
	var b strings.Builder
	b.WriteString(c.fg())
	for _, v := range vals {
		if v < 0 {
			b.WriteString(colDark.fg() + "·" + c.fg())
			continue
		}
		i := 0
		if max > 0 {
			i = int(v / max * float64(len(runes)-1))
		}
		b.WriteRune(runes[i])
	}
	b.WriteString(sgrReset)
	return b.String()
}

// spinnerFrames drive the "working" indicator; index by tick.
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

func spinner(tick int) string {
	return spinnerFrames[tick%len(spinnerFrames)]
}

// supportsColor reports whether we should emit escape sequences at all. Used
// by the non-TUI subcommands, which are frequently piped into other tools.
func supportsColor() bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	t := os.Getenv("TERM")
	return t != "" && t != "dumb"
}

// stripANSI removes escape sequences, for writing plain output to files.
func stripANSI(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	var st ansiState
	for _, r := range s {
		if st.step(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}
