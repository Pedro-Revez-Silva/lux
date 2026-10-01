package adapter

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/marcioapm/lux/internal/proto"
)

// retained is the input payload bytes the ledger holds.
func (l *inputLedger) retained() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, st := range l.m {
		n += len(st.in.Text) + len(st.in.Raw)
	}
	return n
}

func heap() uint64 {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

// 1,024 inputs of 64 KiB, each accepted then consumed, failed, or
// accepted without a receipt: once settled the ledger keeps none of their
// text, only what dedupes a repeated report.
func TestLedgerDropsSettledPayloads(t *testing.T) {
	const n, size = 1024, 64 << 10
	var l inputLedger
	sink := &inputSink{}
	before := heap()
	for i := range n {
		in := proto.Input{RequestID: fmt.Sprintf("r%d", i), Text: strings.Repeat("x", size)}
		l.track(in)
		switch i % 3 {
		case 0:
			l.accept(sink, in, Delivery{Lands: LandsNextStep, Receipt: true}, "")
			l.consume(sink, in.RequestID)
		case 1:
			l.accept(sink, in, Delivery{Lands: LandsNextStep, Receipt: true}, "")
			l.fail(sink, in, errors.New("dropped"))
		case 2:
			l.accept(sink, in, Delivery{Lands: LandsNextStep}, "")
		}
	}
	if b := l.retained(); b != 0 {
		t.Fatalf("the ledger retains %d payload bytes after %d settled inputs", b, n)
	}
	if grew := int64(heap()) - int64(before); grew > n*size/8 {
		t.Fatalf("heap grew %d bytes for %d settled inputs of %d bytes", grew, n, size)
	}
	// A repeated report of a settled input is still not heard twice.
	lines := len(sink.lines())
	l.consume(sink, "r0")
	l.accept(sink, proto.Input{RequestID: "r0"}, Delivery{Lands: LandsNextStep, Receipt: true}, "")
	l.fail(sink, proto.Input{RequestID: "r1"}, errors.New("again"))
	if len(sink.lines()) != lines {
		t.Fatalf("a settled input reported again: %q", sink.lines()[lines:])
	}
	if len(l.open) != 0 {
		t.Fatalf("%d inputs still open", len(l.open))
	}
}

// An input the agent may still read keeps its text (it may be sent
// again); closing the ledger fails it and drops the text.
func TestLedgerCloseFailsWhatIsUnread(t *testing.T) {
	var l inputLedger
	sink := &inputSink{}
	in := proto.Input{RequestID: "s", Text: "keep"}
	l.track(in)
	l.accept(sink, in, Delivery{Lands: LandsNextStep, Receipt: true}, "t")
	if got := l.unread("t"); len(got) != 1 || got[0].Text != "keep" {
		t.Fatalf("unread %v", got)
	}
	l.close(sink, errors.New("gone"))
	l.accept(sink, proto.Input{RequestID: "late", Text: "x"}, Delivery{Lands: LandsNextStep, Receipt: true}, "")
	want := "accepted s next_step receipt=true\nfailed s: gone\naccepted late next_step receipt=true\nfailed late: gone"
	if got := strings.Join(sink.lines(), "\n"); got != want || l.retained() != 0 {
		t.Fatalf("got\n%s\nretained %d", got, l.retained())
	}
}
