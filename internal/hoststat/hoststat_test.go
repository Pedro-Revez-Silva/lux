package hoststat

import (
	"os"
	"runtime/debug"
	"testing"
)

func TestParseStat(t *testing.T) {
	// 100 USER_HZ per second; idle (4th), iowait (5th) and guest (9th, 10th)
	// are not busy time.
	got := parseStat("cpu  100 200 300 9000 500 10 20 30 700 800\ncpu0 1 2 3 4\ncpu1 1 2 3 4\nintr 1 2\nctxt 5\n")
	if got.Seconds != 6.6 || got.Cores != 2 {
		t.Fatalf("got %+v", got)
	}
}

func TestParseMeminfo(t *testing.T) {
	got := parseMeminfo("MemTotal:       16000 kB\nMemFree:  1000 kB\nMemAvailable:    4000 kB\n")
	if got.Total != 16000*1024 || got.Used != 12000*1024 {
		t.Fatalf("got %+v", got)
	}
}

func TestReadDisk(t *testing.T) {
	d, err := ReadDisk(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if d.Total <= 0 || d.Used < 0 || d.Free < 0 || d.Used+d.Free > d.Total {
		t.Fatalf("got %+v", d)
	}
	if _, err := ReadDisk("/nonexistent/" + t.Name()); !os.IsNotExist(err) {
		t.Fatalf("missing dir: %v", err)
	}
}

func TestParseStatus(t *testing.T) {
	kb := parseKB("Name:\tluxd\nVmHWM:\t    9000 kB\nVmRSS:\t    7000 kB\nThreads:\t12\n", "VmRSS:", "VmHWM:")
	if kb["VmRSS:"] != 7000*1024 || kb["VmHWM:"] != 9000*1024 || len(kb) != 2 {
		t.Fatalf("got %v", kb)
	}
}

// touch makes RSS spike by 64 MiB and drop again.
func touch() {
	buf := make([]byte, 64<<20)
	for i := 0; i < len(buf); i += 4096 {
		buf[i] = 1
	}
	buf = nil
	debug.FreeOSMemory()
}

// Reads in one process: the same start, a CPU counter that does not go
// back, plausible levels. A spike shows in the next read's peak, and stays
// in every read until one is Stored (a lost sample's spike carries over);
// after that it is gone.
func TestProcessSampler(t *testing.T) {
	var s ProcessSampler
	a, err := s.Read()
	if err != nil {
		t.Fatal(err)
	}
	s.Stored()
	touch()
	b, err := s.Read()
	if err != nil {
		t.Fatal(err)
	}
	if a.Started.IsZero() || !a.Started.Equal(b.Started) || b.CPUSeconds < a.CPUSeconds {
		t.Fatalf("%+v then %+v", a, b)
	}
	if b.RSSBytes <= 0 || b.HeapBytes <= 0 || b.Goroutines < 2 {
		t.Fatalf("got %+v", b)
	}
	if b.PeakRSSBytes == nil {
		t.Skip("this kernel or sandbox does not reset the peak")
	}
	spike := *b.PeakRSSBytes
	if spike < b.RSSBytes || spike < a.RSSBytes+48<<20 {
		t.Fatalf("peak %d, rss before %d, now %d", spike, a.RSSBytes, b.RSSBytes)
	}
	if c, _ := s.Read(); *c.PeakRSSBytes < spike {
		t.Fatalf("not stored, yet the peak fell from %d to %d", spike, *c.PeakRSSBytes)
	}
	s.Stored()
	if c, _ := s.Read(); *c.PeakRSSBytes >= spike {
		t.Fatalf("stored, yet the peak is still %d", *c.PeakRSSBytes)
	}
}
