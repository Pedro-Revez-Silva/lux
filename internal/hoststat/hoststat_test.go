package hoststat

import (
	"os"
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
