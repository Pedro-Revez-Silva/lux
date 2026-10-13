package sizing

import (
	"errors"
	"math"
	"math/bits"
	"reflect"
	"strings"
	"testing"

	"github.com/marcioapm/lux/internal/spec"
)

func some[T any](v T) Optional[T] { return Optional[T]{true, v} }

// gib converts a whole or half GiB count to bytes exactly.
func gib(n float64) spec.Bytes { return spec.Bytes(n * (1 << 30)) }

func shape(cpu float64) Shape {
	return Shape{Dimensions: Dimensions{cpu, gib(cpu * 4), gib(cpu * 20)}, Family: "m", Arch: "arm64", MinCPUs: 0.5, DefaultCPUs: 2}
}

func model(t *testing.T, cpu float64) *Model {
	t.Helper()
	return mustNew(t, shape(cpu))
}

func names(m *Model) []string {
	var out []string
	for _, e := range m.List() {
		out = append(out, e.Name)
	}
	return out
}

func TestLadders(t *testing.T) {
	for _, cpu := range []float64{4, 8} {
		m := model(t, cpu)
		entries := m.List()
		for i, e := range entries {
			wantCPU := math.Ldexp(0.5, i)
			if e.CPUs != wantCPU || e.Memory != gib(wantCPU*4) || e.Disk != gib(wantCPU*20) || e.Fraction != wantCPU/cpu {
				t.Fatal(e)
			}
			parsed, err := m.Parse(e.Name)
			if err != nil || parsed != e {
				t.Fatalf("parse: %+v %v", parsed, err)
			}
			resolved, err := m.Resolve(Request{Size: some(e.Name)})
			if err != nil || resolved.Resolved != e {
				t.Fatalf("explicit: %+v %v", resolved, err)
			}
		}
		entries[0].Name = "changed"
		if m.List()[0].Name != "m-05-arm64" {
			t.Fatal("List leaked storage")
		}
	}
	if got, want := names(model(t, 8)), []string{"m-05-arm64", "m-1-arm64", "m-2-arm64", "m-4-arm64", "m-8-arm64"}; !reflect.DeepEqual(got, want) {
		t.Fatal(got)
	}
	if got, want := names(model(t, 4)), []string{"m-05-arm64", "m-1-arm64", "m-2-arm64", "m-4-arm64"}; !reflect.DeepEqual(got, want) {
		t.Fatal(got)
	}
}

func TestShapeValidation(t *testing.T) {
	cases := []struct {
		name   string
		change func(*Shape)
		want   string
	}{
		{"zero CPU", func(s *Shape) { s.CPUs = 0 }, "nominal CPUs 0 is not a power of two"},
		{"negative CPU", func(s *Shape) { s.CPUs = -8 }, "nominal CPUs -8 is not a power of two"},
		{"NaN CPU", func(s *Shape) { s.CPUs = math.NaN() }, "nominal CPUs NaN is not a power of two"},
		{"Inf CPU", func(s *Shape) { s.CPUs = math.Inf(1) }, "nominal CPUs +Inf is not a power of two"},
		{"max float CPU", func(s *Shape) { s.CPUs = math.MaxFloat64 }, "is not a power of two"},
		{"non power CPU, divisible bytes", func(s *Shape) { s.CPUs, s.Memory, s.Disk = 6, gib(24), gib(120) }, "nominal CPUs 6 is not a power of two"},
		{"next CPU", func(s *Shape) { s.CPUs = math.Nextafter(8, 9) }, "nominal CPUs 8.000000000000002 is not a power of two"},
		{"above cap", func(s *Shape) { s.CPUs, s.Memory, s.Disk = 1<<54, 1<<62, 1<<62 }, "exceeds the 2^53 cap"},
		{"small min", func(s *Shape) { s.MinCPUs = 0.25 }, "minimum CPUs 0.25 is below 0.5"},
		{"non power min", func(s *Shape) { s.MinCPUs = 0.75 }, "minimum CPUs 0.75 is not a power of two"},
		{"NaN min", func(s *Shape) { s.MinCPUs = math.NaN() }, "minimum CPUs NaN is not a power of two"},
		{"zero min", func(s *Shape) { s.MinCPUs = 0 }, "minimum CPUs 0 is not a power of two"},
		{"Inf min", func(s *Shape) { s.MinCPUs = math.Inf(1) }, "minimum CPUs +Inf is not a power of two"},
		{"min above nominal", func(s *Shape) { s.MinCPUs, s.DefaultCPUs = 16, 16 }, "nominal CPUs 8 is below minimum 16"},
		{"zero memory", func(s *Shape) { s.Memory = 0 }, "positive bytes"},
		{"zero disk", func(s *Shape) { s.Disk = 0 }, "positive bytes"},
		{"negative even memory", func(s *Shape) { s.Memory = -gib(32) }, "positive bytes"},
		{"negative even disk", func(s *Shape) { s.Disk = -gib(160) }, "positive bytes"},
		{"memory remainder", func(s *Shape) { s.Memory++ }, "not divisible by 16"},
		{"disk remainder", func(s *Shape) { s.Disk += 8 }, "not divisible by 16"},
		{"too few bytes", func(s *Shape) { s.Memory = 8 }, "not divisible by 16"},
		{"max int remainder", func(s *Shape) { s.Disk = math.MaxInt64 }, "not divisible by 16"},
		{"default absent", func(s *Shape) { s.DefaultCPUs = 3 }, "default CPUs 3 is not a ladder entry"},
		{"default above ladder", func(s *Shape) { s.DefaultCPUs = 16 }, "default CPUs 16 is not a ladder entry"},
		{"default below ladder", func(s *Shape) { s.DefaultCPUs = 0.25 }, "default CPUs 0.25 is not a ladder entry"},
		{"default zero", func(s *Shape) { s.DefaultCPUs = 0 }, "default CPUs 0 is not a ladder entry"},
		{"default NaN", func(s *Shape) { s.DefaultCPUs = math.NaN() }, "default CPUs NaN is not a ladder entry"},
		{"bad arch", func(s *Shape) { s.Arch = "x86_64" }, `invalid architecture "x86_64"`},
		{"empty family", func(s *Shape) { s.Family = "" }, `invalid family ""`},
		{"bad family", func(s *Shape) { s.Family = "M!" }, `invalid family "M!"`},
		// The longest name is <family>-05-arm64, 9 characters plus the family.
		{"64-character name", func(s *Shape) { s.Family = strings.Repeat("m", 55) }, "size name exceeds 63 characters"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := shape(8)
			c.change(&s)
			_, err := New(s)
			if !errors.Is(err, ErrInvalidShape) || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want %q, got %v", c.want, err)
			}
		})
	}

	accepted := []struct {
		name   string
		change func(*Shape)
	}{
		{"near max int bytes", func(s *Shape) { s.Memory, s.Disk = math.MaxInt64-15, math.MaxInt64-15 }},
		{"at cap", func(s *Shape) { s.CPUs, s.Memory, s.Disk = 1<<53, 1<<62, 1<<62 }},
		{"63-character name", func(s *Shape) { s.Family = strings.Repeat("m", 54) }},
		{"custom family amd64", func(s *Shape) { s.Family, s.Arch, s.MinCPUs, s.DefaultCPUs = "custom-compute", "amd64", 1, 4 }},
		{"half CPU host", func(s *Shape) { *s = shape(0.5); s.DefaultCPUs = 0.5 }},
	}
	for _, c := range accepted {
		t.Run(c.name, func(t *testing.T) {
			s := shape(8)
			c.change(&s)
			m, err := New(s)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range m.List() {
				if e.Memory <= 0 || e.Memory != s.Memory/spec.Bytes(1/e.Fraction) || e.Disk != s.Disk/spec.Bytes(1/e.Fraction) || len(e.Name) > MaxName {
					t.Fatal(e)
				}
			}
		})
	}
	s := shape(8)
	s.Family, s.Arch, s.MinCPUs, s.DefaultCPUs = "custom-compute", "amd64", 1, 4
	if got := names(mustNew(t, s)); got[0] != "custom-compute-1-amd64" {
		t.Fatal(got)
	}
	s = shape(8)
	s.Family = strings.Repeat("m", 54)
	if got := names(mustNew(t, s)); len(got[0]) != MaxName {
		t.Fatal(got)
	}
}

func mustNew(t *testing.T, s Shape) *Model {
	t.Helper()
	m, err := New(s)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestDecimalDisk(t *testing.T) {
	s := shape(8)
	s.Disk = 160e9
	m := mustNew(t, s)
	for _, e := range m.List() {
		if e.Disk != spec.Bytes(e.CPUs*20e9) {
			t.Fatal(e)
		}
	}
}

func TestZeroModel(t *testing.T) {
	var m Model
	if got := m.List(); len(got) != 0 {
		t.Fatal(got)
	}
	if _, err := m.Parse("m-2-arm64"); !errors.Is(err, ErrInvalidShape) {
		t.Fatal(err)
	}
	for _, r := range []Request{{}, {CPUs: some(1.0)}, {Size: some("m-2-arm64")}} {
		if _, err := m.Resolve(r); !errors.Is(err, ErrInvalidShape) {
			t.Fatal(r, err)
		}
	}
}

func TestCanonicalNames(t *testing.T) {
	m := model(t, 8)
	for _, name := range []string{"", "m4", "lux-m-2-arm64", "m-0.5-arm64", "m-005-arm64", "m-02-arm64", "m-2.0-arm64", "m-2e0-arm64", "m-+2-arm64", "m-3-arm64", "m-16-arm64", "m-2-amd64", "c-2-arm64", "M-2-arm64", " m-2-arm64", "m-2-arm64\n", "m--2-arm64"} {
		if _, err := m.Parse(name); !errors.Is(err, ErrUnknownSize) {
			t.Fatalf("parse %q: %v", name, err)
		}
		if _, err := m.Resolve(Request{Size: some(name)}); !errors.Is(err, ErrUnknownSize) {
			t.Fatalf("resolve %q: %v", name, err)
		}
	}
}

func TestResolution(t *testing.T) {
	m := model(t, 8)
	cases := []struct {
		name string
		r    Request
		want string
	}{
		{"default", Request{}, "m-2-arm64"},
		{"cpu only", Request{CPUs: some(1.0)}, "m-1-arm64"},
		{"3 CPUs 10Gi", Request{CPUs: some(3.0), Memory: some(gib(10))}, "m-4-arm64"},
		{"memory determines", Request{CPUs: some(1.0), Memory: some(gib(12))}, "m-4-arm64"},
		{"disk determines", Request{Disk: some(gib(41))}, "m-4-arm64"},
		{"minimum", Request{CPUs: some(math.SmallestNonzeroFloat64), Memory: some(spec.Bytes(1)), Disk: some(spec.Bytes(1))}, "m-05-arm64"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := m.Resolve(c.r)
			if err != nil || got.Resolved.Name != c.want || got.Requested != c.r {
				t.Fatalf("%+v %v", got, err)
			}
		})
	}
	s := shape(8)
	s.DefaultCPUs = 4
	got, err := mustNew(t, s).Resolve(Request{})
	if err != nil || got.Resolved.Name != "m-4-arm64" {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestOmittedFieldsIgnored(t *testing.T) {
	m := model(t, 8)
	cases := []struct {
		name string
		r    Request
		want string
		norm Request
	}{
		{
			"cpu present, memory and disk junk",
			Request{CPUs: some(1.0), Memory: Optional[spec.Bytes]{Value: 1 << 40}, Disk: Optional[spec.Bytes]{Value: -1}},
			"m-1-arm64",
			Request{CPUs: some(1.0)},
		},
		{
			"memory present, cpu and disk oversized junk",
			Request{CPUs: Optional[float64]{Value: 1 << 40}, Memory: some(gib(4)), Disk: Optional[spec.Bytes]{Value: 1 << 40}},
			"m-1-arm64",
			Request{Memory: some(gib(4))},
		},
		{
			"size omitted but named",
			Request{Size: Optional[string]{Value: "m-8-arm64"}},
			"m-2-arm64",
			Request{},
		},
		{
			"explicit size with raw junk",
			Request{Size: some("m-4-arm64"), CPUs: Optional[float64]{Value: 99}, Memory: Optional[spec.Bytes]{Value: -1}, Disk: Optional[spec.Bytes]{Value: math.MaxInt64}},
			"m-4-arm64",
			Request{Size: some("m-4-arm64")},
		},
		{
			"all omitted with junk",
			Request{Size: Optional[string]{Value: "m-8-arm64"}, CPUs: Optional[float64]{Value: math.NaN()}, Memory: Optional[spec.Bytes]{Value: -1}, Disk: Optional[spec.Bytes]{Value: math.MaxInt64}},
			"m-2-arm64",
			Request{},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := m.Resolve(c.r)
			if err != nil || got.Resolved.Name != c.want || got.Requested != c.norm {
				t.Fatalf("%+v %v", got, err)
			}
		})
	}
}

func TestBoundaries(t *testing.T) {
	for _, cpu := range []float64{4, 8} {
		m := model(t, cpu)
		entries := m.List()
		for i, e := range entries {
			exact := []Request{{CPUs: some(e.CPUs)}, {Memory: some(e.Memory)}, {Disk: some(e.Disk)}, {CPUs: some(e.CPUs), Memory: some(e.Memory), Disk: some(e.Disk)}}
			for _, r := range exact {
				got, err := m.Resolve(r)
				if err != nil || got.Resolved != e {
					t.Fatalf("exact %+v: %+v %v", r, got, err)
				}
			}
			next := []Request{{CPUs: some(math.Nextafter(e.CPUs, math.Inf(1)))}, {Memory: some(e.Memory + 1)}, {Disk: some(e.Disk + 1)}}
			for _, r := range next {
				got, err := m.Resolve(r)
				if i == len(entries)-1 {
					var big *OversizedError
					if !errors.As(err, &big) || big.Requested != r || big.Maximum != e {
						t.Fatalf("oversized: %+v %v", got, err)
					}
				} else if err != nil || got.Resolved != entries[i+1] {
					t.Fatalf("next %+v: %+v %v", r, got, err)
				}
			}
		}
	}
}

func TestBytesText(t *testing.T) {
	for _, c := range []struct {
		b    spec.Bytes
		want string
	}{
		{0, "0"},
		{1, "1"},
		{1536, "1536"},
		{3072, "3Ki"},
		{1 << 20, "1Mi"},
		{gib(160), "160Gi"},
		{1 << 40, "1Ti"},
		{1 << 62, "4194304Ti"},
		{160e9, "156250000Ki"},
		{gib(160) + 1, "171798691841"},
	} {
		if got := bytesText(c.b); got != c.want {
			t.Fatalf("bytesText(%d) = %q, want %q", c.b, got, c.want)
		}
	}
}

func TestOversizedMessage(t *testing.T) {
	m := model(t, 8)
	cases := []struct {
		r       Request
		want    string
		omitted []string
	}{
		{Request{CPUs: some(9.0), Memory: Optional[spec.Bytes]{Value: -1}, Disk: Optional[spec.Bytes]{Value: 1 << 40}}, "cpus=9", []string{"memory", "disk"}},
		{Request{Memory: some(gib(33))}, "memory=33Gi", []string{"cpus", "disk"}},
		{Request{CPUs: some(1.0), Disk: some(gib(160) + 1)}, "disk=171798691841", []string{"memory"}},
	}
	for _, c := range cases {
		_, err := m.Resolve(c.r)
		var big *OversizedError
		if !errors.As(err, &big) {
			t.Fatal(c.r, err)
		}
		if want := normalise(c.r); big.Requested != want {
			t.Fatalf("Requested %+v, want %+v", big.Requested, want)
		}
		msg := err.Error()
		requested, maximum, ok := strings.Cut(msg, " exceeds ")
		if !ok || !strings.Contains(requested, c.want) || !strings.Contains(maximum, "m-8-arm64") || !strings.Contains(maximum, "cpus=8 memory=32Gi disk=160Gi") {
			t.Fatal(msg)
		}
		for _, field := range c.omitted {
			if strings.Contains(requested, field) {
				t.Fatalf("omitted %s in %q", field, msg)
			}
		}
	}
	_, err := m.Resolve(Request{CPUs: some(9.0)})
	if want := "request cpus=9 exceeds the largest size m-8-arm64 (cpus=8 memory=32Gi disk=160Gi)"; err == nil || err.Error() != want {
		t.Fatal(err)
	}
}

func TestErrorKinds(t *testing.T) {
	m := model(t, 8)
	var invalid []Request
	for _, v := range []float64{0, -1, math.NaN(), math.Inf(1), math.Inf(-1)} {
		invalid = append(invalid, Request{CPUs: some(v)}, Request{CPUs: some(v), Memory: some(gib(1))})
	}
	for _, v := range []spec.Bytes{0, -1, math.MinInt64} {
		invalid = append(invalid, Request{Memory: some(v)}, Request{Disk: some(v)}, Request{CPUs: some(1.0), Disk: some(v)})
	}
	for _, r := range invalid {
		_, err := m.Resolve(r)
		var big *OversizedError
		if !errors.Is(err, ErrInvalidRequest) || errors.As(err, &big) {
			t.Fatal(r, err)
		}
	}
	for _, r := range []Request{{CPUs: some(math.MaxFloat64)}, {Memory: some(spec.Bytes(math.MaxInt64))}, {Disk: some(spec.Bytes(math.MaxInt64))}} {
		var big *OversizedError
		if _, err := m.Resolve(r); !errors.As(err, &big) || errors.Is(err, ErrInvalidRequest) {
			t.Fatal(r, err)
		}
	}
	for _, r := range []Request{
		{Size: some("m-2-arm64"), CPUs: some(0.0)},
		{Size: some("m-2-arm64"), Memory: some(spec.Bytes(1))},
		{Size: some("m-2-arm64"), Disk: some(spec.Bytes(0))},
		{Size: some("nope"), CPUs: some(1.0)},
	} {
		if _, err := m.Resolve(r); !errors.Is(err, ErrConflict) {
			t.Fatal(r, err)
		}
	}
}

// tier is the ladder index covering q multiples of the smallest entry: ceil(log2(q)), clamped at 0.
func tier(q float64) int {
	if q <= 1 {
		return 0
	}
	frac, exp := math.Frexp(q)
	if frac == 0.5 {
		return exp - 1
	}
	return exp
}

// byteTier is tier for integer bytes, with exact ceiling division.
func byteTier(req, smallest spec.Bytes) int {
	q := (req + smallest - 1) / smallest
	return bits.Len64(uint64(q - 1))
}

func TestGeneratedCoverAndMonotonicity(t *testing.T) {
	for _, cpu := range []float64{4, 8} {
		m := model(t, cpu)
		entries := m.List()
		smallest := entries[0]
		cpus := []float64{math.SmallestNonzeroFloat64}
		mem := []spec.Bytes{1}
		disk := []spec.Bytes{1}
		for _, e := range entries {
			cpus = append(cpus, e.CPUs, math.Nextafter(e.CPUs, math.Inf(1)))
			mem = append(mem, e.Memory, e.Memory+1)
			disk = append(disk, e.Disk, e.Disk+1)
		}
		index := func(r Request) int {
			want := 0
			if r.CPUs.Present {
				want = max(want, tier(r.CPUs.Value/smallest.CPUs))
			}
			if r.Memory.Present {
				want = max(want, byteTier(r.Memory.Value, smallest.Memory))
			}
			if r.Disk.Present {
				want = max(want, byteTier(r.Disk.Value, smallest.Disk))
			}
			return want
		}
		// resolved returns the ladder index, or len(entries) for an oversized request.
		resolved := func(r Request) int {
			got, err := m.Resolve(r)
			var big *OversizedError
			if errors.As(err, &big) {
				return len(entries)
			}
			if err != nil {
				t.Fatalf("%+v: %v", r, err)
			}
			for i, e := range entries {
				if e == got.Resolved {
					return i
				}
			}
			t.Fatalf("%+v resolved off-ladder %+v", r, got)
			return -1
		}
		for mask := 1; mask < 8; mask++ {
			for _, c := range cpus {
				for _, mb := range mem {
					for _, db := range disk {
						r := Request{
							CPUs:   Optional[float64]{mask&1 != 0, c},
							Memory: Optional[spec.Bytes]{mask&2 != 0, mb},
							Disk:   Optional[spec.Bytes]{mask&4 != 0, db},
						}
						// Junk that every ladder entry fails to cover, so any comparison against it changes the result.
						if !r.CPUs.Present {
							r.CPUs.Value = math.MaxFloat64
						}
						if !r.Memory.Present {
							r.Memory.Value = math.MaxInt64
						}
						if !r.Disk.Present {
							r.Disk.Value = math.MaxInt64
						}
						want := min(index(r), len(entries))
						got := resolved(r)
						if got != want {
							t.Fatalf("mask %b %+v: tier %d, want %d", mask, r, got, want)
						}
						higher := []Request{r, r, r}
						if r.CPUs.Present {
							higher[0].CPUs.Value = math.Nextafter(c, math.Inf(1))
						}
						if r.Memory.Present {
							higher[1].Memory.Value = mb + 1
						}
						if r.Disk.Present {
							higher[2].Disk.Value = db + 1
						}
						for _, h := range higher {
							if resolved(h) < got {
								t.Fatalf("non-monotonic %+v -> %+v", r, h)
							}
						}
					}
				}
			}
		}
	}
}

func TestFallback(t *testing.T) {
	m := model(t, 8)
	for _, d := range []Dimensions{{8, gib(32), gib(160)}, {16, gib(64), gib(320)}, {9, gib(33), gib(161)}} {
		if err := m.ValidateFallback("arm64", d); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		arch string
		d    Dimensions
	}{
		{"amd64", shape(8).Dimensions},
		{"", shape(8).Dimensions},
		{"arm64", Dimensions{math.Nextafter(8, 0), gib(32), gib(160)}},
		{"arm64", Dimensions{16, gib(32) - 1, gib(320)}},
		{"arm64", Dimensions{16, gib(64), gib(160) - 1}},
		{"arm64", Dimensions{math.NaN(), gib(64), gib(320)}},
		{"arm64", Dimensions{math.Inf(1), gib(64), gib(320)}},
	}
	for _, c := range cases {
		if err := m.ValidateFallback(c.arch, c.d); err == nil {
			t.Fatal("invalid fallback", c)
		}
	}
}
