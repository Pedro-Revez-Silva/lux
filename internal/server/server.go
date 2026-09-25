// Package server is luxd: the REST API, the runner hub, the scheduler and
// the reapers.
package server

import (
	"cmp"
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/marcioapm/lux/console"
	"github.com/marcioapm/lux/internal/blob"
	"github.com/marcioapm/lux/internal/spec"
	"github.com/marcioapm/lux/internal/store"
)

// Provisioning defaults (luxd's LUX_SCALE_DOWN_AFTER, LUX_LAUNCH_TIMEOUT).
const (
	DefaultScaleDownAfter = 10 * time.Minute
	DefaultLaunchTimeout  = 10 * time.Minute
	// How often each pool's instances are listed with the provider, how
	// long a lost provisioned host keeps its instance, and how long after a
	// launch the listings may still miss it.
	DefaultProviderCheckEvery = time.Minute
	DefaultLostGrace          = 5 * time.Minute
	DefaultListingLag         = time.Minute
	// DefaultOutdatedDrainPercent: 10% of a pool's hosts (at least 1) may
	// drain for outdated binaries at once.
	DefaultOutdatedDrainPercent = 10
)

type Config struct {
	Listen    string
	PublicURL string
	// RunnerURL is the URL runners use to reach luxd (LUX_URL in their
	// env), which may be a private address clients can't reach. Empty:
	// PublicURL.
	RunnerURL string
	// RunnerBinDir holds runner binaries luxd serves and hashes at
	// startup for self-update: <dir>/linux-{arm64,amd64}/{lux-runner,lux-shim}.
	// Empty: luxd offers none, and never drains a host for being outdated.
	RunnerBinDir string
	// LeaseDuration is how long a host may go without a heartbeat before it
	// and its live Runs are lost.
	LeaseDuration time.Duration
	// Tick is the scheduler and reaper interval.
	Tick time.Duration
	// Provisioners by pool provider (ec2).
	Providers map[string]Provider
	// ScaleDownAfter is how long a provisioned host stays idle before it
	// is drained and terminated (beyond the pool's minimum and warm hosts).
	ScaleDownAfter time.Duration
	// LaunchTimeout is how long a launched host may take to register.
	LaunchTimeout time.Duration
	// ProviderCheckEvery is how often the provisioner lists each pool's
	// instances with the provider (orphans, vanished hosts; API limits).
	ProviderCheckEvery time.Duration
	// ListingLag is how long after a launch the provider's listings may still
	// miss the instance (EC2's are eventually consistent): until then, a host
	// missing from them is not taken for gone.
	ListingLag time.Duration
	// LostGrace is how long a lost provisioned host keeps its instance
	// before it is terminated (a restart or network blip is not a loss).
	LostGrace time.Duration
	// OutdatedDrainPercent caps how many of a pool's hosts may be
	// draining for outdated binaries at once, as a percentage of its live
	// hosts (at least 1 regardless). 0: DefaultOutdatedDrainPercent.
	// Every luxd instance must serve identical runner binaries before a
	// rolling deploy: otherwise which one a host's next heartbeat reaches
	// decides whether it is "outdated" (docs/operations.md).
	OutdatedDrainPercent int
	// Defaults are a Run's resources where its spec leaves them unset
	// (zero: spec.BuiltinDefaults).
	Defaults spec.Defaults
	// ConsoleAuth: how requests without an API key authenticate (access.go).
	ConsoleAuth ConsoleAuth
	// SampleEvery is how often the system is sampled for history.
	SampleEvery time.Duration
	// How long history is kept: raw samples, minute and hour rollups.
	HistoryRaw, HistoryMinutes, HistoryHours time.Duration
}

type Server struct {
	cfg   Config
	db    *store.Store
	blobs *blob.Store
	log   *slog.Logger
	hub   *Hub
	// secrets holds submitted secret values in memory, by run id, until
	// the placement that needs them has been assigned. Never persisted.
	secrets *secretCache
	// cfAccess verifies Cloudflare Access tokens, when that is the console
	// auth (access.go).
	cfAccess *cfAccess
	// wakeups wake followers of Run events (wakeups.go).
	wakeups *wakeups
	kick    chan struct{}
	// lastAliveCheck: when the provisioner last asked providers which
	// hosts still exist.
	lastAliveCheck time.Time
	// deployment identifies this lux database in provider tags, so two
	// deployments sharing a cloud account never take each other's
	// instances for orphans (read by the provisioner).
	deployment string
	// bins: the runner binaries (runner_bin_dir), by arch then name
	// (lux-runner, lux-shim), read into memory and hashed once at
	// startup. Served from these bytes, never reopened, so an in-place
	// file swap can never make luxd serve bytes that don't match the
	// sha256 it advertises.
	bins map[string]map[string]runnerBin
	wg   sync.WaitGroup
}

func New(cfg Config, db *store.Store, blobs *blob.Store, log *slog.Logger) *Server {
	if cfg.LeaseDuration == 0 {
		cfg.LeaseDuration = 30 * time.Second
	}
	if cfg.Tick == 0 {
		cfg.Tick = time.Second
	}
	if cfg.ProviderCheckEvery == 0 {
		cfg.ProviderCheckEvery = DefaultProviderCheckEvery
	}
	if cfg.LostGrace == 0 {
		cfg.LostGrace = DefaultLostGrace
	}
	if cfg.ListingLag == 0 {
		cfg.ListingLag = DefaultListingLag
	}
	if cfg.ScaleDownAfter == 0 {
		cfg.ScaleDownAfter = DefaultScaleDownAfter
	}
	if cfg.LaunchTimeout == 0 {
		cfg.LaunchTimeout = DefaultLaunchTimeout
	}
	if cfg.OutdatedDrainPercent == 0 {
		cfg.OutdatedDrainPercent = DefaultOutdatedDrainPercent
	}
	if cfg.Defaults == (spec.Defaults{}) {
		cfg.Defaults = spec.BuiltinDefaults
	}
	cfg.RunnerURL = cmp.Or(cfg.RunnerURL, cfg.PublicURL)
	cfg.SampleEvery = cmp.Or(cfg.SampleEvery, 10*time.Second)
	cfg.HistoryRaw = cmp.Or(cfg.HistoryRaw, DefaultHistoryRaw)
	cfg.HistoryMinutes = cmp.Or(cfg.HistoryMinutes, DefaultHistoryMinutes)
	cfg.HistoryHours = cmp.Or(cfg.HistoryHours, DefaultHistoryHours)
	s := &Server{
		cfg:     cfg,
		db:      db,
		blobs:   blobs,
		log:     log,
		secrets: newSecretCache(),
		wakeups: newWakeups(),
		kick:    make(chan struct{}, 1),
	}
	if cfg.ConsoleAuth.Mode == "cloudflare-access" {
		s.cfAccess = newCFAccess(cfg.ConsoleAuth.CFTeam, cfg.ConsoleAuth.CFAud)
	}
	s.hub = newHub(s)
	s.loadRunnerBinaries()
	return s
}

// Kick asks the scheduler to run now rather than at the next tick.
func (s *Server) Kick() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	s.newAPI(mux)
	s.runnerRoutes(mux)
	// The console used to live under /console/: old links land on the same
	// page at the root.
	mux.HandleFunc("GET /console", redirectConsole)
	mux.HandleFunc("GET /console/{path...}", redirectConsole)
	// The operator console is the catch-all: static files; it calls the API
	// with the key its user gives it. /v1/ and /runner/ never reach it, so
	// an unknown API path keeps the mux's own 404 or 405.
	app := console.Handler()
	root := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if (r.Method == http.MethodGet || r.Method == http.MethodHead) &&
			p != "/v1" && !strings.HasPrefix(p, "/v1/") &&
			p != "/runner" && !strings.HasPrefix(p, "/runner/") {
			if _, pattern := mux.Handler(r); pattern == "" {
				app.ServeHTTP(w, r)
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
	return logMiddleware(s.log, root)
}

func redirectConsole(w http.ResponseWriter, r *http.Request) {
	u := *r.URL
	u.Path = "/" + strings.TrimLeft(r.PathValue("path"), "/")
	u.RawPath = ""
	http.Redirect(w, r, u.RequestURI(), http.StatusMovedPermanently)
}

// Run starts the background loops and serves until ctx ends.
func (s *Server) Run(ctx context.Context) error {
	srv := &http.Server{Addr: s.cfg.Listen, Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	s.wg.Add(6)
	go func() { defer s.wg.Done(); s.schedulerLoop(ctx) }()
	go func() { defer s.wg.Done(); s.provisionerLoop(ctx) }()
	go func() { defer s.wg.Done(); s.reaperLoop(ctx) }()
	go func() { defer s.wg.Done(); s.hub.deliveryLoop(ctx) }()
	go func() { defer s.wg.Done(); s.historyLoop(ctx) }()
	go func() { defer s.wg.Done(); s.listenLoop(ctx) }()
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	s.log.Info("luxd listening", "addr", s.cfg.Listen)
	select {
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s.hub.closeAll()
		_ = srv.Shutdown(shutdown)
		s.wg.Wait()
		return nil
	case err := <-errc:
		return err
	}
}

type secretCache struct {
	mu sync.Mutex
	m  map[string]map[string]string
}

func newSecretCache() *secretCache { return &secretCache{m: map[string]map[string]string{}} }

func (c *secretCache) put(runID string, v map[string]string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[runID] = v
}

func (c *secretCache) get(runID string) (map[string]string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.m[runID]
	return v, ok
}

func (c *secretCache) drop(runID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, runID)
}
