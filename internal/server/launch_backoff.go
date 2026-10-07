package server

import (
	"encoding/json"
	"fmt"
	"time"
)

// Provider failures delay only launches, doubling from 15s to 5m after
// each failed attempt returns. Backoffs are in memory and reset on each
// newly acquired lease epoch, not on renewal of an unexpired lease.
const (
	launchBackoffInitial = 15 * time.Second
	launchBackoffMax     = 5 * time.Minute
)

type poolBackoff struct {
	failures  int
	notBefore time.Time
	lastErr   error
	config    string
}

// launchRefused distinguishes provider errors from persistence and quota errors.
type launchRefused struct{ err error }

func (e *launchRefused) Error() string { return e.err.Error() }
func (e *launchRefused) Unwrap() error { return e.err }

// poolConfig fingerprints all provisioner settings, comparing pointers by value.
func poolConfig(pl poolRow) string {
	b, _ := json.Marshal(pl)
	return string(b)
}

func (s *Server) launchBackoffFor(pl poolRow) *poolBackoff {
	bo := s.launchBackoff[pl.ID]
	if bo != nil && bo.config != poolConfig(pl) {
		delete(s.launchBackoff, pl.ID)
		return nil
	}
	return bo
}

func (s *Server) launchFailed(pl poolRow, err error) {
	if s.launchBackoff == nil {
		s.launchBackoff = map[string]*poolBackoff{}
	}
	bo := s.launchBackoffFor(pl)
	if bo == nil {
		bo = &poolBackoff{config: poolConfig(pl)}
		s.launchBackoff[pl.ID] = bo
	}
	bo.failures++
	delay := s.cfg.LaunchBackoff
	for i := 1; i < bo.failures && delay < s.cfg.LaunchBackoffMax; i++ {
		delay *= 2
	}
	bo.notBefore = s.now().Add(min(delay, s.cfg.LaunchBackoffMax))
	bo.lastErr = err
}

// Changing errors and countdowns do not distinguish backoff events.
var backoffVolatile = []string{"failures", "retryInSeconds", "detail", "error"}

func (bo *poolBackoff) evidence(now time.Time) map[string]any {
	wait := bo.notBefore.Sub(now).Round(time.Second)
	// Not providerErrorText: its 500-byte cut can split a rune.
	text := truncateRunes(requestID.ReplaceAllString(bo.lastErr.Error(), ""), 200)
	return map[string]any{"failures": bo.failures, "retryInSeconds": int(wait.Seconds()), "error": text,
		"detail": fmt.Sprintf("launch backing off after %d failures; next attempt in %s: %s", bo.failures, wait, text)}
}

func truncateRunes(s string, n int) string {
	for i := range s {
		if n == 0 {
			return s[:i]
		}
		n--
	}
	return s
}

func (s *Server) forgetLaunchBackoffs(pools []poolRow) {
	live := make(map[string]bool, len(pools))
	for _, pl := range pools {
		live[pl.ID] = true
	}
	for id := range s.launchBackoff {
		if !live[id] {
			delete(s.launchBackoff, id)
		}
	}
}
