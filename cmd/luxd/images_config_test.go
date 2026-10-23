package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// [images] auth = "ecr": an ECR registry, no file or variable credential,
// a minimum validity an ECR token (12h) can meet, shared repositories on
// the same registry; checked before anything connects (validate calls no
// AWS API: there is none to call here).
func TestImagesECRConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "luxd.toml")
	load := func(file string) (config, error) {
		t.Helper()
		if err := os.WriteFile(path, []byte(file), 0o600); err != nil {
			t.Fatal(err)
		}
		return loadConfig(path)
	}
	const repo = "123456789012.dkr.ecr.eu-north-1.amazonaws.com/lux/managed"
	c, err := load("[images]\nrepository = \"" + repo + "\"\nauth = \"ecr\"\nshared = [\"123456789012.dkr.ecr.eu-north-1.amazonaws.com/lux/runtime\"]\n")
	if err != nil {
		t.Fatal(err)
	}
	if c.Images.Auth != "ecr" || c.Images.ECRMinValidity.Duration != 4*time.Hour {
		t.Fatalf("%+v", c.Images)
	}
	t.Setenv("LUX_IMAGES_ECR_MIN_VALIDITY", "5h")
	if c, err := load("[images]\nrepository = \"" + repo + "\"\nauth = \"ecr\"\n"); err != nil || c.Images.ECRMinValidity.Duration != 5*time.Hour {
		t.Fatalf("env: %v %+v", err, c.Images)
	}
	t.Setenv("LUX_IMAGES_ECR_MIN_VALIDITY", "")
	for _, tc := range []struct{ file, want string }{
		{"[images]\nrepository = \"ghcr.io/acme/lux\"\nauth = \"ecr\"\n", "private ECR registry"},
		{"[images]\nrepository = \"123456789012.dkr.ecr.eu-north-1.amazonaws.com.evil.io/x\"\nauth = \"ecr\"\n", "private ECR registry"},
		{"[images]\nrepository = \"" + repo + "\"\nauth = \"ecr\"\ncredential_file = \"/etc/lux/cred\"\n", "takes no images.credential_file"},
		{"[images]\nrepository = \"" + repo + "\"\nauth = \"ecr\"\ncredential_env = \"X\"\n", "takes no images.credential_file"},
		{"[images]\nrepository = \"" + repo + "\"\nauth = \"ecr\"\necr_min_validity = \"12h\"\n", "ecr_min_validity"},
		{"[images]\nrepository = \"" + repo + "\"\nauth = \"ecr\"\necr_min_validity = \"5m\"\n", "ecr_min_validity"},
		{"[images]\nrepository = \"" + repo + "\"\nauth = \"ecr\"\nshared = [\"999999999999.dkr.ecr.eu-north-1.amazonaws.com/base\"]\n", "on the registry of images.repository"},
		{"[images]\nrepository = \"" + repo + "\"\nauth = \"ecr\"\nshared = [\"" + repo + "/x\"]\n", "inside images.repository"},
		{"[images]\nauth = \"ecr\"\n", "images.repository (LUX_IMAGES_REPOSITORY) is required"},
		{"[images]\nrepository = \"" + repo + "\"\nauth = \"sso\"\n", `must be "static" or "ecr"`},
		{"[images]\nrepository = \"" + repo + "\"\nauth = \"static\"\n", "needs images.credential_file"},
	} {
		if _, err := load(tc.file); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: got %v, want %q", tc.file, err, tc.want)
		}
	}
	// Static stays as it was.
	if _, err := load("[images]\nrepository = \"ghcr.io/acme/lux\"\ncredential_file = \"/etc/lux/cred\"\n"); err != nil {
		t.Fatal(err)
	}
}
