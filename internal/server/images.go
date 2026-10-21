package server

import (
	"encoding/json"
	"os"
	"strings"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
)

// ImagesConfig is luxd's managed image registry ([images]): where Runs
// with image.build.publish publish (Repository/<tenant id>), and the one
// credential the runners push and pull those images with. The credential
// is read from CredentialFile or the environment variable CredentialEnv
// whenever an assignment needs it (so a rotated file takes effect without
// a restart), attached to that assignment as it is sent, and never stored.
type ImagesConfig struct {
	Repository     string
	CredentialFile string
	CredentialEnv  string
	// Shared: repositories every tenant may pull with the credential
	// (a common base image), never push to.
	Shared []string
}

// configured: a repository and somewhere to read its credential from.
func (c ImagesConfig) configured() bool {
	return c.Repository != "" && (c.CredentialFile != "" || c.CredentialEnv != "")
}

// managedImages is the managed registry as specs see it; Root "" when
// none is configured.
func (s *Server) managedImages() spec.ManagedImages {
	c := s.cfg.Images
	if !c.configured() {
		return spec.ManagedImages{}
	}
	return spec.ManagedImages{Root: c.Repository, Shared: c.Shared}
}

// managedCredential reads the managed registry's credential; "" when it
// can't (the runner then fails what needs it, saying so).
func (s *Server) managedCredential() string {
	c := s.cfg.Images
	var v string
	switch {
	case c.CredentialFile != "":
		b, err := os.ReadFile(c.CredentialFile)
		if err != nil {
			s.log.Warn("managed images: reading the credential file failed", "file", c.CredentialFile, "err", err)
			return ""
		}
		v = string(b)
	case c.CredentialEnv != "":
		v = os.Getenv(c.CredentialEnv)
	}
	return strings.TrimSpace(v)
}

// checkManagedImages refuses a spec that publishes without a managed
// registry, or names another tenant's managed images.
func (s *Server) checkManagedImages(tenant string, sp spec.RunSpec) error {
	if errs := s.managedImages().CheckManaged(sp, tenant); len(errs) > 0 {
		return &spec.ValidationError{Problems: errs}
	}
	return nil
}

// attachManagedImages adds the managed registry, with its credential, to
// an assignment that needs it, as it is sent: never stored.
func (s *Server) attachManagedImages(a *proto.Assign) bool {
	m := s.managedImages()
	cf := ""
	if a.ImageResolved != nil {
		cf = a.ImageResolved.Containerfile
	}
	if m.Root == "" || !m.Uses(a.Spec, cf) {
		return false
	}
	a.ManagedImages = &proto.ManagedImages{
		Root:       m.Root,
		Repository: spec.TenantRepository(m.Root, a.TenantID),
		Shared:     m.Shared,
		Credential: s.managedCredential(),
	}
	return true
}

// attachSecrets adds to an assignment, as it is sent, what luxd holds in
// memory only: the Run's secret values, and the managed registry's
// credential when the Run needs it.
func (s *Server) attachSecrets(runID string, data json.RawMessage) json.RawMessage {
	var a proto.Assign
	if err := json.Unmarshal(data, &a); err != nil {
		return data
	}
	changed := s.attachManagedImages(&a)
	if vals, ok := s.secrets.get(runID); ok && len(vals) > 0 {
		a.Secrets = vals
		changed = true
	}
	if !changed {
		return data
	}
	return proto.Marshal(a)
}

// markManagedAuth sets each Run's ManagedAuth from this luxd's managed
// registry: whether it needs the credential luxd supplies (it publishes,
// or its image or a FROM is a managed or shared repository's), so that
// only a runner with proto.CapManagedImages, which takes the credential,
// gets it. A Run using only its own registryAuth needs no such runner.
func (s *Server) markManagedAuth(runs ...*pendingRun) {
	m := s.managedImages()
	for _, r := range runs {
		cf := ""
		if r.ImageResolved != nil {
			cf = r.ImageResolved.Containerfile
		}
		r.ManagedAuth = m.Uses(r.Spec, cf)
	}
}

func slicePtrs[T any](xs []T) []*T {
	out := make([]*T, len(xs))
	for i := range xs {
		out[i] = &xs[i]
	}
	return out
}
