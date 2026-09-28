package cli

import "testing"

// As the console's formatCores: lux's own processes idle at a few millicores.
func TestCores(t *testing.T) {
	for v, want := range map[float64]string{0: "0", 0.0012: "1.2m", 0.003: "3m", 0.25: "250m", 1: "1 core", 2.5: "2.5 cores"} {
		if got := cores(v); got != want {
			t.Errorf("cores(%v) = %q, want %q", v, got, want)
		}
	}
}
