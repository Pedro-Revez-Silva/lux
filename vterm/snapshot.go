package vterm

import "strconv"

// Snapshot is a byte sequence that, written to a blank terminal of the
// same size, reproduces this one: it resets the modes Snapshot restores,
// clears the screen, writes the history lines (so they scroll into the
// receiving terminal's own history) and the main screen, then the
// alternate screen if it is shown, and finally the scroll region, modes,
// cursor and title.
//
// It does not send RIS (ESC c): on many terminals that also wipes their
// scrollback, which a person attaching keeps.
//
// With limit > 0, the oldest history lines are left out as needed to keep
// it within limit bytes. If the screen part alone (main, and alternate when
// shown) is over limit, there is no snapshot: Snapshot returns nil, and a
// client goes on without one. Work and allocation are bounded by a small
// multiple of limit, however long the history: the screen part is
// rendered first, then history from the newest line back while it fits.
func (t *Terminal) Snapshot(limit int) []byte {
	prefix := t.appendPrefix(make([]byte, 0, 256))
	screens := 1
	if t.scr == &t.alt {
		screens = 2
	}
	hint := screens*t.rows*(t.cols+16) + 512
	room := 0 // the screen part's bound; 0 none
	if limit > 0 {
		if room = limit - len(prefix); room <= 0 {
			return nil
		}
		hint = min(hint, room)
	}
	scr, ok := t.appendScreen(make([]byte, 0, hint), room)
	if !ok {
		return nil
	}

	// History newest to oldest, each line rendered once into hist while
	// it fits; ends[i] is where the i-th newest line ends in it.
	budget := room - len(scr)
	var hist, line []byte
	if limit > 0 {
		hist = make([]byte, 0, min(budget, t.hist.n*(t.cols+16)))
	}
	ends := make([]int, 0, min(t.hist.n, 1024))
	for i := t.hist.n - 1; i >= 0; i-- {
		line = t.appendLine(line[:0], t.HistoryLine(i), true)
		line = append(line, "\r\n"...)
		if limit > 0 && len(hist)+len(line) > budget {
			break
		}
		hist = appendCapped(hist, line, budget)
		ends = append(ends, len(hist))
	}
	b := make([]byte, 0, len(prefix)+len(hist)+len(scr))
	b = append(b, prefix...)
	for k := len(ends) - 1; k >= 0; k-- {
		start := 0
		if k > 0 {
			start = ends[k-1]
		}
		b = append(b, hist[start:ends[k]]...)
	}
	return append(b, scr...)
}

// appendCapped appends p to b, growing b no further than limit (when
// limit > 0) so a snapshot's allocation stays within its limit.
func appendCapped(b, p []byte, limit int) []byte {
	if n := len(b) + len(p); limit > 0 && n > cap(b) {
		nb := make([]byte, len(b), min(max(2*cap(b), n), max(limit, n)))
		copy(nb, b)
		b = nb
	}
	return append(b, p...)
}

// appendPrefix resets what Snapshot later sets and clears the screen.
func (t *Terminal) appendPrefix(b []byte) []byte {
	b = append(b, "\x1b[?25l\x1b[?1049l\x1b[r\x1b[m\x1b[?6l\x1b[?7h\x1b[4l\x1b[20l\x1b>\x1b(B\x1b)B\x0f\x1b[0 q"...)
	for _, n := range replayModes {
		b = csiMode(b, n, false)
	}
	return append(b, "\x1b[H\x1b[2J"...)
}

// appendScreen is the part of Snapshot after the history. With room > 0
// it gives up (false) once it would be over room bytes, and never grows
// its buffer past room.
func (t *Terminal) appendScreen(b []byte, room int) ([]byte, bool) {
	var line []byte
	// putLine appends l, rendered, failing when it does not fit.
	putLine := func(l []Cell, trim bool) bool {
		line = t.appendLine(line[:0], l, trim)
		if room > 0 && len(b)+len(line) > room {
			return false
		}
		b = appendCapped(b, line, room)
		return true
	}
	for y, l := range t.main.lines {
		if y > 0 {
			b = append(b, "\r\n"...)
		}
		if !putLine(l, true) {
			return nil, false
		}
	}

	if t.scr == &t.alt {
		ms := t.main.saved
		if t.altMode == 1049 {
			// 1049 saves the main screen's cursor itself, as it was then.
			if ms.valid {
				b = t.appendCursorState(b, ms)
			}
			b = append(b, "\x1b[?1049h"...)
			b = resetCursorState(b)
		} else {
			b = t.appendSaved(b, ms)
			b = csiMode(b, t.altMode, true)
		}
		b = append(b, "\x1b[m\x1b[H\x1b[2J"...)
		for y, l := range t.alt.lines {
			b = cup(b, y, 0)
			if !putLine(l, false) {
				return nil, false
			}
		}
		b = t.appendSaved(b, t.alt.saved)
	} else {
		b = t.appendSaved(b, t.main.saved)
	}

	b = t.appendTabs(b)
	if t.top != 0 || t.bot != t.rows {
		b = append(b, "\x1b["...)
		b = strconv.AppendInt(b, int64(t.top+1), 10)
		b = append(b, ';')
		b = strconv.AppendInt(b, int64(t.bot), 10)
		b = append(b, 'r')
	}
	for _, n := range replayModes {
		if t.dec[n] {
			b = csiMode(b, n, true)
		}
	}
	if t.keypad {
		b = append(b, "\x1b="...)
	}
	if !t.autowrap {
		b = csiMode(b, 7, false)
	}

	// The cursor. Pending wrap (the last column written, the next
	// character going to the next line) is reproduced by writing that
	// column's character again.
	b = append(b, "\x1b[m"...)
	y := t.cur.y
	if t.origin {
		b = csiMode(b, 6, true)
		y -= t.top
	}
	if t.cur.wrapNext {
		x := t.cols - 1
		l := t.scr.lines[t.cur.y]
		if l[x].Flags&FlagCont != 0 && x > 0 {
			x--
		}
		b = cup(b, y, x)
		b = t.appendSGR(b, l[x].Style)
		b = append(b, t.Text(l[x])...)
	} else {
		b = cup(b, y, t.cur.x)
	}
	if t.insert {
		b = append(b, "\x1b[4h"...)
	}
	if t.newline {
		b = append(b, "\x1b[20h"...)
	}
	b = appendCharsets(b, t.g, t.gl)
	b = t.appendSGR(b, t.cur.pen)
	if t.cursorStyle != 0 {
		b = append(b, "\x1b["...)
		b = strconv.AppendInt(b, int64(t.cursorStyle), 10)
		b = append(b, " q"...)
	}
	if t.title != "" {
		b = append(b, "\x1b]2;"...)
		b = append(b, t.title...)
		b = append(b, 0x07)
	}
	if !t.cursorHidden {
		b = append(b, "\x1b[?25h"...)
	}
	b = append(b, t.pending()...)
	if room > 0 && len(b) > room {
		return nil, false
	}
	return b, true
}

// appendTabs sets the tab stops, when they are not the default every 8
// columns. It moves the cursor.
func (t *Terminal) appendTabs(b []byte) []byte {
	def := true
	for x, set := range t.tabs {
		if set != (x > 0 && x%8 == 0) {
			def = false
			break
		}
	}
	if def {
		return b
	}
	b = append(b, "\x1b[3g"...)
	for x, set := range t.tabs {
		if set {
			b = cup(b, 0, x)
			b = append(b, "\x1bH"...)
		}
	}
	return b
}

// appendSaved reproduces a screen's saved cursor (DECSC): its position,
// pen, origin mode and character sets.
func (t *Terminal) appendSaved(b []byte, s savedCursor) []byte {
	if !s.valid {
		return b
	}
	b = t.appendCursorState(b, s)
	b = append(b, "\x1b7"...)
	return resetCursorState(b)
}

// appendCursorState sets what DECSC saves to s. The scroll region is the
// whole screen when it is called, so origin mode leaves positions as
// they are.
func (t *Terminal) appendCursorState(b []byte, s savedCursor) []byte {
	if s.origin {
		b = csiMode(b, 6, true)
	}
	b = appendCharsets(b, s.g, s.gl)
	b = cup(b, s.y, s.x)
	return t.appendSGR(b, s.pen)
}

// appendCharsets designates G0 and G1 and shifts to gl, from the defaults
// (both ASCII, G0 shifted in).
func appendCharsets(b []byte, g [2]charset, gl int) []byte {
	if g[0] == csGraphics {
		b = append(b, "\x1b(0"...)
	}
	if g[1] == csGraphics {
		b = append(b, "\x1b)0"...)
	}
	if gl == 1 {
		b = append(b, 0x0e)
	}
	return b
}

func resetCursorState(b []byte) []byte {
	return append(b, "\x1b[?6l\x1b(B\x1b)B\x0f\x1b[m"...)
}

// appendLine writes a line's cells from the cursor at its column 0. With
// trim, trailing blanks in the default style are left out (the line was
// cleared); the style is reset at the end, so a scroll that follows
// clears in the default background.
func (t *Terminal) appendLine(b []byte, l []Cell, trim bool) []byte {
	end := len(l)
	if trim {
		for end > 0 && l[end-1] == (Cell{}) {
			end--
		}
	}
	var pen Style
	for x := 0; x < end; x++ {
		c := l[x]
		if c.Flags&FlagCont != 0 && x > 0 && l[x-1].Flags&FlagWide != 0 {
			continue
		}
		if c.Style != pen {
			b = t.appendSGR(b, c.Style)
			pen = c.Style
		}
		if c.Flags&FlagCont != 0 {
			b = append(b, ' ')
			continue
		}
		b = append(b, t.Text(c)...)
	}
	if pen != (Style{}) {
		b = append(b, "\x1b[m"...)
	}
	return b
}

func (t *Terminal) appendSGR(b []byte, s Style) []byte {
	b = append(b, "\x1b[0"...)
	a := s.Attr
	for _, f := range []struct {
		bit Attr
		n   byte
	}{{AttrBold, '1'}, {AttrFaint, '2'}, {AttrItalic, '3'}, {AttrBlink, '5'}, {AttrReverse, '7'}, {AttrInvisible, '8'}, {AttrStrike, '9'}} {
		if a&f.bit != 0 {
			b = append(b, ';', f.n)
		}
	}
	switch u := a.Underline(); u {
	case 0:
	case 1:
		b = append(b, ";4"...)
	default:
		b = append(b, ";4:"...)
		b = strconv.AppendInt(b, int64(u), 10)
	}
	b = appendColor(b, s.Fg, 30, 90, "38")
	b = appendColor(b, s.Bg, 40, 100, "48")
	return append(b, 'm')
}

func appendColor(b []byte, c Color, base, bright int, ext string) []byte {
	switch c & colorKind {
	case colorIndexed:
		n := int(c & 0xff)
		switch {
		case n < 8:
			b = append(b, ';')
			b = strconv.AppendInt(b, int64(base+n), 10)
		case n < 16:
			b = append(b, ';')
			b = strconv.AppendInt(b, int64(bright+n-8), 10)
		default:
			b = append(b, ';')
			b = append(b, ext...)
			b = append(b, ";5;"...)
			b = strconv.AppendInt(b, int64(n), 10)
		}
	case colorRGB:
		b = append(b, ';')
		b = append(b, ext...)
		b = append(b, ";2;"...)
		b = strconv.AppendInt(b, int64(c>>16&0xff), 10)
		b = append(b, ';')
		b = strconv.AppendInt(b, int64(c>>8&0xff), 10)
		b = append(b, ';')
		b = strconv.AppendInt(b, int64(c&0xff), 10)
	}
	return b
}

// cup moves the cursor to row y, column x (0-based).
func cup(b []byte, y, x int) []byte {
	b = append(b, "\x1b["...)
	b = strconv.AppendInt(b, int64(y+1), 10)
	b = append(b, ';')
	b = strconv.AppendInt(b, int64(x+1), 10)
	return append(b, 'H')
}

func csiMode(b []byte, n int, on bool) []byte {
	b = append(b, "\x1b[?"...)
	b = strconv.AppendInt(b, int64(n), 10)
	if on {
		return append(b, 'h')
	}
	return append(b, 'l')
}
