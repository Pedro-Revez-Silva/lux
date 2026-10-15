package vterm

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
	"testing"
)

// screenText is the current screen as text, one line per row, trailing
// blanks trimmed.
func screenText(t *Terminal) string {
	var b strings.Builder
	for y := range t.rows {
		var l strings.Builder
		for x := range t.cols {
			l.WriteString(t.Text(t.Cell(x, y)))
		}
		b.WriteString(strings.TrimRight(l.String(), " "))
		b.WriteByte('\n')
	}
	return b.String()
}

func historyText(t *Terminal) []string {
	var out []string
	for i := range t.HistoryLen() {
		var l strings.Builder
		for _, c := range t.HistoryLine(i) {
			l.WriteString(t.Text(c))
		}
		out = append(out, strings.TrimRight(l.String(), " "))
	}
	return out
}

func feed(t *Terminal, s string) { t.Write([]byte(s)) }

// writer is t as an io.Writer, for fmt.Fprintf.
type writer struct{ t *Terminal }

func (w writer) Write(p []byte) (int, error) { w.t.Write(p); return len(p), nil }

// restored is a fresh terminal of t's size that has been sent t's
// snapshot.
func restored(t *Terminal, history int) *Terminal {
	r := New(t.rows, t.cols, history)
	r.Write(t.Snapshot(0))
	return r
}

// sameState fails unless a and b show the same cells (text and style),
// cursor, modes, title and scroll region, on both screens, and the same
// history.
func sameState(tb testing.TB, a, b *Terminal) {
	tb.Helper()
	if a.rows != b.rows || a.cols != b.cols {
		tb.Fatalf("size %dx%d, want %dx%d", b.rows, b.cols, a.rows, a.cols)
	}
	cmpScreen := func(name string, sa, sb *screen) {
		for y := range a.rows {
			for x := range a.cols {
				ca, cb := sa.lines[y][x], sb.lines[y][x]
				if a.Text(ca) != b.Text(cb) || ca.Style != cb.Style || ca.Flags != cb.Flags {
					tb.Fatalf("%s cell %d,%d: got %q %+v flags %d, want %q %+v flags %d", name, x, y,
						b.Text(cb), cb.Style, cb.Flags, a.Text(ca), ca.Style, ca.Flags)
				}
			}
		}
	}
	cmpScreen("main", &a.main, &b.main)
	if a.AltScreen() != b.AltScreen() {
		tb.Fatalf("alt screen %v, want %v", b.AltScreen(), a.AltScreen())
	}
	if a.AltScreen() {
		cmpScreen("alt", &a.alt, &b.alt)
	}
	ax, ay, av := a.Cursor()
	bx, by, bv := b.Cursor()
	if ax != bx || ay != by || av != bv || a.cur.wrapNext != b.cur.wrapNext {
		tb.Fatalf("cursor %d,%d visible %v wrap %v, want %d,%d visible %v wrap %v", bx, by, bv, b.cur.wrapNext, ax, ay, av, a.cur.wrapNext)
	}
	if a.cur.pen != b.cur.pen {
		tb.Fatalf("pen %+v, want %+v", b.cur.pen, a.cur.pen)
	}
	for _, n := range append([]int{6, 7, 25, 47, 1047, 1049}, replayModes...) {
		if a.Mode(n) != b.Mode(n) {
			tb.Fatalf("mode %d: %v, want %v", n, b.Mode(n), a.Mode(n))
		}
	}
	if a.keypad != b.keypad || a.insert != b.insert || a.newline != b.newline || a.cursorStyle != b.cursorStyle {
		tb.Fatalf("keypad/insert/newline/cursor style differ")
	}
	if a.Title() != b.Title() {
		tb.Fatalf("title %q, want %q", b.Title(), a.Title())
	}
	at, ab := a.ScrollRegion()
	bt, bb := b.ScrollRegion()
	if at != bt || ab != bb {
		tb.Fatalf("scroll region %d-%d, want %d-%d", bt, bb, at, ab)
	}
	ha, hb := historyText(a), historyText(b)
	if strings.Join(ha, "\n") != strings.Join(hb, "\n") {
		tb.Fatalf("history:\n%q\nwant:\n%q", hb, ha)
	}
}

func TestPlainScreen(t *testing.T) {
	term := New(5, 20, 100)
	feed(term, "hello\r\nworld\r\n\tx")
	if got, want := screenText(term), "hello\nworld\n        x\n\n\n"; got != want {
		t.Fatalf("screen %q, want %q", got, want)
	}
	if x, y, _ := term.Cursor(); x != 9 || y != 2 {
		t.Fatalf("cursor %d,%d", x, y)
	}
	r := restored(term, 100)
	if got := screenText(r); got != screenText(term) {
		t.Fatalf("restored screen %q", got)
	}
	sameState(t, term, r)
}

func TestScrollbackBounded(t *testing.T) {
	term := New(4, 10, 50)
	for i := range 1000 {
		fmt.Fprintf(writer{term}, "line %d\r\n", i)
	}
	if n := term.HistoryLen(); n != 50 {
		t.Fatalf("history %d lines, want 50", n)
	}
	h := historyText(term)
	// 1000 lines and a blank one being written: the screen holds 997–999
	// and the blank, the history the 50 before.
	if h[0] != "line 947" || h[49] != "line 996" {
		t.Fatalf("history from %q to %q", h[0], h[49])
	}
	r := restored(term, 50)
	sameState(t, term, r)
}

// A long run of output keeps memory bounded by the history limit: the
// ring's slots are reused, not grown.
func TestScrollbackMemoryBounded(t *testing.T) {
	term := New(24, 80, 2000)
	line := bytes.Repeat([]byte("x"), 79)
	line = append(line, "\r\n"...)
	for range 10000 {
		term.Write(line)
	}
	if len(term.hist.lines) != 2000 {
		t.Fatalf("history slots %d", len(term.hist.lines))
	}
	for i := range term.hist.lines {
		if c := cap(term.hist.lines[i]); c > 80 {
			t.Fatalf("history line %d holds %d cells", i, c)
		}
	}
	// With the ring full, scrolling reuses the lines it evicts.
	if n := testing.AllocsPerRun(10, func() { term.Write(line) }); n != 0 {
		t.Fatalf("%v allocations per scrolled line", n)
	}
}

// The emulator's own behaviour, against literal expectations (xterm's, and
// tmux's where they agree): what the round-trip tests cannot catch, since
// they compare the emulator with itself.
func TestBehaviour(t *testing.T) {
	type cur struct {
		x, y int
		wrap bool
	}
	for _, tc := range []struct {
		name       string
		rows, cols int
		in         string
		resize     [2]int // after in, when set
		screen     string
		cursor     cur
		history    []string
		check      func(*Terminal) string
	}{
		{name: "LF at the region's bottom scrolls the region only", rows: 4, cols: 10,
			in:     "r0\r\nr1\r\nr2\r\nr3\x1b[2;3r\x1b[3;1Ha\nb",
			screen: "r0\na2\n b\nr3\n", cursor: cur{2, 2, false}},
		{name: "a one-line region is refused", rows: 3, cols: 5,
			in:     "a\r\nb\r\nc\x1b[2;2r",
			screen: "a\nb\nc\n", cursor: cur{1, 2, false}},
		{name: "ASCII to the last column waits to wrap", rows: 3, cols: 5,
			in:     "abcde",
			screen: "abcde\n\n\n", cursor: cur{4, 0, true}},
		{name: "the next character after a pending wrap goes to the next line", rows: 3, cols: 5,
			in:     "abcdef",
			screen: "abcde\nf\n\n", cursor: cur{1, 1, false}},
		{name: "pending wrap for non-ASCII", rows: 3, cols: 5,
			in:     "abcd\u00e9\u00fc",
			screen: "abcd\u00e9\n\u00fc\n\n", cursor: cur{1, 1, false}},
		{name: "a wide character ending at the last column waits to wrap", rows: 2, cols: 5,
			in:     "abc宽",
			screen: "abc宽\n\n", cursor: cur{4, 0, true}},
		{name: "a wide character at the last column blanks it and wraps", rows: 2, cols: 5,
			in:     "abcde\x1b[1;5H宽",
			screen: "abcd\n宽\n", cursor: cur{2, 1, false}},
		{name: "RI at the region's top scrolls the region down", rows: 4, cols: 5,
			in:     "r0\r\nr1\r\nr2\r\nr3\x1b[2;3r\x1b[2;1H\x1bM",
			screen: "r0\n\nr1\nr3\n", cursor: cur{0, 1, false}},
		{name: "erase uses the pen's background (BCE)", rows: 2, cols: 5,
			in:     "abc\x1b[41m\x1b[1K\x1b[m",
			screen: "\n\n", cursor: cur{3, 0, false},
			check: func(term *Terminal) string {
				for x := range 4 {
					if c := term.Cell(x, 0); c.Style != (Style{Bg: Indexed(1)}) {
						return fmt.Sprintf("cell %d: %+v", x, c)
					}
				}
				if c := term.Cell(4, 0); c != (Cell{}) {
					return fmt.Sprintf("cell 4: %+v", c)
				}
				return ""
			}},
		{name: "DECSC saves the pen", rows: 2, cols: 5,
			in:     "\x1b[1;31m\x1b7\x1b[m\x1b[2;3H\x1b8x",
			screen: "x\n\n", cursor: cur{1, 0, false},
			check: func(term *Terminal) string {
				if c := term.Cell(0, 0); c.Style != (Style{Fg: Indexed(1), Attr: AttrBold}) {
					return fmt.Sprintf("cell %+v", c)
				}
				return ""
			}},
		{name: "DECRC in origin mode stays in the region", rows: 6, cols: 10,
			in:     "\x1b[2;5r\x1b[?6h\x1b[4;1H\x1b7\x1b[1;2r\x1b8",
			screen: "\n\n\n\n\n\n", cursor: cur{0, 1, false}},
		{name: "ED 3 clears the history", rows: 2, cols: 5,
			in:     "a\r\nb\r\nc\r\nd\x1b[3J",
			screen: "c\nd\n", cursor: cur{1, 1, false}},
		{name: "a region not at the top scrolls nothing into the history", rows: 4, cols: 5,
			in:     "r0\r\nr1\r\nr2\r\nr3\x1b[2;3r\x1b[3;1H\n\n",
			screen: "r0\n\n\nr3\n", cursor: cur{0, 2, false}},
		{name: "a region at the top scrolls into the history", rows: 4, cols: 5,
			in:     "r0\r\nr1\r\nr2\r\nr3\x1b[1;2r\x1b[2;1H\n",
			screen: "r1\n\nr2\nr3\n", cursor: cur{0, 1, false}, history: []string{"r0"}},
		{name: "the alternate screen has no history", rows: 2, cols: 5,
			in:     "m\x1b[?1049h1\r\n2\r\n3\r\n4",
			screen: "3\n4\n", cursor: cur{1, 1, false}},
		{name: "DL at row 0 does not go to the history", rows: 3, cols: 5,
			in:     "a\r\nb\r\nc\x1b[H\x1b[M",
			screen: "b\nc\n\n", cursor: cur{0, 0, false}},
		{name: "a shrink sends the rows above the cursor to the history", rows: 5, cols: 10,
			in:     "row 0\r\nrow 1\r\nrow 2\r\nrow 3\r\nrow 4\r\nlast",
			resize: [2]int{3, 6},
			screen: "row 3\nrow 4\nlast\n", cursor: cur{4, 2, false},
			history: []string{"row 0", "row 1", "row 2"}},
		{name: "a shrink with the cursor high cuts the bottom", rows: 5, cols: 10,
			in:     "top\r\nx\x1b[5;1Hbottom\x1b[1;1H",
			resize: [2]int{3, 10},
			screen: "top\nx\n\n", cursor: cur{0, 0, false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			term := New(tc.rows, tc.cols, 100)
			feed(term, tc.in)
			if tc.resize != [2]int{} {
				term.Resize(tc.resize[0], tc.resize[1])
			}
			if got := screenText(term); got != tc.screen {
				t.Errorf("screen %q, want %q", got, tc.screen)
			}
			x, y, _ := term.Cursor()
			if got := (cur{x, y, term.cur.wrapNext}); got != tc.cursor {
				t.Errorf("cursor %+v, want %+v", got, tc.cursor)
			}
			if got := historyText(term); strings.Join(got, "|") != strings.Join(tc.history, "|") {
				t.Errorf("history %q, want %q", got, tc.history)
			}
			if tc.check != nil {
				if msg := tc.check(term); msg != "" {
					t.Error(msg)
				}
			}
		})
	}
}

// UTF-8-encoded C1 controls (U+0080–U+009F) take no cell, as in tmux.
func TestC1ZeroWidth(t *testing.T) {
	term := New(3, 10, 10)
	feed(term, "ab\u0085cd\u009b31mX")
	if got := screenText(term); got != "abcd31mX\n\n\n" {
		t.Fatalf("screen %q", got)
	}
	if x, y, _ := term.Cursor(); x != 8 || y != 0 || term.cur.wrapNext {
		t.Fatalf("cursor %d,%d wrap %v, want 8,0", x, y, term.cur.wrapNext)
	}
	sameState(t, term, restored(term, 10))
}

// A combining mark whose cluster is already interned allocates nothing.
func TestCombiningKnownClusterNoAlloc(t *testing.T) {
	term := New(24, 80, 100)
	line := []byte(strings.Repeat("e\u0301a\u0308\u0301", 10) + "\r\n")
	term.Write(line)
	if n := testing.AllocsPerRun(50, func() { term.Write(line) }); n != 0 {
		t.Fatalf("%v allocations per line", n)
	}
	if got := term.Text(term.Cell(1, 22)); got != "a\u0308\u0301" {
		t.Fatalf("cluster %q", got)
	}
}

// ED 3 (what clear sends) empties the history but keeps its lines: a
// clear-and-redraw loop scrolls without allocating, and memory stays
// within the history limit.
func TestClearHistoryReusesLines(t *testing.T) {
	term := New(24, 80, 100)
	var redraw []byte
	redraw = append(redraw, "\x1b[H\x1b[2J\x1b[3J"...)
	for i := range 60 {
		redraw = fmt.Appendf(redraw, "status %d\r\n", i)
	}
	for range 5 {
		term.Write(redraw)
	}
	if n := testing.AllocsPerRun(20, func() { term.Write(redraw) }); n != 0 {
		t.Fatalf("%v allocations per redraw", n)
	}
	if n := len(term.hist.lines) + len(term.hist.spare); n > 100 {
		t.Fatalf("%d history lines held, over the limit of 100", n)
	}
	if got := term.HistoryLen(); got != 60-23 {
		t.Fatalf("history %d lines after a redraw, want 37", got)
	}
}

func TestAltScreen(t *testing.T) {
	for _, mode := range []int{1049, 1047, 47} {
		t.Run(fmt.Sprint(mode), func(t *testing.T) {
			term := New(6, 30, 100)
			feed(term, "\x1b]2;my app\x07")
			feed(term, "shell$ run\r\nmain screen text\r\n")
			fmt.Fprintf(writer{term}, "\x1b[?%dh", mode)
			feed(term, "\x1b[2J\x1b[H\x1b[1;37;44m full screen \x1b[m\x1b[3;5Hmenu\x1b[?1h\x1b=\x1b[?1002h\x1b[?1006h\x1b[?2004h\x1b[2;5r\x1b[4;2H")
			if !term.AltScreen() {
				t.Fatal("not on the alternate screen")
			}
			r := restored(term, 100)
			sameState(t, term, r)
			if !r.AltScreen() || !r.Mode(mode) || !r.Mode(1002) || !r.Mode(1006) || !r.Mode(2004) || !r.Mode(1) {
				t.Fatalf("modes not restored: alt %v", r.AltScreen())
			}
			if got := screenText(r); !strings.Contains(got, " full screen") || !strings.Contains(got, "menu") {
				t.Fatalf("alt screen %q", got)
			}
			if r.Title() != "my app" {
				t.Fatalf("title %q", r.Title())
			}

			// The program leaves the full screen: both get the same
			// bytes and must end up with the same main screen.
			leave := fmt.Sprintf("\x1b[r\x1b[?%dl\x1b[?1l\x1b>back\r\n", mode)
			feed(term, leave)
			feed(r, leave)
			sameState(t, term, r)
			if got := screenText(r); !strings.Contains(got, "main screen text") || strings.Contains(got, "menu") {
				t.Fatalf("main screen after leaving: %q", got)
			}
		})
	}
}

func TestAttributesColoursWideChars(t *testing.T) {
	term := New(6, 20, 100)
	feed(term, "\x1b[1;3;4mbiu\x1b[m \x1b[38;5;200;48;2;1;2;3mx\x1b[m \x1b[91;102mbr\x1b[m\r\n")
	feed(term, "\x1b[4:3mcurl\x1b[24m \x1b[7;9;2mrs\x1b[m\r\n")
	feed(term, "宽字符ok\r\n")
	feed(term, "e\u0301 a\u0308\r\n")
	feed(term, "\x1b[38:2::10:20:30mc\x1b[m")
	// A wide character that does not fit wraps.
	feed(term, "\x1b[5;20H宽")

	c := term.Cell(0, 0)
	if term.Text(c) != "b" || c.Style.Attr&(AttrBold|AttrItalic) != AttrBold|AttrItalic || c.Style.Attr.Underline() != 1 {
		t.Fatalf("cell 0,0 %+v", c)
	}
	if c := term.Cell(4, 0); c.Style.Fg != Indexed(200) || c.Style.Bg != RGB(1, 2, 3) {
		t.Fatalf("cell 4,0 %+v", c)
	}
	if c := term.Cell(6, 0); c.Style.Fg != Indexed(9) || c.Style.Bg != Indexed(10) {
		t.Fatalf("cell 6,0 %+v", c)
	}
	if c := term.Cell(0, 1); c.Style.Attr.Underline() != 3 {
		t.Fatalf("curly underline %+v", c)
	}
	if c := term.Cell(0, 2); term.Text(c) != "宽" || c.Flags != FlagWide || term.Cell(1, 2).Flags != FlagCont || term.Text(term.Cell(6, 2)) != "o" {
		t.Fatalf("wide chars: %q", screenText(term))
	}
	if term.Text(term.Cell(0, 3)) != "e\u0301" || term.Text(term.Cell(2, 3)) != "a\u0308" {
		t.Fatalf("combining: %q %q", term.Text(term.Cell(0, 3)), term.Text(term.Cell(2, 3)))
	}
	if c := term.Cell(0, 4); c.Style.Fg != RGB(10, 20, 30) {
		t.Fatalf("colon rgb %+v", c)
	}
	if term.Text(term.Cell(0, 5)) != "宽" {
		t.Fatalf("wrapped wide char: %q", screenText(term))
	}
	sameState(t, term, restored(term, 100))
}

func TestPendingWrapAndModes(t *testing.T) {
	term := New(3, 5, 10)
	feed(term, "abcde") // the cursor waits at the last column
	feed(term, "\x1b[?25l\x1b[5 q\x1b[4h\x1b(0")
	r := restored(term, 10)
	sameState(t, term, r)
	feed(term, "fq")
	feed(r, "fq")
	sameState(t, term, r)

	term = New(4, 10, 10)
	feed(term, "\x1b[?7l0123456789XY\x1b[2;3r\x1b[?6h\x1b[2;4H")
	r = restored(term, 10)
	sameState(t, term, r)
	feed(term, "z\nw")
	feed(r, "z\nw")
	sameState(t, term, r)
}

func TestSavedCursorRestored(t *testing.T) {
	term := New(5, 20, 10)
	feed(term, "\x1b[3;7H\x1b[31m\x1b7\x1b[m\x1b[1;1Htop")
	r := restored(term, 10)
	sameState(t, term, r)
	feed(term, "\x1b8x")
	feed(r, "\x1b8x")
	sameState(t, term, r)
}

func TestResize(t *testing.T) {
	term := New(5, 10, 100)
	for i := range 5 {
		fmt.Fprintf(writer{term}, "row %d\r\n", i)
	}
	feed(term, "last")
	term.Resize(3, 6)
	if r, c := term.Size(); r != 3 || c != 6 {
		t.Fatalf("size %dx%d", r, c)
	}
	if got := screenText(term); got != "row 3\nrow 4\nlast\n" {
		t.Fatalf("after shrinking %q", got)
	}
	if x, y, _ := term.Cursor(); x != 4 || y != 2 {
		t.Fatalf("cursor %d,%d", x, y)
	}
	sameState(t, term, restored(term, 100))
	term.Resize(8, 30)
	sameState(t, term, restored(term, 100))
	term.Resize(100000, 100000)
	if r, c := term.Size(); r != MaxRows || c != MaxCols {
		t.Fatalf("not clamped: %dx%d", r, c)
	}
}

// Shrinking while a full-screen program is up keeps the main screen's
// rows around its saved cursor, as tmux does: the rows above go to the
// history, and leaving the alternate screen puts the cursor back on its
// own line.
func TestResizeOnAltScreen(t *testing.T) {
	term := New(10, 20, 100)
	for i := range 9 {
		fmt.Fprintf(writer{term}, "out %d\r\n", i)
	}
	feed(term, "$ vim")
	feed(term, "\x1b[?1049h\x1b[H\x1b[2Jeditor")
	term.Resize(5, 20)
	if got := screenText(term); got != "editor\n\n\n\n\n" {
		t.Fatalf("alternate screen %q", got)
	}
	if x, y, _ := term.Cursor(); x != 6 || y != 0 {
		t.Fatalf("cursor on the alternate screen %d,%d", x, y)
	}
	feed(term, "\x1b[?1049l")
	if got := screenText(term); got != "out 5\nout 6\nout 7\nout 8\n$ vim\n" {
		t.Fatalf("main screen %q", got)
	}
	if got := strings.Join(historyText(term), "|"); got != "out 0|out 1|out 2|out 3|out 4" {
		t.Fatalf("history %q", got)
	}
	if x, y, _ := term.Cursor(); x != 5 || y != 4 {
		t.Fatalf("cursor %d,%d, want 5,4", x, y)
	}

	// Without a saved cursor (47), the main screen keeps its last
	// non-blank row.
	term = New(10, 20, 100)
	for i := range 7 {
		fmt.Fprintf(writer{term}, "out %d\r\n", i)
	}
	feed(term, "\x1b[?47h")
	term.Resize(4, 20)
	feed(term, "\x1b[?47l")
	if got := screenText(term); got != "out 3\nout 4\nout 5\nout 6\n" {
		t.Fatalf("main screen (47) %q", got)
	}
	if got := strings.Join(historyText(term), "|"); got != "out 0|out 1|out 2" {
		t.Fatalf("history (47) %q", got)
	}

	// 1049 with the saved cursor (row 3) above the last non-blank row (9):
	// the main screen keeps the rows around the saved cursor.
	term = New(10, 20, 100)
	for i := range 10 {
		fmt.Fprintf(writer{term}, "\x1b[%d;1Hout %d", i+1, i)
	}
	feed(term, "\x1b[4;1H\x1b[?1049h")
	term.Resize(5, 20)
	feed(term, "\x1b[?1049l")
	if got := screenText(term); got != "out 0\nout 1\nout 2\nout 3\nout 4\n" {
		t.Fatalf("main screen (saved cursor) %q", got)
	}
	if x, y, _ := term.Cursor(); x != 0 || y != 3 {
		t.Fatalf("cursor (saved cursor) %d,%d, want 0,3", x, y)
	}
	if n := term.HistoryLen(); n != 0 {
		t.Fatalf("history (saved cursor) %d lines, want none", n)
	}
}

// A DECSC-saved cursor moves with the rows a shrink takes off the top.
func TestResizeShiftsSavedCursor(t *testing.T) {
	term := New(10, 20, 100)
	feed(term, "\x1b[7;3H\x1b7")
	for i := range 10 {
		fmt.Fprintf(writer{term}, "\x1b[%d;1Hrow %d", i+1, i)
	}
	term.Resize(6, 20) // the cursor is on row 9: rows 0–3 leave
	feed(term, "\x1b8X")
	if got := screenText(term); got != "row 4\nrow 5\nroX 6\nrow 7\nrow 8\nrow 9\n" {
		t.Fatalf("screen %q", got)
	}
	if x, y, _ := term.Cursor(); x != 3 || y != 2 {
		t.Fatalf("cursor %d,%d, want 3,2", x, y)
	}
}

// roundTripPieces are what the round-trip tests build output from.
// Not here: REP (CSI b), which repeats the last character printed; a
// snapshot does not carry that character. New pieces go at the end, so
// the committed fuzz corpus keeps its meaning.
var roundTripPieces = []string{
	"a", "xy", "宽", "e\u0301", "\r", "\n", "\t", "\b", "\x1b[", "31m", "\x1b[m", "\x1b[1;4;7m",
	"\x1b[2J", "\x1b[K", "\x1b[1K", "\x1b[3;4H", "\x1b[5A", "\x1b[2B", "\x1b[3C", "\x1b[D",
	"\x1b[2L", "\x1b[M", "\x1b[2@", "\x1b[P", "\x1b[X", "\x1b[S", "\x1b[T", "\x1b[2;5r", "\x1b[r",
	"\x1b[?1049h", "\x1b[?1049l", "\x1b[?47h", "\x1b[?47l", "\x1b[?1h", "\x1b[?2004h", "\x1b[?7l", "\x1b[?7h",
	"\x1b[?6h", "\x1b[?6l", "\x1b[4h", "\x1b[4l", "\x1b7", "\x1b8", "\x1bM", "\x1bD", "\x1bE",
	"\x1b]0;title\x07", "\x1b]2;t2\x1b\\", "\x1bP1$r\x1b\\", "\x1b(0", "\x1b(B", "lqqk", "\x0e", "\x0f",
	"\x1b[38;5;123m", "\x1b[48;2;9;8;7m", "\x1b[?25l", "\x1b[?25h", "\x1bc", "\x1b[3J", "\x1b[!p",
	"\xff", "\xe5\xae", "\x1b[3g", "\x1bH", "\x1b[Z", "\x1b[20h", "\x1b[20l",
	// 68:
	"\x1b)0", "\x1b=", "\x1b>", "\x1b[4 q", "\x1b[?1047h", "\x1b[?1047l", "\x1b[?1048h", "\x1b[?1048l",
	"\x1b[?1000h", "\x1b[?1006h", "\x1b[?1002h", "\x1b[?1000l", "\x1b[999C宽", "\x1b[5;2r", "\x1b#8",
	"\x1b[s", "\x1b[u", "\x1b[5G", "\x1b[2d", "\x1b[2I", "\x1b[E", "\x1b[F", "\u0085", "\x1b[41m",
}

// roundTrip feeds the pieces to a rows×cols terminal in uneven writes; at
// byte cut (maybe mid-sequence) it resizes to resize (when set), as attach
// does, and takes a snapshot into a fresh terminal. Both are then fed the
// rest, and must end in the same state.
func roundTrip(t *testing.T, rows, cols int, idx []byte, cut int, resize [2]int) {
	t.Helper()
	var all []byte
	for _, i := range idx {
		all = append(all, roundTripPieces[int(i)%len(roundTripPieces)]...)
	}
	cut = min(cut, len(all))
	term := New(rows, cols, 20)
	var r *Terminal
	take := func() {
		if resize != [2]int{} {
			term.Resize(resize[0], resize[1])
		}
		r = restored(term, 20)
	}
	step := 1
	for done := 0; done < len(all); {
		if r == nil && done == cut {
			take()
		}
		step = step*5%17 + 1
		n := min(step, len(all)-done)
		if r == nil && done < cut && done+n > cut {
			n = cut - done
		}
		term.Write(all[done : done+n])
		if r != nil {
			r.Write(all[done : done+n])
		}
		done += n
	}
	if r == nil {
		take()
	}
	// Both end the same way, so that a sequence left open does not hide a
	// difference.
	term.Write([]byte("\x18\x1b\\ "))
	r.Write([]byte("\x18\x1b\\ "))
	defer func() {
		if t.Failed() {
			t.Logf("%dx%d, cut %d, resize %v, input %q", rows, cols, cut, resize, all)
		}
	}()
	sameState(t, term, r)
}

// FuzzSnapshotRoundTrip: data is rows, cols, the cut (2 bytes, big
// endian), the resize (rows, cols; 0 none), then piece indices.
//
//	go test -fuzz FuzzSnapshotRoundTrip ./vterm/
func FuzzSnapshotRoundTrip(f *testing.F) {
	f.Add([]byte{3, 8, 0, 4, 0, 0, 0, 1, 2})
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) < 6 {
			return
		}
		rows, cols := 2+int(data[0])%10, 2+int(data[1])%30
		cut := int(data[2])<<8 | int(data[3])
		var resize [2]int
		if data[4] != 0 && data[5] != 0 {
			resize = [2]int{1 + int(data[4])%12, 1 + int(data[5])%32}
		}
		roundTrip(t, rows, cols, data[6:], cut, resize)
	})
}

// Random output, including escape sequences cut anywhere, never panics,
// and its snapshot (after a resize, sometimes) always reproduces it: seeds
// 1..20, or VTERM_SEED.
func TestRandomOutputRoundTrips(t *testing.T) {
	seeds := []uint64{}
	if s, err := strconv.ParseUint(os.Getenv("VTERM_SEED"), 10, 64); err == nil {
		seeds = append(seeds, s)
	} else {
		for s := range uint64(20) {
			seeds = append(seeds, s+1)
		}
	}
	for _, seed := range seeds {
		rng := rand.New(rand.NewPCG(seed, 2))
		for iter := range 300 {
			rows, cols := 2+rng.IntN(10), 2+rng.IntN(30)
			idx := make([]byte, 200)
			for i := range idx {
				idx[i] = byte(rng.IntN(len(roundTripPieces)))
			}
			var resize [2]int
			if rng.IntN(3) == 0 {
				resize = [2]int{1 + rng.IntN(12), 1 + rng.IntN(32)}
			}
			roundTrip(t, rows, cols, idx, rng.IntN(1500), resize)
			if t.Failed() {
				t.Fatalf("seed %d, iteration %d", seed, iter)
			}
		}
	}
}

// A snapshot over its byte limit leaves out the oldest history lines,
// whole, and still reproduces the screen.
func TestSnapshotLimit(t *testing.T) {
	term := New(4, 20, 1000)
	for i := range 500 {
		fmt.Fprintf(writer{term}, "\x1b[3%dmline %d\x1b[m\r\n", i%8, i)
	}
	full := term.Snapshot(0)
	limited := term.Snapshot(len(full) / 2)
	if len(limited) > len(full)/2 {
		t.Fatalf("%d bytes over the limit %d", len(limited), len(full)/2)
	}
	r := New(4, 20, 1000)
	r.Write(limited)
	if got := screenText(r); got != screenText(term) {
		t.Fatalf("screen %q", got)
	}
	h, all := historyText(r), historyText(term)
	if len(h) == 0 || len(h) >= len(all) || h[len(h)-1] != all[len(all)-1] || h[0] != all[len(all)-len(h)] {
		t.Fatalf("history kept %d of %d lines, from %q", len(h), len(all), h[0])
	}
	// As much history as fits: the next older line would not have.
	older := term.appendLine(nil, term.HistoryLine(term.HistoryLen()-len(h)-1), true)
	if len(limited)+len(older)+2 <= len(full)/2 {
		t.Fatalf("%d bytes of %d: the next older line (%d bytes) would have fit", len(limited), len(full)/2, len(older)+2)
	}
	// A limit that fits the screen and no history line keeps no history,
	// never less screen; one below the screen itself gives no snapshot.
	screenOnly := len(restored(term, 0).Snapshot(0))
	r = New(4, 20, 1000)
	r.Write(term.Snapshot(screenOnly))
	if r.HistoryLen() != 0 || screenText(r) != screenText(term) {
		t.Fatalf("screen-sized limit: history %d, screen %q", r.HistoryLen(), screenText(r))
	}
	if s := term.Snapshot(screenOnly - 1); s != nil {
		t.Fatalf("a %d-byte snapshot over a limit of %d", len(s), screenOnly-1)
	}
}

// A limited snapshot's history is the newest lines as one unbroken run:
// a long line that does not fit ends it, older short ones are not kept.
func TestSnapshotHistoryIsNewestContiguous(t *testing.T) {
	term := New(4, 60, 1000)
	for i := range 300 {
		if i%3 == 0 {
			fmt.Fprintf(writer{term}, "\x1b[38;2;1;2;3m%s %d\x1b[m\r\n", strings.Repeat("L", 50), i)
		} else {
			fmt.Fprintf(writer{term}, "s%d\r\n", i)
		}
	}
	all := historyText(term)
	full := len(term.Snapshot(0))
	for limit := full / 3; limit < full; limit += 7 {
		r := New(4, 60, 1000)
		r.Write(term.Snapshot(limit))
		h := historyText(r)
		if strings.Join(h, "|") != strings.Join(all[len(all)-len(h):], "|") {
			t.Fatalf("limit %d: not the newest %d lines, contiguous", limit, len(h))
		}
	}
}

// With the alternate screen shown, a limit the screens do not fit gives
// no snapshot, never one with part of the alternate screen.
func TestSnapshotLimitOnAltScreen(t *testing.T) {
	term := New(10, 80, 0)
	feed(term, "main\r\n\x1b[?1049h")
	for y := range 10 {
		fmt.Fprintf(writer{term}, "\x1b[%dH", y+1)
		for x := range 79 {
			fmt.Fprintf(writer{term}, "\x1b[38;2;%d;%d;1mx", x, y)
		}
	}
	full := len(term.Snapshot(0))
	none := 0
	for limit := full / 2; limit < full; limit += 13 {
		s := term.Snapshot(limit)
		if s == nil {
			none++
			continue
		}
		r := New(10, 80, 0)
		r.Write(s)
		sameState(t, term, r)
	}
	if none == 0 {
		t.Fatal("every limit fitted: nothing tested")
	}
}

// BenchmarkSnapshot is a full terminal's snapshot: what attach holds the
// terminal's lock for.
func BenchmarkSnapshot(b *testing.B) {
	term := New(24, 80, 2000)
	line := []byte("\x1b[1;32m" + strings.Repeat("colour", 6) + "\x1b[m plain text to fill the rest of the line out.\r\n")
	for range 3000 {
		term.Write(line)
	}
	var n int
	for b.Loop() {
		n = len(term.Snapshot(0))
	}
	b.ReportMetric(float64(n), "bytes")
}

func BenchmarkWrite(b *testing.B) {
	for _, tc := range []struct {
		name string
		line string
	}{
		{"yes", "y\r\n"},
		{"text", "the quick brown fox jumps over the lazy dog \x1b[31mred\x1b[0m and some more text to fill 80 cols\r\n"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			chunk := bytes.Repeat([]byte(tc.line), 32<<10/len(tc.line))
			term := New(24, 80, 2000)
			b.SetBytes(int64(len(chunk)))
			for b.Loop() {
				term.Write(chunk)
			}
		})
	}
}
