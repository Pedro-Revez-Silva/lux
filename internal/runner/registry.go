package runner

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

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

// registryLogin writes the auth file for the spec's registryAuth and the
// pull credentials luxd minted for the assignment; nil when there are none.
// Registries not listed get no credentials.
func (p *placement) registryLogin(sp spec.RunSpec) (*registryAuth, error) {
	return writeAuthFile(filepath.Join(p.r.cfg.DataDir, "tmp"), sp.Image.RegistryAuth, p.assign.Secrets, p.assign.RegistryCredentials)
}

// writeAuthFile writes auths, then the minted entries for registries the
// spec has no credential for: an explicit registryAuth wins over luxd's.
// Minted keys are <host>/<owner>; spec keys are bare hosts. Podman matches
// keys most specific first (containers-auth.json(5)), so a minted entry
// next to a spec entry for its host would override that entry on the
// owner's repositories (a push credential replaced by a pull-only one):
// the minted entry is dropped instead.
func writeAuthFile(dir string, auths []spec.RegistryAuth, secrets map[string]string, minted []proto.RegistryCredential) (*registryAuth, error) {
	type entry struct {
		Auth string `json:"auth"`
	}
	m := map[string]entry{}
	a := &registryAuth{}
	for _, ra := range auths {
		v := secrets[ra.Secret]
		if v == "" {
			continue
		}
		user, pass := registryCredential(v)
		enc := base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
		m[ra.Registry] = entry{Auth: enc}
		a.secrets = append(a.secrets, v, pass, enc)
	}
	for _, c := range minted {
		host, _, _ := strings.Cut(c.Registry, "/")
		if _, explicit := m[host]; explicit || c.Registry == "" || c.Password == "" {
			continue
		}
		enc := base64.StdEncoding.EncodeToString([]byte(c.User + ":" + c.Password))
		m[c.Registry] = entry{Auth: enc}
		a.secrets = append(a.secrets, c.Password, enc)
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
