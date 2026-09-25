package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

// runnerArches are the architectures luxd may hold runner binaries for.
// Others are simply never offered.
var runnerArches = []string{"arm64", "amd64"}

// runnerBinNames are the files expected under runner_bin_dir/linux-<arch>/.
var runnerBinNames = []string{"lux-runner", "lux-shim"}

// runnerBin is one binary read into memory at startup: its bytes and their
// sha256, together, so serving one can never disagree with the other.
type runnerBin struct {
	data   []byte
	sha256 string
}

// loadRunnerBinaries reads whatever runner_bin_dir holds into memory, once
// at startup: arch/linux-arm64/lux-runner and lux-shim, and the amd64
// equivalents, hashed as they are read. A missing arch directory or file
// is simply not offered. Serving from these bytes (serveRunnerBin), rather
// than reopening the path on every request, means a release that replaces
// the files on disk in place can never make luxd serve bytes that don't
// match the sha256 it already told a host: the new build only takes
// effect on luxd's own restart, same as any other config.
func (s *Server) loadRunnerBinaries() {
	s.bins = map[string]map[string]runnerBin{}
	if s.cfg.RunnerBinDir == "" {
		return
	}
	for _, arch := range runnerArches {
		for _, name := range runnerBinNames {
			data, err := os.ReadFile(filepath.Join(s.cfg.RunnerBinDir, "linux-"+arch, name))
			if err != nil {
				continue
			}
			if s.bins[arch] == nil {
				s.bins[arch] = map[string]runnerBin{}
			}
			s.bins[arch][name] = runnerBin{data: data, sha256: sha256Hex(data)}
		}
	}
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// outdatedBinariesReason is the state_reason for an outdated-binaries
// drain, and the ExitHost.Reason sent with the exit.
const outdatedBinariesReason = "outdated binaries"

// hasBothBinaries reports whether luxd holds every binary in
// runnerBinNames for arch: the precondition for drain and undrain
// decisions, so luxd is never asking a host for what it cannot itself
// serve (a partial upload, or a build that only produced one file).
func (s *Server) hasBothBinaries(arch string) bool {
	have := s.bins[arch]
	for _, name := range runnerBinNames {
		if _, ok := have[name]; !ok {
			return false
		}
	}
	return true
}

// binariesOutdated reports whether luxd holds a binary for arch that
// differs from what the runner reports having. Only when luxd holds both
// binaries for the arch (hasBothBinaries): an older runner (no shas), an
// arch luxd serves nothing for, or one where only one of the pair is
// present are never grounds to drain — luxd never drains a host for an
// arch it cannot itself serve.
func (s *Server) binariesOutdated(arch, runnerSHA, shimSHA string) bool {
	if !s.hasBothBinaries(arch) {
		return false
	}
	have := s.bins[arch]
	if runnerSHA != "" && have["lux-runner"].sha256 != runnerSHA {
		return true
	}
	if shimSHA != "" && have["lux-shim"].sha256 != shimSHA {
		return true
	}
	return false
}

// binariesMatch is the converse, used to un-drain a host once a restart
// downloaded binaries that now match: both shas must equal what luxd holds
// (a runner reporting nothing never matches; it stays drained).
func (s *Server) binariesMatch(arch, runnerSHA, shimSHA string) bool {
	have := s.bins[arch]
	return s.hasBothBinaries(arch) && have["lux-runner"].sha256 == runnerSHA && have["lux-shim"].sha256 == shimSHA
}

// drainIfOutdated cordons hostID (drainHosts with no stopReason: no Run is
// stopped) when its binaries are outdated and its pool has room for one
// more such drain: at most max(1, OutdatedDrainPercent% of the pool's live
// hosts) at once, so a release never cordons a whole pool in one instant.
// The pool's pg_advisory_xact_lock serializes count-then-cordon across
// concurrent Hellos (a luxd restart wakes a whole pool at once); under
// READ COMMITTED they would otherwise all read the same count and
// overshoot the cap. Returns the hosts to notify.
func (s *Server) drainIfOutdated(ctx context.Context, tx pgx.Tx, hostID, arch, runnerSHA, shimSHA string) ([]string, error) {
	if !s.binariesOutdated(arch, runnerSHA, shimSHA) {
		return nil, nil
	}
	var tenant, pool string
	if err := tx.QueryRow(ctx, `SELECT coalesce(tenant_id, ''), pool FROM hosts WHERE id = $1`, hostID).Scan(&tenant, &pool); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('outdated-drain:' || $1 || '/' || $2, 0))`, tenant, pool); err != nil {
		return nil, err
	}
	var live, draining int
	err := tx.QueryRow(ctx, `
		SELECT
			count(*) FILTER (WHERE state IN ('ready', 'draining')),
			count(*) FILTER (WHERE state = 'draining' AND $3 = ANY(drain_causes))
		FROM hosts
		WHERE coalesce(tenant_id, '') = $1 AND pool = $2`,
		tenant, pool, causeOutdated).Scan(&live, &draining)
	if err != nil || draining >= max(1, live*s.cfg.OutdatedDrainPercent/100) {
		return nil, err
	}
	return s.drainHosts(ctx, tx, outdatedBinariesReason, causeOutdated, "", "id = $1", hostID)
}

// runnerBinManifest is the contract's {"linux-arm64": {"lux-runner": sha,
// "lux-shim": sha}, ...}, for arches luxd holds both binaries for: a host
// downloading by this manifest, or a drain decision made from it, never
// meets an arch luxd can only half serve.
func (s *Server) runnerBinManifest() map[string]map[string]string {
	out := map[string]map[string]string{}
	for arch, m := range s.bins {
		if !s.hasBothBinaries(arch) {
			continue
		}
		shas := map[string]string{}
		for name, b := range m {
			shas[name] = b.sha256
		}
		out["linux-"+arch] = shas
	}
	return out
}

// serveRunnerBinManifest is GET /runner/v1/bin/manifest: host-token auth, like
// the other /runner/v1/... routes.
func (s *Server) serveRunnerBinManifest(w http.ResponseWriter, r *http.Request) error {
	if _, err := s.authHostToken(r); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, s.runnerBinManifest())
	return nil
}

// serveRunnerBin is GET /runner/v1/bin/linux-{arch}/{lux-runner|lux-shim}:
// serves the binary (~20MB) held in memory since startup (loadRunnerBinaries),
// never reopening the path — a release that replaces the file on disk in
// place can never make this serve bytes whose sha256 differs from what it
// advertises, in X-Lux-Sha256 and the manifest, for the process's life.
func (s *Server) serveRunnerBin(w http.ResponseWriter, r *http.Request) error {
	if _, err := s.authHostToken(r); err != nil {
		return err
	}
	name := r.PathValue("name")
	arch, ok := strings.CutPrefix(r.PathValue("osArch"), "linux-")
	if !ok || !slices.Contains(runnerBinNames, name) {
		return errNotFound
	}
	b, ok := s.bins[arch][name]
	if !ok {
		return errNotFound
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(len(b.data)))
	w.Header().Set("X-Lux-Sha256", b.sha256)
	if _, err := w.Write(b.data); err != nil {
		s.log.Warn("runner binary download cut short", "arch", arch, "name", name, "err", err)
		panic(http.ErrAbortHandler) // abort the response: never a clean end
	}
	return nil
}
