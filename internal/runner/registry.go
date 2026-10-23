package runner

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
)

// registryAuth is a placement's registry credentials, as a containers
// auth.json the runner hands podman (--authfile) for its pulls and pushes.
// The file lives in the runner's tmp dir, 0600, only while it is needed;
// nothing else (events, logs, errors) ever holds the credentials.
type registryAuth struct {
	file    string
	secrets []string // what to redact: values, passwords, their base64
}

// registryLogin writes the auth file for the spec's registryAuth; nil when
// there is none. Registries not listed get no credentials.
func (p *placement) registryLogin(sp spec.RunSpec) (*registryAuth, error) {
	return writeAuthFile(filepath.Join(p.r.cfg.DataDir, "tmp"), sp.Image.RegistryAuth, p.assign.Secrets)
}

func writeAuthFile(dir string, auths []spec.RegistryAuth, secrets map[string]string) (*registryAuth, error) {
	keyed := map[string]string{}
	for _, ra := range auths {
		if v := secrets[ra.Secret]; v != "" {
			keyed[ra.Registry] = v
		}
	}
	return writeAuth(dir, keyed)
}

// managedLogin writes the auth file for the managed image registry: its
// credential keyed by the tenant's own repository and each shared one, and
// nothing else, so Podman offers it for no other repository (containers
// auth.json takes a repository path as a key, and uses the most specific
// one that matches). nil when the assignment has none.
func (p *placement) managedLogin() (*registryAuth, error) {
	m := p.assign.ManagedImages
	if m == nil || m.Credential == "" {
		return nil, nil
	}
	keyed := map[string]string{m.Repository: m.Credential}
	for _, r := range m.Shared {
		keyed[r] = m.Credential
	}
	return writeAuth(filepath.Join(p.r.cfg.DataDir, "tmp"), keyed)
}

// writeAuth writes a containers auth.json of key (a registry, or a
// repository path) to credential; nil when there are none.
func writeAuth(dir string, keyed map[string]string) (*registryAuth, error) {
	type entry struct {
		Auth string `json:"auth"`
	}
	m := map[string]entry{}
	a := &registryAuth{}
	for key, v := range keyed {
		user, pass := registryCredential(v)
		enc := base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
		m[key] = entry{Auth: enc}
		a.secrets = append(a.secrets, v, pass, enc)
	}
	if len(m) == 0 {
		return nil, nil
	}
	b, _ := json.Marshal(map[string]any{"auths": m})
	f, err := os.CreateTemp(dir, "auth-*.json") // 0600
	if err != nil {
		return nil, err
	}
	a.file = f.Name()
	_, err = f.Write(b)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		a.close()
		return nil, err
	}
	return a, nil
}

// registryCredential reads a registryAuth secret: user:password, or a bare
// token, which most registries take as the password of any user.
func registryCredential(v string) (user, pass string) {
	if u, p, ok := strings.Cut(v, ":"); ok && u != "" {
		return u, p
	}
	return "lux", v
}

// path is the --authfile to pass podman ("" for none).
func (a *registryAuth) path() string {
	if a == nil {
		return ""
	}
	return a.file
}

func (a *registryAuth) close() {
	if a != nil && a.file != "" {
		os.Remove(a.file)
	}
}

// redact takes the credentials out of an error's text, as gitws does with
// git's output: podman errors are the Run's failure reason, and events.
func (a *registryAuth) redact(err error) error {
	if err == nil || a == nil {
		return err
	}
	msg := a.redactString(err.Error())
	if msg == err.Error() {
		return err
	}
	return errors.New(msg)
}

func (a *registryAuth) redactString(s string) string {
	if a == nil {
		return s
	}
	for _, v := range a.secrets {
		if v != "" {
			s = strings.ReplaceAll(s, v, "[REDACTED]")
		}
	}
	return s
}

// cacheRef is where the build cache keeps a build: the local tag's key,
// in the spec's repository.
func cacheRef(repo, key string) string { return repo + ":" + key }

// imageAuth is a placement's registry credentials while it readies its
// image: the spec's (registryAuth), for every registry they name, and the
// managed registry's, for its tenant's repository and the shared ones only.
type imageAuth struct {
	user, managed *registryAuth
	m             *proto.ManagedImages
	tenant        string
}

// forPull is the auth file to pull ref with: the managed registry's for
// an image in it (refused for another tenant's repository, or when luxd
// sent no credential), the spec's for any other.
func (a *imageAuth) forPull(ref string) (*registryAuth, error) {
	// Every image a spec names comes through here before any local copy is
	// reused: lux's own published names never do (a spec stored before the
	// rule, or a luxd without it, included), whatever luxd sent.
	if spec.ReservedImage(ref) {
		return nil, fmt.Errorf("%s is lux's own name for published images: name the publishedRef", ref)
	}
	if a == nil {
		return nil, nil
	}
	if a.m != nil {
		mi := spec.ManagedImages{Root: a.m.Root, Shared: a.m.Shared}
		if mi.Under(ref) {
			if spec.ImageRepository(ref) != a.m.Repository && !slices.Contains(a.m.Shared, spec.ImageRepository(ref)) {
				return nil, fmt.Errorf("%s is another tenant's managed image repository", spec.ImageRepository(ref))
			}
			if err := a.managedUsable(); err != nil {
				return nil, err
			}
			return a.managed, nil
		}
	}
	return a.user, nil
}

// managedUsable: luxd sent the managed registry's credential, and it has
// not expired (luxd knows an ECR token's expiry, and hands out only one
// valid for images.ecr_min_validity; an image taking longer to ready
// finds it expired, and says so rather than failing with the registry's
// unauthorized).
func (a *imageAuth) managedUsable() error {
	if a.managed == nil {
		return errNoManagedCredential
	}
	if ms := a.m.CredentialExpiresAt; ms != 0 && !time.Now().Before(time.UnixMilli(ms).Add(-time.Minute)) {
		return fmt.Errorf("managed images: luxd's registry credential expired at %s: this image took longer to ready than [images] ecr_min_validity allows",
			time.UnixMilli(ms).UTC().Format(time.RFC3339))
	}
	return nil
}

var errNoManagedCredential = errors.New("managed images: luxd sent no credential for its image registry (images.credential_file or credential_env unreadable?)")

func (a *imageAuth) close() {
	if a != nil {
		a.user.close()
		a.managed.close()
	}
}

func (a *imageAuth) redact(err error) error {
	if a == nil {
		return err
	}
	return a.managed.redact(a.user.redact(err))
}

func (a *imageAuth) redactString(s string) string {
	if a == nil {
		return s
	}
	return a.managed.redactString(a.user.redactString(s))
}
