// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"sync"

	"golang.org/x/term"
)

// KeyType distinguishes printable input from the escape sequences terminals
// use for navigation keys.
type KeyType int

const (
	KeyRune KeyType = iota
	KeyUp
	KeyDown
	KeyLeft
	KeyRight
	KeyEnter
	KeyEsc
	KeyTab
	KeyBackspace
	KeyPgUp
	KeyPgDn
	KeyHome
	KeyEnd
	KeyCtrlC
	KeyUnknown
)

// Key is one decoded keypress.
type Key struct {
	Type KeyType
	Rune rune
}

// IsRune reports whether k is the printable rune r.
func (k Key) IsRune(r rune) bool { return k.Type == KeyRune && k.Rune == r }

// Screen owns the terminal: raw mode, the alternate screen buffer, and the
// input decoding goroutine.
type Screen struct {
	Keys <-chan Key

	out      *bufio.Writer
	fd       int
	oldState *term.State

	mu       sync.Mutex
	closed   bool
	lastW    int
	lastH    int
	lastDrew int // number of lines drawn in the previous frame
}

// NewScreen puts the terminal into raw mode on the alternate screen. The
// caller must call Close, ideally from a defer, or the user's shell is left in
// a broken state.
func NewScreen() (*Screen, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return nil, fmt.Errorf("stdin is not a terminal; use the non-interactive subcommands instead")
	}
	old, err := term.MakeRaw(fd)
	if err != nil {
		return nil, fmt.Errorf("entering raw mode: %w", err)
	}
	keys := make(chan Key, 32)
	s := &Screen{
		Keys:     keys,
		out:      bufio.NewWriterSize(os.Stdout, 1<<16),
		fd:       fd,
		oldState: old,
	}
	s.write(altScreenOn + cursorHide + clearAll + cursorHome)
	s.out.Flush()
	go readKeys(os.Stdin, keys)
	return s, nil
}

func (s *Screen) write(str string) { s.out.WriteString(str) }

// Close restores the terminal. It is safe to call more than once.
func (s *Screen) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	s.write(sgrReset + altScreenOff + cursorShow)
	s.out.Flush()
	term.Restore(s.fd, s.oldState)
}

// Size returns the current terminal dimensions, with sane fallbacks if the
// terminal won't say.
func (s *Screen) Size() (w, h int) {
	w, h, err := term.GetSize(s.fd)
	if err != nil || w <= 0 || h <= 0 {
		return 80, 24
	}
	return w, h
}

// Render paints exactly one frame. Lines are clipped to the terminal width and
// each is cleared to end-of-line, which avoids the flicker you get from
// clearing the whole screen between frames.
func (s *Screen) Render(lines []string, w, h int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	// A resize can leave stale glyphs outside the new bounds; a full clear on
	// the frame after a resize is cheap and fixes it.
	if w != s.lastW || h != s.lastH {
		s.write(clearAll)
		s.lastW, s.lastH = w, h
	}
	s.write(cursorHome)
	n := len(lines)
	if n > h {
		n = h
	}
	for i := range n {
		s.write(truncate(lines[i], w))
		s.write(sgrReset + clearToEOL)
		if i < n-1 {
			s.write("\r\n")
		}
	}
	s.write(clearBelow)
	s.lastDrew = n
	s.out.Flush()
}

// readKeys decodes stdin into Key values.
//
// Terminals deliver an escape sequence in a single read, so we parse whole
// buffers rather than trying to time out between bytes.
func readKeys(f *os.File, out chan<- Key) {
	defer close(out)
	buf := make([]byte, 256)
	for {
		n, err := f.Read(buf)
		if err != nil {
			return
		}
		b := buf[:n]
		for len(b) > 0 {
			k, used := decodeKey(b)
			if used == 0 {
				used = 1
			}
			b = b[used:]
			out <- k
		}
	}
}

func decodeKey(b []byte) (Key, int) {
	switch b[0] {
	case 0x03:
		return Key{Type: KeyCtrlC}, 1
	case '\r', '\n':
		return Key{Type: KeyEnter}, 1
	case '\t':
		return Key{Type: KeyTab}, 1
	case 0x7f, 0x08:
		return Key{Type: KeyBackspace}, 1
	case 0x1b:
		if len(b) == 1 {
			return Key{Type: KeyEsc}, 1
		}
		return decodeEscape(b)
	}
	if b[0] < 0x20 {
		return Key{Type: KeyUnknown}, 1
	}
	r, size := decodeRune(b)
	return Key{Type: KeyRune, Rune: r}, size
}

func decodeEscape(b []byte) (Key, int) {
	// CSI sequences: ESC [ ... final
	if b[1] == '[' || b[1] == 'O' {
		i := 2
		for i < len(b) && !(b[i] >= '@' && b[i] <= '~') {
			i++
		}
		if i >= len(b) {
			return Key{Type: KeyEsc}, 1
		}
		seq := string(b[2:i])
		final := b[i]
		used := i + 1
		switch final {
		case 'A':
			return Key{Type: KeyUp}, used
		case 'B':
			return Key{Type: KeyDown}, used
		case 'C':
			return Key{Type: KeyRight}, used
		case 'D':
			return Key{Type: KeyLeft}, used
		case 'H':
			return Key{Type: KeyHome}, used
		case 'F':
			return Key{Type: KeyEnd}, used
		case '~':
			switch seq {
			case "1", "7":
				return Key{Type: KeyHome}, used
			case "4", "8":
				return Key{Type: KeyEnd}, used
			case "5":
				return Key{Type: KeyPgUp}, used
			case "6":
				return Key{Type: KeyPgDn}, used
			}
		}
		return Key{Type: KeyUnknown}, used
	}
	// ESC followed by a printable rune (Alt-key); treat as Esc.
	return Key{Type: KeyEsc}, 1
}

func decodeRune(b []byte) (rune, int) {
	r := []rune(string(b))
	if len(r) == 0 {
		return 0, 1
	}
	return r[0], len(string(r[0]))
}

// hrule draws a horizontal rule of width w in the gradient, used to separate
// panes.
func hrule(w int, c rgb) string {
	if w <= 0 {
		return ""
	}
	return c.paint(strings.Repeat("─", w))
}

// gradientRule draws a rule that sweeps through the palette.
func gradientRule(w int) string {
	if w <= 0 {
		return ""
	}
	var b strings.Builder
	for i := range w {
		b.WriteString(gradient(float64(i) / float64(w)).fg())
		b.WriteRune('━')
	}
	b.WriteString(sgrReset)
	return b.String()
}

// gradientText colors each character of s along the palette.
func gradientText(s string) string {
	runes := []rune(s)
	var b strings.Builder
	b.WriteString(sgrBold)
	for i, r := range runes {
		if len(runes) > 1 {
			b.WriteString(gradient(float64(i) / float64(len(runes)-1)).fg())
		}
		b.WriteRune(r)
	}
	b.WriteString(sgrReset)
	return b.String()
}
