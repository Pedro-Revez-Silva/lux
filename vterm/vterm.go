// Package vterm keeps a terminal's parsed state — the screen, cursor,
// modes, title and a bounded scrollback — from the bytes a program writes
// to it, and renders that state as bytes that reproduce it on a blank
// terminal (Snapshot). It is what lets a client attach to a running
// terminal and see its screen, as tmux does, rather than a tail of bytes.
//
// Memory is bounded by the size: two screens of rows×cols cells and at
// most the history limit of main-screen lines, each at most cols cells,
// at 20 bytes a cell. With a 2000-line history that is about 3.3 MB at
// 24×80 and 24 MB at the largest size, 200×500 (MaxRows×MaxCols), plus at
// most 4096 interned grapheme clusters of up to 32 bytes.
// The alternate screen has no history. A Terminal is not safe for
// concurrent use.
//
// lux-shim feeds every tty workload through one; clients outside this
// module (a multiplexer repainting a window it switches to) can use the
// same package so both sides agree on what a screen is. It follows lux
// releases.
package vterm

import (
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/width"
)

// Size limits: a resize beyond them is clamped, so a client cannot make
// the emulator allocate without bound. 200×500 is more than a 4K display
// shows at a small font. A snapshot whose screen part alone is over its
// limit is not made (Snapshot returns nil), which only colour-heavy
// screens near these sizes reach.
const (
	MaxRows = 200
	MaxCols = 500
)

// Color is a cell's colour: 0 the terminal's default, else Indexed or RGB.
type Color uint32

const (
	colorIndexed Color = 1 << 24
	colorRGB     Color = 2 << 24
	colorKind    Color = 0xff << 24
)

// Indexed is palette colour n (0–255).
func Indexed(n uint8) Color { return colorIndexed | Color(n) }

// RGB is a true colour.
func RGB(r, g, b uint8) Color { return colorRGB | Color(r)<<16 | Color(g)<<8 | Color(b) }

// Attr is a set of text attributes; bits 8–10 hold the underline style.
type Attr uint16

const (
	AttrBold Attr = 1 << iota
	AttrFaint
	AttrItalic
	AttrBlink
	AttrReverse
	AttrInvisible
	AttrStrike
)

const (
	ulShift = 8
	ulMask  = Attr(7) << ulShift
)

// Underline returns the underline style (0 none, 1 single, 2 double,
// 3 curly, 4 dotted, 5 dashed).
func (a Attr) Underline() int { return int(a&ulMask) >> ulShift }

func (a Attr) withUnderline(n int) Attr {
	if n < 0 || n > 5 {
		n = 1
	}
	return a&^ulMask | Attr(n)<<ulShift
}

// Style is how a cell is drawn.
type Style struct {
	Fg, Bg Color
	Attr   Attr
}

// Cell is one column of a line. The zero Cell is a blank in the default
// style. A wide character takes its cell (Wide) and the next (Cont).
type Cell struct {
	Ch    rune // 0 blank; >= clusterBase: a grapheme cluster (Terminal.Text)
	Style Style
	Flags uint8
}

const (
	FlagWide uint8 = 1 << iota
	FlagCont
)

// Combining sequences are interned: a cell's Ch at or above clusterBase
// indexes clusters. Bounded: past maxClusters, marks are dropped.
const (
	clusterBase   = 0x110000
	maxClusters   = 4096
	maxClusterLen = 32
)

type charset uint8

const (
	csASCII charset = iota
	csGraphics
)

type cursor struct {
	x, y     int
	pen      Style
	wrapNext bool
}

type savedCursor struct {
	cursor
	origin bool
	g      [2]charset
	gl     int
	valid  bool
}

type screen struct {
	lines [][]Cell
	saved savedCursor
}

// Parser states (a VT500-style state machine, reduced to what is kept).
const (
	stGround = iota
	stEsc
	stEscInter
	stCSI
	stOSC
	stOSCEsc
	stString // DCS, SOS, PM, APC: skipped to ST
	stStringEsc
)

const (
	maxParams = 16
	maxOSC    = 4096
	maxPend   = 256
)

// Terminal is the emulated terminal.
type Terminal struct {
	rows, cols int
	main, alt  screen
	scr        *screen
	altMode    int // the DEC mode that entered the alternate screen: 47, 1047 or 1049
	hist       history

	cur          cursor
	top, bot     int // scroll region, [top, bot)
	origin       bool
	autowrap     bool
	insert       bool // IRM
	newline      bool // LNM
	keypad       bool // DECKPAM
	cursorHidden bool
	cursorStyle  int // DECSCUSR
	dec          map[int]bool
	title        string
	tabs         []bool
	g            [2]charset
	gl           int
	lastChar     rune

	clusters  []string
	clusterID map[string]rune

	// Parser.
	st     int
	u8     [utf8.UTFMax]byte
	u8n    int
	prefix byte
	inter  byte
	params [maxParams]int
	sub    [maxParams]bool
	np     int
	pcur   int
	pseen  bool
	osc    []byte
	pend   []byte
}

// replayModes are the DEC private modes a client needs to behave as the
// program expects, in the order Snapshot sets them.
var replayModes = []int{
	1,    // DECCKM: application cursor keys
	5,    // DECSCNM: reverse video
	12,   // cursor blink
	66,   // DECNKM: application keypad
	1004, // focus events
	1005, // UTF-8 mouse encoding
	1006, // SGR mouse encoding
	1015, // urxvt mouse encoding
	9,    // X10 mouse
	1000, // normal mouse tracking
	1001, // highlight tracking
	1002, // button-event tracking
	1003, // any-event tracking
	2004, // bracketed paste
}

var isReplayMode = func() map[int]bool {
	m := map[int]bool{}
	for _, n := range replayModes {
		m[n] = true
	}
	return m
}()

// New is a blank terminal of rows×cols keeping at most historyLimit lines
// of main-screen scrollback.
func New(rows, cols, historyLimit int) *Terminal {
	rows, cols = clampSize(rows, cols)
	t := &Terminal{rows: rows, cols: cols, hist: history{limit: max(historyLimit, 0)}, clusterID: map[string]rune{}}
	t.main.lines = blankLines(rows, cols)
	t.alt.lines = blankLines(rows, cols)
	t.reset()
	return t
}

func clampSize(rows, cols int) (int, int) {
	return min(max(rows, 1), MaxRows), min(max(cols, 1), MaxCols)
}

func blankLines(rows, cols int) [][]Cell {
	ls := make([][]Cell, rows)
	for i := range ls {
		ls[i] = make([]Cell, cols)
	}
	return ls
}

// reset is RIS: everything but the history.
func (t *Terminal) reset() {
	for _, s := range []*screen{&t.main, &t.alt} {
		for _, l := range s.lines {
			clear(l)
		}
		s.saved = savedCursor{}
	}
	t.scr, t.altMode = &t.main, 0
	t.cur = cursor{}
	t.top, t.bot = 0, t.rows
	t.origin, t.autowrap, t.insert, t.newline, t.keypad = false, true, false, false, false
	t.cursorHidden, t.cursorStyle = false, 0
	t.dec = map[int]bool{}
	t.title = ""
	t.resetTabs()
	t.g, t.gl = [2]charset{}, 0
	t.lastChar = 0
}

func (t *Terminal) resetTabs() {
	t.tabs = make([]bool, t.cols)
	for x := 8; x < t.cols; x += 8 {
		t.tabs[x] = true
	}
}

// Size is rows×cols.
func (t *Terminal) Size() (rows, cols int) { return t.rows, t.cols }

// Cell is the cell at column x of row y of the current screen.
func (t *Terminal) Cell(x, y int) Cell {
	if y < 0 || y >= t.rows || x < 0 || x >= t.cols {
		return Cell{}
	}
	return t.scr.lines[y][x]
}

// MainCell is the cell at x, y of the main screen, whichever is shown.
func (t *Terminal) MainCell(x, y int) Cell {
	if y < 0 || y >= t.rows || x < 0 || x >= t.cols {
		return Cell{}
	}
	return t.main.lines[y][x]
}

// Text is what a cell shows: " " for a blank, "" for the second half of a
// wide character.
func (t *Terminal) Text(c Cell) string {
	switch {
	case c.Flags&FlagCont != 0:
		return ""
	case c.Ch == 0:
		return " "
	case c.Ch >= clusterBase && int(c.Ch-clusterBase) < len(t.clusters):
		return t.clusters[c.Ch-clusterBase]
	}
	return string(c.Ch)
}

// HistoryLen is the number of scrollback lines kept.
func (t *Terminal) HistoryLen() int { return t.hist.n }

// HistoryLine is scrollback line i, 0 the oldest, without trailing
// blanks and cut to the current width (a line kept from before a resize
// to fewer columns is not rewrapped).
func (t *Terminal) HistoryLine(i int) []Cell {
	l := t.hist.line(i)
	if len(l) > t.cols {
		l = l[:t.cols]
		if l[t.cols-1].Flags&FlagWide != 0 {
			l = l[:t.cols-1]
		}
	}
	return l
}

// Cursor is the cursor's position and whether it is shown.
func (t *Terminal) Cursor() (x, y int, visible bool) { return t.cur.x, t.cur.y, !t.cursorHidden }

// AltScreen reports whether the alternate screen is shown.
func (t *Terminal) AltScreen() bool { return t.scr == &t.alt }

// Mode reports whether DEC private mode n is set (those Snapshot restores,
// plus 6 origin, 7 autowrap and 25 cursor visible).
func (t *Terminal) Mode(n int) bool {
	switch n {
	case 6:
		return t.origin
	case 7:
		return t.autowrap
	case 25:
		return !t.cursorHidden
	case 47, 1047, 1049:
		return t.altMode == n
	}
	return t.dec[n]
}

// Title is the window title (OSC 0 or 2).
func (t *Terminal) Title() string { return t.title }

// ScrollRegion is the scroll region's top and bottom rows, inclusive.
func (t *Terminal) ScrollRegion() (top, bottom int) { return t.top, t.bot - 1 }

// Write feeds output of the program to the terminal.
func (t *Terminal) Write(p []byte) {
	for i := 0; i < len(p); {
		// The common case, printable ASCII in the ground state, skips the
		// state machine a byte at a time.
		if t.st == stGround && t.u8n == 0 {
			j := i
			for j < len(p) && p[j] >= 0x20 && p[j] < 0x7f {
				j++
			}
			if j > i {
				t.printASCII(p[i:j])
				i = j
				continue
			}
		}
		t.step(p[i])
		i++
	}
}

func (t *Terminal) step(b byte) {
	switch t.st {
	case stGround:
		if t.u8n > 0 || b >= 0x80 {
			t.utf8(b)
			return
		}
		if b < 0x20 || b == 0x7f {
			t.control(b)
			return
		}
		t.print(rune(b))
	case stEsc, stEscInter, stCSI:
		if b < 0x20 || b == 0x7f {
			t.controlInSequence(b)
			return
		}
		if b >= 0x80 {
			return
		}
		if len(t.pend) < maxPend {
			t.pend = append(t.pend, b)
		}
		if t.st == stCSI {
			t.csiByte(b)
		} else {
			t.escByte(b)
		}
	case stOSC:
		switch {
		case b == 0x07:
			t.dispatchOSC()
			t.st = stGround
		case b == 0x1b:
			t.st = stOSCEsc
		case b == 0x18 || b == 0x1a:
			t.st = stGround
		case b < 0x20:
		default:
			if len(t.osc) < maxOSC {
				t.osc = append(t.osc, b)
			}
		}
	case stOSCEsc:
		t.dispatchOSC()
		t.st = stGround
		if b != '\\' {
			t.enterEsc()
			t.step(b)
		}
	case stString:
		switch b {
		case 0x1b:
			t.st = stStringEsc
		case 0x18, 0x1a:
			t.st = stGround
		}
	case stStringEsc:
		t.st = stGround
		if b != '\\' {
			t.enterEsc()
			t.step(b)
		}
	}
}

func (t *Terminal) utf8(b byte) {
	if b < 0x80 || (t.u8n > 0 && b&0xc0 != 0x80) {
		// A sequence cut short: what was collected is one bad character.
		t.u8n = 0
		t.print(utf8.RuneError)
		t.step(b)
		return
	}
	if t.u8n == 0 && (b < 0xc2 || b > 0xf4) {
		t.print(utf8.RuneError)
		return
	}
	t.u8[t.u8n] = b
	t.u8n++
	if !utf8.FullRune(t.u8[:t.u8n]) {
		return
	}
	r, _ := utf8.DecodeRune(t.u8[:t.u8n])
	t.u8n = 0
	t.print(r)
}

func (t *Terminal) controlInSequence(b byte) {
	switch b {
	case 0x18, 0x1a:
		t.st = stGround
	case 0x1b:
		t.enterEsc()
	case 0x7f:
	default:
		t.control(b)
	}
}

func (t *Terminal) enterEsc() {
	t.st, t.inter = stEsc, 0
	t.pend = append(t.pend[:0], 0x1b)
}

// pending is the part of a sequence (or UTF-8 character) the output ended
// in the middle of: Snapshot ends with it, so the rest of it, sent live,
// completes it on the client as it did here.
func (t *Terminal) pending() []byte {
	switch t.st {
	case stGround:
		return t.u8[:t.u8n]
	case stEsc, stEscInter, stCSI:
		return t.pend
	case stOSC:
		return append([]byte("\x1b]"), t.osc...)
	case stOSCEsc:
		return append(append([]byte("\x1b]"), t.osc...), 0x1b)
	case stString:
		return []byte("\x1bP")
	case stStringEsc:
		return []byte("\x1bP\x1b")
	}
	return nil
}

func (t *Terminal) control(b byte) {
	switch b {
	case 0x08: // BS
		t.cur.wrapNext = false
		if t.cur.x > 0 {
			t.cur.x--
		}
	case 0x09: // HT
		t.tab(1)
	case 0x0a, 0x0b, 0x0c: // LF, VT, FF
		t.index()
		if t.newline {
			t.cur.x = 0
		}
	case 0x0d: // CR
		t.cur.x, t.cur.wrapNext = 0, false
	case 0x0e: // SO
		t.gl = 1
	case 0x0f: // SI
		t.gl = 0
	case 0x1b:
		t.enterEsc()
	}
}

func (t *Terminal) escByte(b byte) {
	if b >= 0x20 && b <= 0x2f {
		t.inter, t.st = b, stEscInter
		return
	}
	if t.st == stEsc {
		switch b {
		case '[':
			t.st, t.prefix, t.inter, t.np, t.pcur, t.pseen = stCSI, 0, 0, 0, -1, false
			return
		case ']':
			t.st, t.osc = stOSC, t.osc[:0]
			return
		case 'P', 'X', '^', '_':
			t.st = stString
			return
		}
	}
	t.st = stGround
	t.dispatchEsc(t.inter, b)
}

func (t *Terminal) dispatchEsc(inter, b byte) {
	switch inter {
	case 0:
		switch b {
		case '7':
			t.saveCursor()
		case '8':
			t.restoreCursor()
		case 'D':
			t.index()
		case 'E':
			t.index()
			t.cur.x = 0
		case 'M':
			t.reverseIndex()
		case 'H':
			t.tabs[t.cur.x] = true
		case 'c':
			t.reset()
		case '=':
			t.keypad = true
		case '>':
			t.keypad = false
		}
	case '(', ')':
		cs := csASCII
		if b == '0' {
			cs = csGraphics
		}
		t.g[inter-'('] = cs
	case '#':
		if b == '8' { // DECALN
			for _, l := range t.scr.lines {
				for x := range l {
					l[x] = Cell{Ch: 'E'}
				}
			}
			t.top, t.bot = 0, t.rows
			t.cur.x, t.cur.y, t.cur.wrapNext = 0, 0, false
		}
	}
}

func (t *Terminal) csiByte(b byte) {
	switch {
	case b >= '0' && b <= '9':
		if t.pcur < 0 {
			t.pcur = 0
		}
		t.pcur = min(t.pcur*10+int(b-'0'), 65535)
		t.pseen = true
	case b == ';' || b == ':':
		t.pushParam()
		if t.np < maxParams {
			t.sub[t.np] = b == ':'
		}
		t.pseen = true
	case b >= '<' && b <= '?':
		if !t.pseen && t.prefix == 0 {
			t.prefix = b
		}
	case b >= 0x20 && b <= 0x2f:
		t.inter = b
	case b >= 0x40 && b <= 0x7e:
		if t.pseen {
			t.pushParam()
		}
		t.st = stGround
		t.dispatchCSI(b)
	}
}

func (t *Terminal) pushParam() {
	if t.np < maxParams {
		t.params[t.np] = t.pcur
		if t.np == 0 {
			t.sub[0] = false
		}
		t.np++
	}
	t.pcur = -1
}

// arg is parameter i, def if it is missing or 0.
func (t *Terminal) arg(i, def int) int {
	if i >= t.np || t.params[i] <= 0 {
		return def
	}
	return t.params[i]
}

// raw is parameter i, 0 if missing.
func (t *Terminal) raw(i int) int {
	if i >= t.np || t.params[i] < 0 {
		return 0
	}
	return t.params[i]
}

func (t *Terminal) dispatchCSI(final byte) {
	if t.prefix == '?' {
		if t.inter == 0 && (final == 'h' || final == 'l') {
			for i := 0; i < t.np; i++ {
				t.setDECMode(t.raw(i), final == 'h')
			}
		}
		return
	}
	if t.prefix != 0 {
		return // CSI > and CSI = sequences: not kept
	}
	if t.inter != 0 {
		switch {
		case t.inter == ' ' && final == 'q': // DECSCUSR
			t.cursorStyle = t.raw(0)
		case t.inter == '!' && final == 'p': // DECSTR
			t.softReset()
		}
		return
	}
	switch final {
	case '@':
		t.insertCells(t.arg(0, 1))
	case 'A':
		t.cursorUp(t.arg(0, 1))
	case 'B', 'e':
		t.cursorDown(t.arg(0, 1))
	case 'C', 'a':
		t.moveTo(t.cur.x+t.arg(0, 1), t.cur.y)
	case 'D':
		t.moveTo(t.cur.x-t.arg(0, 1), t.cur.y)
	case 'E':
		t.cursorDown(t.arg(0, 1))
		t.cur.x = 0
	case 'F':
		t.cursorUp(t.arg(0, 1))
		t.cur.x = 0
	case 'G', '`':
		t.moveTo(t.arg(0, 1)-1, t.cur.y)
	case 'H', 'f':
		t.cursorPosition(t.arg(0, 1)-1, t.arg(1, 1)-1)
	case 'I':
		t.tab(t.arg(0, 1))
	case 'J':
		t.eraseDisplay(t.raw(0))
	case 'K':
		t.eraseLine(t.raw(0))
	case 'L':
		t.insertLines(t.arg(0, 1))
	case 'M':
		t.deleteLines(t.arg(0, 1))
	case 'P':
		t.deleteCells(t.arg(0, 1))
	case 'S':
		t.scrollUp(t.top, t.bot, t.arg(0, 1))
	case 'T':
		t.scrollDown(t.top, t.bot, t.arg(0, 1))
	case 'X':
		n := min(t.arg(0, 1), t.cols-t.cur.x)
		t.blank(t.scr.lines[t.cur.y], t.cur.x, t.cur.x+n)
		t.cur.wrapNext = false
	case 'Z':
		t.backTab(t.arg(0, 1))
	case 'b':
		if t.lastChar != 0 {
			for range min(t.arg(0, 1), t.rows*t.cols) {
				t.print(t.lastChar)
			}
		}
	case 'd':
		t.cursorPosition(t.arg(0, 1)-1, t.cur.x)
	case 'g':
		switch t.raw(0) {
		case 0:
			t.tabs[t.cur.x] = false
		case 3:
			clear(t.tabs)
		}
	case 'h', 'l':
		for i := 0; i < t.np; i++ {
			switch t.raw(i) {
			case 4:
				t.insert = final == 'h'
			case 20:
				t.newline = final == 'h'
			}
		}
	case 'm':
		t.sgr()
	case 'r':
		top, bot := t.arg(0, 1)-1, t.arg(1, t.rows)
		bot = min(bot, t.rows)
		if top < bot-1 {
			t.top, t.bot = top, bot
			t.cursorPosition(0, 0)
		}
	case 's':
		t.saveCursor()
	case 'u':
		t.restoreCursor()
	}
}

func (t *Terminal) setDECMode(n int, on bool) {
	switch n {
	case 6:
		t.origin = on
		t.cursorPosition(0, 0)
	case 7:
		t.autowrap = on
		if !on {
			t.cur.wrapNext = false
		}
	case 25:
		t.cursorHidden = !on
	case 47, 1047:
		if on {
			t.enterAlt(n)
		} else {
			t.leaveAlt()
		}
	case 1048:
		if on {
			t.saveCursor()
		} else {
			t.restoreCursor()
		}
	case 1049:
		if on {
			if t.scr != &t.alt {
				t.saveCursor()
				t.enterAlt(n)
			}
		} else if t.scr == &t.alt {
			t.leaveAlt()
			t.restoreCursor()
		}
	default:
		if isReplayMode[n] {
			if on {
				t.dec[n] = true
			} else {
				delete(t.dec, n)
			}
		}
	}
}

// enterAlt shows the alternate screen, cleared, whichever mode asks for
// it (as tmux does): what it held before is never shown again, so the
// state while the main screen is shown does not depend on it.
func (t *Terminal) enterAlt(mode int) {
	if t.scr == &t.alt {
		return
	}
	for _, l := range t.alt.lines {
		clear(l)
	}
	t.alt.saved = savedCursor{}
	t.scr, t.altMode = &t.alt, mode
}

func (t *Terminal) leaveAlt() {
	t.scr, t.altMode = &t.main, 0
}

func (t *Terminal) softReset() {
	t.cursorHidden, t.insert, t.origin, t.autowrap, t.keypad = false, false, false, true, false
	delete(t.dec, 1)
	t.top, t.bot = 0, t.rows
	t.cur.pen = Style{}
	t.cur.wrapNext = false
	t.g, t.gl = [2]charset{}, 0
	t.scr.saved = savedCursor{}
}

func (t *Terminal) saveCursor() {
	t.scr.saved = savedCursor{cursor: t.cur, origin: t.origin, g: t.g, gl: t.gl, valid: true}
	t.scr.saved.wrapNext = false
}

func (t *Terminal) restoreCursor() {
	s := t.scr.saved
	if !s.valid {
		t.cur.x, t.cur.y, t.cur.pen, t.cur.wrapNext = 0, 0, Style{}, false
		t.origin = false
		return
	}
	t.cur, t.origin, t.g, t.gl = s.cursor, s.origin, s.g, s.gl
	t.cur.x, t.cur.y = min(t.cur.x, t.cols-1), min(t.cur.y, t.rows-1)
	if t.origin {
		// In origin mode the cursor stays in the scroll region, as xterm
		// keeps it.
		t.cur.y = min(max(t.cur.y, t.top), t.bot-1)
	}
}

func (t *Terminal) dispatchOSC() {
	n, rest := 0, t.osc
	for len(rest) > 0 && rest[0] >= '0' && rest[0] <= '9' && n < 10000 {
		n = n*10 + int(rest[0]-'0')
		rest = rest[1:]
	}
	if len(rest) == 0 || rest[0] != ';' {
		return
	}
	if n == 0 || n == 2 {
		t.title = string(rest[1:])
	}
}

// Cursor movement.

func (t *Terminal) moveTo(x, y int) {
	t.cur.x, t.cur.y = min(max(x, 0), t.cols-1), min(max(y, 0), t.rows-1)
	t.cur.wrapNext = false
}

// cursorPosition is CUP: row and column 0-based, relative to the scroll
// region in origin mode.
func (t *Terminal) cursorPosition(row, col int) {
	if t.origin {
		t.moveTo(col, min(row+t.top, t.bot-1))
		return
	}
	t.moveTo(col, row)
}

func (t *Terminal) cursorUp(n int) {
	lim := 0
	if t.cur.y >= t.top {
		lim = t.top
	}
	t.moveTo(t.cur.x, max(t.cur.y-n, lim))
}

func (t *Terminal) cursorDown(n int) {
	lim := t.rows - 1
	if t.cur.y < t.bot {
		lim = t.bot - 1
	}
	t.moveTo(t.cur.x, min(t.cur.y+n, lim))
}

func (t *Terminal) tab(n int) {
	x := t.cur.x
	for ; n > 0 && x < t.cols-1; n-- {
		for x++; x < t.cols-1 && !t.tabs[x]; x++ {
		}
	}
	t.cur.x, t.cur.wrapNext = x, false
}

func (t *Terminal) backTab(n int) {
	x := t.cur.x
	for ; n > 0 && x > 0; n-- {
		for x--; x > 0 && !t.tabs[x]; x-- {
		}
	}
	t.cur.x, t.cur.wrapNext = x, false
}

func (t *Terminal) index() {
	t.cur.wrapNext = false
	switch {
	case t.cur.y == t.bot-1:
		t.scrollUp(t.top, t.bot, 1)
	case t.cur.y < t.rows-1:
		t.cur.y++
	}
}

func (t *Terminal) reverseIndex() {
	t.cur.wrapNext = false
	switch {
	case t.cur.y == t.top:
		t.scrollDown(t.top, t.bot, 1)
	case t.cur.y > 0:
		t.cur.y--
	}
}

// Editing.

// blankCell is what erasing leaves: a blank in the pen's background
// (background colour erase, as xterm and tmux do).
func (t *Terminal) blankCell() Cell { return Cell{Style: Style{Bg: t.cur.pen.Bg}} }

func (t *Terminal) blank(l []Cell, from, to int) {
	from, to = max(from, 0), min(to, len(l))
	if from >= to {
		return
	}
	t.splitWide(l, from)
	t.splitWide(l, to)
	b := t.blankCell()
	for x := from; x < to; x++ {
		l[x] = b
	}
}

// splitWide clears a wide character that a change starting at column x
// would cut in two.
func (t *Terminal) splitWide(l []Cell, x int) {
	if x <= 0 || x >= len(l) {
		return
	}
	if l[x].Flags&FlagCont != 0 {
		l[x-1] = Cell{Style: l[x-1].Style}
		l[x] = Cell{Style: l[x].Style}
	}
}

// scrollUp moves rows [top, bot) up n, lines leaving the top of a region
// at the top of the main screen going to the history.
func (t *Terminal) scrollUp(top, bot, n int) {
	n = min(n, bot-top)
	if n <= 0 {
		return
	}
	ls := t.scr.lines
	toHistory := t.scr == &t.main && top == 0 && t.hist.limit > 0
	b := t.blankCell()
	for range n {
		l := ls[top]
		if toHistory {
			if l = t.hist.push(l); len(l) != t.cols {
				l = make([]Cell, t.cols)
			}
		}
		copy(ls[top:bot-1], ls[top+1:bot])
		fill(l, b)
		ls[bot-1] = l
	}
}

func fill(l []Cell, b Cell) {
	if b == (Cell{}) {
		clear(l)
		return
	}
	for x := range l {
		l[x] = b
	}
}

func (t *Terminal) scrollDown(top, bot, n int) {
	n = min(n, bot-top)
	if n <= 0 {
		return
	}
	ls := t.scr.lines
	b := t.blankCell()
	for range n {
		l := ls[bot-1]
		copy(ls[top+1:bot], ls[top:bot-1])
		fill(l, b)
		ls[top] = l
	}
}

func (t *Terminal) insertLines(n int) {
	if t.cur.y < t.top || t.cur.y >= t.bot {
		return
	}
	t.scrollDown(t.cur.y, t.bot, n)
	t.cur.x, t.cur.wrapNext = 0, false
}

func (t *Terminal) deleteLines(n int) {
	if t.cur.y < t.top || t.cur.y >= t.bot {
		return
	}
	// Deleted lines never go to the history: only a region scrolling.
	ls := t.scr.lines
	n = min(n, t.bot-t.cur.y)
	b := t.blankCell()
	for range n {
		l := ls[t.cur.y]
		copy(ls[t.cur.y:t.bot-1], ls[t.cur.y+1:t.bot])
		fill(l, b)
		ls[t.bot-1] = l
	}
	t.cur.x, t.cur.wrapNext = 0, false
}

func (t *Terminal) insertCells(n int) {
	l := t.scr.lines[t.cur.y]
	x := t.cur.x
	n = min(n, t.cols-x)
	t.splitWide(l, x)
	copy(l[x+n:], l[x:t.cols-n])
	t.fixTail(l)
	fill(l[x:x+n], t.blankCell())
	t.cur.wrapNext = false
}

func (t *Terminal) deleteCells(n int) {
	l := t.scr.lines[t.cur.y]
	x := t.cur.x
	n = min(n, t.cols-x)
	t.splitWide(l, x)
	t.splitWide(l, x+n)
	copy(l[x:], l[x+n:])
	// The vacated tail is filled as is: its old cells were moved, not cut.
	fill(l[t.cols-n:], t.blankCell())
	t.cur.wrapNext = false
}

// fixTail clears a wide character left without its second half at the
// end of a line.
func (t *Terminal) fixTail(l []Cell) {
	if last := len(l) - 1; last >= 0 && l[last].Flags&FlagWide != 0 {
		l[last] = Cell{Style: l[last].Style}
	}
}

func (t *Terminal) eraseDisplay(mode int) {
	ls := t.scr.lines
	switch mode {
	case 0:
		t.blank(ls[t.cur.y], t.cur.x, t.cols)
		for y := t.cur.y + 1; y < t.rows; y++ {
			t.blank(ls[y], 0, t.cols)
		}
	case 1:
		for y := 0; y < t.cur.y; y++ {
			t.blank(ls[y], 0, t.cols)
		}
		t.blank(ls[t.cur.y], 0, t.cur.x+1)
	case 2:
		for y := range ls {
			t.blank(ls[y], 0, t.cols)
		}
	case 3:
		t.hist.clear()
	}
	t.cur.wrapNext = false
}

func (t *Terminal) eraseLine(mode int) {
	l := t.scr.lines[t.cur.y]
	switch mode {
	case 0:
		t.blank(l, t.cur.x, t.cols)
	case 1:
		t.blank(l, 0, t.cur.x+1)
	case 2:
		t.blank(l, 0, t.cols)
	}
	t.cur.wrapNext = false
}

// Printing.

// printASCII prints a run of printable ASCII: the cells of each line it
// covers are written at once.
func (t *Terminal) printASCII(run []byte) {
	if t.insert || t.g[t.gl] != csASCII {
		for _, b := range run {
			t.print(rune(b))
		}
		return
	}
	pen := t.cur.pen
	for len(run) > 0 {
		if t.cur.wrapNext {
			t.index()
			t.cur.x = 0
		}
		l := t.scr.lines[t.cur.y]
		x := t.cur.x
		n := min(len(run), t.cols-x)
		if !t.autowrap && n < len(run) {
			// Without autowrap what does not fit overwrites the last column.
			run = append(run[:n-1:n-1], run[len(run)-1])
		}
		t.splitWide(l, x)
		t.splitWide(l, x+n)
		seg := l[x : x+n]
		for i, b := range run[:n] {
			// A space is kept as a blank: the two are drawn alike, and a
			// snapshot sends blanks as spaces.
			ch := rune(b)
			if b == ' ' {
				ch = 0
			}
			seg[i] = Cell{Ch: ch, Style: pen}
		}
		t.lastChar = rune(run[n-1])
		run = run[n:]
		if x+n >= t.cols {
			t.cur.x, t.cur.wrapNext = t.cols-1, t.autowrap
			if !t.autowrap {
				return
			}
		} else {
			t.cur.x = x + n
		}
	}
}

var graphics = [...]rune{
	'\u00a0', '◆', '▒', '␉', '␌', '␍', '␊', '°', '±', '␤', '␋', '┘', '┐', '┌', '└', '┼',
	'⎺', '⎻', '─', '⎼', '⎽', '├', '┤', '┴', '┬', '│', '≤', '≥', 'π', '≠', '£', '·',
}

func runeWidth(r rune) int {
	if r < 0x300 {
		return 1
	}
	if unicode.In(r, unicode.Mn, unicode.Me, unicode.Cf) || (r >= 0xfe00 && r <= 0xfe0f) {
		return 0
	}
	switch width.LookupRune(r).Kind() {
	case width.EastAsianWide, width.EastAsianFullwidth:
		return 2
	}
	return 1
}

func (t *Terminal) print(r rune) {
	if r >= 0x80 && r < 0xa0 {
		// A UTF-8-encoded C1 control: neither shown nor acted on, as tmux.
		return
	}
	if r < 0x80 && t.g[t.gl] == csGraphics && r >= 0x5f && r <= 0x7e {
		r = graphics[r-0x5f]
	}
	w := runeWidth(r)
	if w == 0 {
		t.combine(r)
		return
	}
	if t.cur.wrapNext && t.autowrap {
		t.index()
		t.cur.x = 0
	}
	if w == 2 && t.cur.x == t.cols-1 {
		if t.cols < 2 {
			return
		}
		if t.autowrap {
			t.blank(t.scr.lines[t.cur.y], t.cur.x, t.cols)
			t.index()
			t.cur.x = 0
		} else {
			t.cur.x--
		}
	}
	if t.insert {
		t.insertCells(w)
	}
	l := t.scr.lines[t.cur.y]
	x := t.cur.x
	t.splitWide(l, x)
	t.splitWide(l, x+w)
	if w == 2 {
		l[x] = Cell{Ch: r, Style: t.cur.pen, Flags: FlagWide}
		l[x+1] = Cell{Style: t.cur.pen, Flags: FlagCont}
	} else if r == ' ' {
		l[x] = Cell{Style: t.cur.pen}
	} else {
		l[x] = Cell{Ch: r, Style: t.cur.pen}
	}
	t.lastChar = r
	if x+w >= t.cols {
		t.cur.x, t.cur.wrapNext = t.cols-1, t.autowrap
	} else {
		t.cur.x, t.cur.wrapNext = x+w, false
	}
}

// combine adds a zero-width character to the cell printed last.
func (t *Terminal) combine(r rune) {
	x, y := t.cur.x-1, t.cur.y
	if t.cur.wrapNext {
		x = t.cur.x
	}
	if x < 0 {
		return
	}
	l := t.scr.lines[y]
	if l[x].Flags&FlagCont != 0 && x > 0 {
		x--
	}
	c := &l[x]
	if c.Ch == 0 {
		return
	}
	// The key is built on the stack; a map lookup with string(key) does
	// not allocate, so only a new cluster does.
	var key [maxClusterLen]byte
	n := 0
	if c.Ch >= clusterBase {
		n = copy(key[:], t.Text(*c))
	} else {
		n = utf8.EncodeRune(key[:], c.Ch)
	}
	if n+utf8.RuneLen(r) > maxClusterLen {
		return
	}
	n += utf8.EncodeRune(key[n:], r)
	id, ok := t.clusterID[string(key[:n])]
	if !ok {
		if len(t.clusters) >= maxClusters {
			return
		}
		s := string(key[:n])
		id = clusterBase + rune(len(t.clusters))
		t.clusters = append(t.clusters, s)
		t.clusterID[s] = id
	}
	c.Ch = id
}

// SGR.

func (t *Terminal) sgr() {
	p := &t.cur.pen
	if t.np == 0 {
		*p = Style{}
		return
	}
	for i := 0; i < t.np; i++ {
		v := t.raw(i)
		switch {
		case v == 0:
			*p = Style{}
		case v == 1:
			p.Attr |= AttrBold
		case v == 2:
			p.Attr |= AttrFaint
		case v == 3:
			p.Attr |= AttrItalic
		case v == 4:
			style := 1
			if i+1 < t.np && t.sub[i+1] {
				i++
				style = t.raw(i)
			}
			p.Attr = p.Attr.withUnderline(style)
		case v == 5 || v == 6:
			p.Attr |= AttrBlink
		case v == 7:
			p.Attr |= AttrReverse
		case v == 8:
			p.Attr |= AttrInvisible
		case v == 9:
			p.Attr |= AttrStrike
		case v == 21:
			p.Attr = p.Attr.withUnderline(2)
		case v == 22:
			p.Attr &^= AttrBold | AttrFaint
		case v == 23:
			p.Attr &^= AttrItalic
		case v == 24:
			p.Attr = p.Attr.withUnderline(0)
		case v == 25:
			p.Attr &^= AttrBlink
		case v == 27:
			p.Attr &^= AttrReverse
		case v == 28:
			p.Attr &^= AttrInvisible
		case v == 29:
			p.Attr &^= AttrStrike
		case v >= 30 && v <= 37:
			p.Fg = Indexed(uint8(v - 30))
		case v == 38:
			var c Color
			c, i = t.extColor(i)
			if c != 0 {
				p.Fg = c
			}
		case v == 39:
			p.Fg = 0
		case v >= 40 && v <= 47:
			p.Bg = Indexed(uint8(v - 40))
		case v == 48:
			var c Color
			c, i = t.extColor(i)
			if c != 0 {
				p.Bg = c
			}
		case v == 49:
			p.Bg = 0
		case v == 58:
			_, i = t.extColor(i) // underline colour: not kept
		case v >= 90 && v <= 97:
			p.Fg = Indexed(uint8(v - 90 + 8))
		case v >= 100 && v <= 107:
			p.Bg = Indexed(uint8(v - 100 + 8))
		}
	}
}

// extColor reads the colour of a 38, 48 or 58 at i, in either the
// semicolon (38;5;n, 38;2;r;g;b) or the colon form (38:5:n, 38:2:[cs]:r:g:b),
// and returns the index of its last parameter.
func (t *Terminal) extColor(i int) (Color, int) {
	if i+1 < t.np && t.sub[i+1] {
		j := i + 1
		for j+1 < t.np && t.sub[j+1] {
			j++
		}
		args := t.params[i+1 : j+1]
		switch {
		case len(args) >= 2 && args[0] == 5:
			return Indexed(uint8(max(args[1], 0))), j
		case len(args) >= 4 && args[0] == 2:
			rgb := args[len(args)-3:]
			return RGB(uint8(max(rgb[0], 0)), uint8(max(rgb[1], 0)), uint8(max(rgb[2], 0))), j
		}
		return 0, j
	}
	switch t.raw(i + 1) {
	case 5:
		if i+2 < t.np {
			return Indexed(uint8(t.raw(i + 2))), i + 2
		}
	case 2:
		if i+4 < t.np {
			return RGB(uint8(t.raw(i+2)), uint8(t.raw(i+3)), uint8(t.raw(i+4))), i + 4
		}
	}
	return 0, t.np
}

// Resize changes the size: lines are cut or padded on the right; rows
// leave a screen from the top (to the history, on the main screen) while
// its cursor row (see resizeOffset) is below the new last row, else from
// the bottom. Saved cursors move with their screen's rows.
func (t *Terminal) Resize(rows, cols int) {
	rows, cols = clampSize(rows, cols)
	if rows == t.rows && cols == t.cols {
		return
	}
	if cols != t.cols {
		for _, s := range []*screen{&t.main, &t.alt} {
			for y, l := range s.lines {
				if cols < len(l) {
					l = l[:cols]
					t.fixTail(l)
				} else {
					l = append(l, make([]Cell, cols-len(l))...)
				}
				s.lines[y] = l
			}
		}
	}
	if rows < t.rows {
		for _, s := range []*screen{&t.main, &t.alt} {
			off := t.resizeOffset(s, rows)
			if s == &t.main {
				for _, l := range s.lines[:off] {
					_ = t.hist.push(l)
				}
			}
			s.lines = s.lines[off : off+rows]
			s.saved.y = max(s.saved.y-off, 0)
			if s == t.scr {
				t.cur.y -= off
			}
		}
	} else {
		for _, s := range []*screen{&t.main, &t.alt} {
			s.lines = append(s.lines, blankLines(rows-t.rows, cols)...)
		}
	}
	t.rows, t.cols = rows, cols
	t.top, t.bot = 0, rows
	t.cur.x, t.cur.y, t.cur.wrapNext = min(t.cur.x, cols-1), min(t.cur.y, rows-1), false
	for _, s := range []*screen{&t.main, &t.alt} {
		s.saved.x, s.saved.y = min(s.saved.x, cols-1), min(s.saved.y, rows-1)
	}
	tabs := t.tabs
	t.resetTabs()
	copy(t.tabs, tabs)
}

// resizeOffset is how many rows leave the top of s when it shrinks to
// rows: enough to keep its cursor's row. That is the cursor for the screen
// shown; for the main screen behind the alternate one, its saved cursor
// (1049 saves it), else its last non-blank row. The hidden alternate
// screen is cleared before it is shown again, so nothing leaves its top.
func (t *Terminal) resizeOffset(s *screen, rows int) int {
	y := 0
	switch {
	case s == t.scr:
		y = t.cur.y
	case s == &t.alt:
		return 0
	case s.saved.valid:
		y = s.saved.y
	default:
		for y = len(s.lines) - 1; y > 0; y-- {
			if !blankLine(s.lines[y]) {
				break
			}
		}
	}
	return max(y-(rows-1), 0)
}

func blankLine(l []Cell) bool {
	for _, c := range l {
		if c != (Cell{}) {
			return false
		}
	}
	return true
}

// history is a ring of at most limit lines. A line scrolled off the
// screen is moved in, not copied; the line it evicts is handed back to be
// the screen's new blank line. ED 3 empties it but keeps its lines as
// spare, handed back the same way until the ring is full again.
type history struct {
	lines    [][]Cell
	spare    [][]Cell
	start, n int
	limit    int
}

// push takes l and returns a line to reuse (nil if there is none).
func (h *history) push(l []Cell) []Cell {
	if h.limit == 0 {
		return l
	}
	if h.n < h.limit {
		h.lines = append(h.lines, l)
		h.n++
		if k := len(h.spare); k > 0 {
			s := h.spare[k-1]
			h.spare[k-1] = nil
			h.spare = h.spare[:k-1]
			return s
		}
		return nil
	}
	old := h.lines[h.start]
	h.lines[h.start] = l
	h.start = (h.start + 1) % h.limit
	return old
}

// line is history line i, 0 the oldest, without trailing blanks.
func (h *history) line(i int) []Cell {
	if i < 0 || i >= h.n {
		return nil
	}
	l := h.lines[(h.start+i)%len(h.lines)]
	end := len(l)
	for end > 0 && l[end-1] == (Cell{}) {
		end--
	}
	return l[:end]
}

func (h *history) clear() {
	// The ring's order does not matter to spare lines.
	h.spare = append(h.spare, h.lines...)
	clear(h.lines)
	h.lines, h.start, h.n = h.lines[:0], 0, 0
}
