package runner

import (
	"fmt"

	"github.com/marcioapm/lux/internal/hoststat"
)

// diskScale turns a Run's resources.disk, in the terms of the nominal disk
// the host offers (Config.Disk), into the limit it is held to: the same
// share of what the filesystem can give Runs, fsCapacity - headroom. It is
// on only when the host offers a nominal disk and was given a headroom;
// otherwise (the zero value) every limit is resources.disk as asked.
//
// fsCapacity is hoststat.Disk.Capacity, the same unprivileged accounting
// as the heartbeat's free bytes and the image GC. It is read once, at
// start: free bytes change as Runs write, and must not resize every Run's
// limit.
type diskScale struct {
	share
	on                   bool
	headroom, fsCapacity int64
}

// newDiskScale reads the filesystem holding dir with statfs, only when
// scaling is on.
func newDiskScale(nominal int64, headroom *int64, dir string, statfs func(string) (hoststat.Disk, error)) (diskScale, error) {
	if nominal <= 0 || headroom == nil {
		return diskScale{}, nil
	}
	if *headroom < 0 {
		return diskScale{}, fmt.Errorf("--disk-headroom must not be negative")
	}
	d, err := statfs(dir)
	if err != nil {
		return diskScale{}, fmt.Errorf("--disk-headroom: reading the filesystem of %s: %w", dir, err)
	}
	fsCapacity := d.Capacity()
	allocatable := fsCapacity - *headroom
	if allocatable <= 0 {
		return diskScale{}, fmt.Errorf("--disk-headroom %d leaves no disk for Runs: the filesystem of %s has %d bytes", *headroom, dir, fsCapacity)
	}
	return diskScale{share: newShare(allocatable, nominal), on: true, headroom: *headroom, fsCapacity: fsCapacity}, nil
}

// limit is the disk a Run asking requested bytes is held to, rounded down
// to whole bytes. A positive nominal request may have a zero-byte limit.
func (d diskScale) limit(requested int64) int64 {
	return d.of(requested)
}
