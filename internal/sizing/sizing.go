// Package sizing resolves nominal coupled pool allocations without applying Run defaults.
package sizing

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/marcioapm/lux/internal/spec"
)

// MaxName bounds the canonical name independently of future API schemas.
const MaxName = 63

// maxCPUs is a policy cap on the nominal CPU count.
const maxCPUs = 1 << 53

var (
	ErrInvalidShape   = errors.New("invalid pool shape")
	ErrInvalidRequest = errors.New("invalid resource request")
	ErrConflict       = errors.New("conflicting resource fields")
	ErrUnknownSize    = errors.New("unknown size")
)

var familyRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

type Dimensions struct {
	CPUs   float64
	Memory spec.Bytes
	Disk   spec.Bytes
}

type Shape struct {
	Dimensions
	Family      string
	Arch        string
	MinCPUs     float64
	DefaultCPUs float64
}

type Entry struct {
	Dimensions
	Name     string
	Fraction float64
}

// Optional separates omission from an explicit zero; Value is meaningful only when Present.
type Optional[T any] struct {
	Present bool
	Value   T
}

type Request struct {
	Size   Optional[string]
	CPUs   Optional[float64]
	Memory Optional[spec.Bytes]
	Disk   Optional[spec.Bytes]
}

// Resolution.Requested holds the request with omitted fields' values zeroed.
type Resolution struct {
	Requested Request
	Resolved  Entry
}

type OversizedError struct {
	Requested Request
	Maximum   Entry
}

func (e *OversizedError) Error() string {
	var req []string
	r, top := e.Requested, e.Maximum
	if r.CPUs.Present {
		req = append(req, "cpus="+cpuText(r.CPUs.Value))
	}
	if r.Memory.Present {
		req = append(req, "memory="+bytesText(r.Memory.Value))
	}
	if r.Disk.Present {
		req = append(req, "disk="+bytesText(r.Disk.Value))
	}
	return fmt.Sprintf("request %s exceeds the largest size %s (cpus=%s memory=%s disk=%s)",
		strings.Join(req, " "), top.Name, cpuText(top.CPUs), bytesText(top.Memory), bytesText(top.Disk))
}

func cpuText(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }

// bytesText uses the largest binary unit spec.Bytes parses that divides b exactly, so the text never rounds.
func bytesText(b spec.Bytes) string {
	units := []string{"", "Ki", "Mi", "Gi", "Ti"}
	i := 0
	for i < len(units)-1 && b != 0 && b%1024 == 0 {
		b /= 1024
		i++
	}
	return strconv.FormatInt(int64(b), 10) + units[i]
}

// Model owns a validated ladder, ascending from the minimum to the whole host. Construct it with New.
type Model struct {
	shape        Shape
	entries      []Entry
	defaultEntry Entry
}

func positive(v float64) bool { return v > 0 && !math.IsInf(v, 0) && !math.IsNaN(v) }

func powerOfTwo(v float64) bool {
	fraction, _ := math.Frexp(v)
	return positive(v) && fraction == 0.5
}

func shapeError(format string, a ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrInvalidShape}, a...)...)
}

func New(s Shape) (*Model, error) {
	if !familyRE.MatchString(s.Family) {
		return nil, shapeError("invalid family %q", s.Family)
	}
	if s.Arch != "arm64" && s.Arch != "amd64" {
		return nil, shapeError("invalid architecture %q", s.Arch)
	}
	if !powerOfTwo(s.CPUs) {
		return nil, shapeError("nominal CPUs %g is not a power of two", s.CPUs)
	}
	if !powerOfTwo(s.MinCPUs) {
		return nil, shapeError("minimum CPUs %g is not a power of two", s.MinCPUs)
	}
	// 0.5 is the smallest nameable CPU tier.
	if s.MinCPUs < 0.5 {
		return nil, shapeError("minimum CPUs %g is below 0.5", s.MinCPUs)
	}
	if s.CPUs < s.MinCPUs {
		return nil, shapeError("nominal CPUs %g is below minimum %g", s.CPUs, s.MinCPUs)
	}
	if s.CPUs > maxCPUs {
		return nil, shapeError("nominal CPUs %g exceeds the 2^53 cap", s.CPUs)
	}
	if s.Memory <= 0 || s.Disk <= 0 {
		return nil, shapeError("nominal memory and disk must be positive bytes")
	}
	// Both are powers of two in [0.5, 2^53], so tiers is in [1, 55] and the divisor fits in int64.
	_, top := math.Frexp(s.CPUs)
	_, bottom := math.Frexp(s.MinCPUs)
	tiers := top - bottom + 1
	divisor := spec.Bytes(1) << (tiers - 1)
	if s.Memory%divisor != 0 || s.Disk%divisor != 0 {
		return nil, shapeError("nominal bytes are not divisible by %d down to the minimum tier", divisor)
	}
	m := &Model{shape: s, entries: make([]Entry, 0, tiers)}
	memory, disk := s.Memory, s.Disk
	cpu, fraction := s.CPUs, 1.0
	for i := 0; i < tiers; i++ {
		token := strconv.FormatFloat(cpu, 'f', 0, 64)
		if cpu == 0.5 {
			token = "05"
		}
		name := s.Family + "-" + token + "-" + s.Arch
		if len(name) > MaxName {
			return nil, shapeError("size name exceeds %d characters: %q", MaxName, name)
		}
		e := Entry{Dimensions: Dimensions{cpu, memory, disk}, Name: name, Fraction: fraction}
		m.entries = append(m.entries, e)
		if cpu == s.DefaultCPUs {
			m.defaultEntry = e
		}
		cpu, memory, disk, fraction = cpu/2, memory/2, disk/2, fraction/2
	}
	if m.defaultEntry.Name == "" {
		return nil, shapeError("default CPUs %g is not a ladder entry", s.DefaultCPUs)
	}
	slices.Reverse(m.entries)
	return m, nil
}

func (m *Model) List() []Entry { return slices.Clone(m.entries) }

// Parse accepts only canonical names belonging to this nominal pool ladder.
func (m *Model) Parse(name string) (Entry, error) {
	if len(m.entries) == 0 {
		return Entry{}, fmt.Errorf("%w: model was not built with New", ErrInvalidShape)
	}
	for _, e := range m.entries {
		if e.Name == name {
			return e, nil
		}
	}
	return Entry{}, fmt.Errorf("%w: %q is not in family %q architecture %q ladder", ErrUnknownSize, name, m.shape.Family, m.shape.Arch)
}

func covers(e Entry, r Request) bool {
	return (!r.CPUs.Present || e.CPUs >= r.CPUs.Value) &&
		(!r.Memory.Present || e.Memory >= r.Memory.Value) &&
		(!r.Disk.Present || e.Disk >= r.Disk.Value)
}

func dropOmitted[T any](o Optional[T]) Optional[T] {
	if !o.Present {
		return Optional[T]{}
	}
	return o
}

// normalise drops omitted fields' values so they are never retained or compared.
func normalise(r Request) Request {
	return Request{dropOmitted(r.Size), dropOmitted(r.CPUs), dropOmitted(r.Memory), dropOmitted(r.Disk)}
}

func (m *Model) Resolve(r Request) (Resolution, error) {
	if len(m.entries) == 0 {
		return Resolution{}, shapeError("model was not constructed by New")
	}
	// Validation and matching read r as given, so the Present checks alone keep omitted values out.
	kept := normalise(r)
	raw := r.CPUs.Present || r.Memory.Present || r.Disk.Present
	if r.Size.Present && raw {
		return Resolution{}, fmt.Errorf("%w: size %q with raw CPU/memory/disk fields", ErrConflict, r.Size.Value)
	}
	if r.Size.Present {
		e, err := m.Parse(r.Size.Value)
		if err != nil {
			return Resolution{}, err
		}
		return Resolution{kept, e}, nil
	}
	if (r.CPUs.Present && !positive(r.CPUs.Value)) || (r.Memory.Present && r.Memory.Value <= 0) || (r.Disk.Present && r.Disk.Value <= 0) {
		return Resolution{}, fmt.Errorf("%w: supplied CPU/memory/disk fields must be finite and positive", ErrInvalidRequest)
	}
	if !raw {
		return Resolution{kept, m.defaultEntry}, nil
	}
	for _, e := range m.entries {
		if covers(e, r) {
			return Resolution{kept, e}, nil
		}
	}
	return Resolution{}, &OversizedError{kept, m.entries[len(m.entries)-1]}
}

// ValidateFallback checks nominal capabilities, not live free capacity or runner protocol support.
func (m *Model) ValidateFallback(arch string, d Dimensions) error {
	if arch != m.shape.Arch {
		return fmt.Errorf("fallback architecture %q differs from shape %q", arch, m.shape.Arch)
	}
	if !positive(d.CPUs) || d.CPUs < m.shape.CPUs || d.Memory < m.shape.Memory || d.Disk < m.shape.Disk {
		return fmt.Errorf("fallback capacities %+v do not cover nominal shape %+v", d, m.shape.Dimensions)
	}
	return nil
}
