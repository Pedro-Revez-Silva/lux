package proto

import "testing"

// BlobParts and the part boundaries luxd and the runner share: an exact
// multiple of BlobPartSize has no empty trailing part.
func TestBlobParts(t *testing.T) {
	const p = BlobPartSize
	for _, c := range []struct {
		size int64
		want int
	}{
		{0, 1}, {1, 1}, {p - 1, 1}, {p, 1}, {p + 1, 2}, {2 * p, 2}, {2*p + 1, 3},
		{7_300_000_000, 109}, // 7.3 GB
		{114*p + 1, 115},
		{115 * p, 115},
	} {
		if got := BlobParts(c.size); got != c.want {
			t.Errorf("BlobParts(%d) = %d, want %d", c.size, got, c.want)
		}
	}
}
