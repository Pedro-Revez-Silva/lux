package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/marcioapm/lux/internal/server"
)

const goodRegistry = `
[registry]
provider = "ecr"
host = "123456789012.dkr.ecr.eu-west-1.amazonaws.com"
[registry.ecr]
region = "eu-west-1"
pull_role_arn = "arn:aws:iam::123456789012:role/lux-registry-pull"
[[registry.grants]]
owner = "acme"
[[registry.grants]]
owner = "team.b"
tenants = ["ten_1", "ten_2"]
`

var registryEnv = []string{"LUX_REGISTRY_PROVIDER", "LUX_REGISTRY_HOST", "LUX_REGISTRY_ECR_REGION", "LUX_REGISTRY_ECR_PULL_ROLE_ARN",
	"LUX_REGISTRY_GRANTS", "LUX_REGISTRY_REGION", "LUX_REGISTRY_PULL_ROLE_ARN"}

func registryEnvClear(t *testing.T) {
	t.Helper()
	for _, k := range registryEnv {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
}

// [registry]: absent is off; a good section reaches the server with its
// provider's token source and tenants defaulting to every tenant; the
// environment overrides the file.
func TestRegistryConfig(t *testing.T) {
	registryEnvClear(t)
	path := filepath.Join(t.TempDir(), "luxd.toml")
	write := func(s string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("")
	c, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.registryConfig(); !reflect.DeepEqual(got, server.RegistryConfig{}) {
		t.Fatalf("absent section: %+v, want off", got)
	}

	write(goodRegistry)
	if c, err = loadConfig(path); err != nil {
		t.Fatal(err)
	}
	want := server.RegistryConfig{Host: "123456789012.dkr.ecr.eu-west-1.amazonaws.com",
		Grants: []server.RegistryGrant{{Owner: "acme", Tenants: []string{"*"}}, {Owner: "team.b", Tenants: []string{"ten_1", "ten_2"}}},
		Source: server.NewECRSource(server.ECRConfig{Region: "eu-west-1", PullRoleARN: "arn:aws:iam::123456789012:role/lux-registry-pull"})}
	if got := c.registryConfig(); !reflect.DeepEqual(got, want) {
		t.Fatalf("file:\n%+v\nwant\n%+v", got, want)
	}

	t.Setenv("LUX_REGISTRY_HOST", "210987654321.dkr.ecr.us-east-2.amazonaws.com")
	t.Setenv("LUX_REGISTRY_ECR_REGION", "us-east-2")
	t.Setenv("LUX_REGISTRY_ECR_PULL_ROLE_ARN", "arn:aws-us-gov:iam::210987654321:role/other")
	t.Setenv("LUX_REGISTRY_GRANTS", `[{"owner":"envowner","tenants":["ten_9"]}]`)
	if c, err = loadConfig(path); err != nil {
		t.Fatal(err)
	}
	want = server.RegistryConfig{Host: "210987654321.dkr.ecr.us-east-2.amazonaws.com",
		Grants: []server.RegistryGrant{{Owner: "envowner", Tenants: []string{"ten_9"}}},
		Source: server.NewECRSource(server.ECRConfig{Region: "us-east-2", PullRoleARN: "arn:aws-us-gov:iam::210987654321:role/other"})}
	if got := c.registryConfig(); !reflect.DeepEqual(got, want) {
		t.Fatalf("env over file:\n%+v\nwant\n%+v", got, want)
	}

	// The environment alone turns it on.
	write("")
	t.Setenv("LUX_REGISTRY_PROVIDER", "ecr")
	if c, err = loadConfig(path); err != nil {
		t.Fatal(err)
	}
	if got := c.registryConfig(); !reflect.DeepEqual(got, want) {
		t.Fatalf("env only:\n%+v\nwant\n%+v", got, want)
	}
}

// Each invalid setting is refused at startup, naming it.
func TestRegistryConfigInvalid(t *testing.T) {
	registryEnvClear(t)
	path := filepath.Join(t.TempDir(), "luxd.toml")
	sub := func(old, new string) string {
		if !strings.Contains(goodRegistry, old) {
			t.Fatalf("fixture lacks %q", old)
		}
		return strings.Replace(goodRegistry, old, new, 1)
	}
	noECR := sub("[registry.ecr]\nregion = \"eu-west-1\"\npull_role_arn = \"arn:aws:iam::123456789012:role/lux-registry-pull\"\n", "")
	for name, bad := range map[string]struct{ file, env, want string }{
		// The provider.
		"provider missing":   {sub(`provider = "ecr"`, ``), "", "registry.provider (LUX_REGISTRY_PROVIDER) is required"},
		"provider unknown":   {sub(`provider = "ecr"`, `provider = "ghcr"`), "", `registry.provider (LUX_REGISTRY_PROVIDER) "ghcr": unknown; want one of ecr`},
		"env provider":       {goodRegistry, "LUX_REGISTRY_PROVIDER=gar", `registry.provider (LUX_REGISTRY_PROVIDER) "gar": unknown`},
		"ecr section absent": {noECR, "", "registry.ecr is required for provider ecr"},
		"ecr section extra":  {sub(`provider = "ecr"`, `provider = "acr"`), "", `registry.ecr (LUX_REGISTRY_ECR_*) is set, but registry.provider is "acr"`},
		"ecr only, no provider": {"[registry.ecr]\nregion = \"eu-west-1\"\n", "",
			"registry.provider (LUX_REGISTRY_PROVIDER) is required"},
		"other provider section": {goodRegistry + "[registry.ghcr]\napp_id = 1\n", "", "registry.ghcr"},
		// Keys that moved under [registry.ecr]: refused, with their new place.
		"flat region": {sub("host = \"123456789012.dkr.ecr.eu-west-1.amazonaws.com\"\n", "host = \"123456789012.dkr.ecr.eu-west-1.amazonaws.com\"\nregion = \"eu-west-1\"\n"), "",
			"registry.region is not read: set registry.ecr.region (LUX_REGISTRY_ECR_REGION) instead"},
		"flat role": {sub("host = \"123456789012.dkr.ecr.eu-west-1.amazonaws.com\"\n", "host = \"123456789012.dkr.ecr.eu-west-1.amazonaws.com\"\npull_role_arn = \"arn:aws:iam::123456789012:role/r\"\n"), "",
			"registry.pull_role_arn is not read: set registry.ecr.pull_role_arn (LUX_REGISTRY_ECR_PULL_ROLE_ARN) instead"},
		"flat env region": {goodRegistry, "LUX_REGISTRY_REGION=eu-west-1", "LUX_REGISTRY_REGION is not read: set registry.ecr.region (LUX_REGISTRY_ECR_REGION) instead"},
		"flat env role": {goodRegistry, "LUX_REGISTRY_PULL_ROLE_ARN=arn:aws:iam::123456789012:role/r",
			"LUX_REGISTRY_PULL_ROLE_ARN is not read: set registry.ecr.pull_role_arn (LUX_REGISTRY_ECR_PULL_ROLE_ARN) instead"},
		// The host, generic then ECR's.
		"host with scheme":   {sub(`host = "123456789012.`, `host = "https://123456789012.`), "", "registry.host (LUX_REGISTRY_HOST) \"https://"},
		"host with path":     {sub(`eu-west-1.amazonaws.com"`, `eu-west-1.amazonaws.com/acme"`), "", "registry.host"},
		"host not ECR":       {sub(`host = "123456789012.dkr.ecr.eu-west-1.amazonaws.com"`, `host = "ghcr.io"`), "", "provider ecr wants <account>.dkr.ecr.<region>.amazonaws.com"},
		"short account":      {sub(`host = "123456789012.`, `host = "12345.`), "", "registry.host"},
		"no host":            {sub("host = \"123456789012.dkr.ecr.eu-west-1.amazonaws.com\"\n", ""), "", "registry.host (LUX_REGISTRY_HOST) is required"},
		"region missing":     {sub(`region = "eu-west-1"`, ``), "", "registry.ecr.region (LUX_REGISTRY_ECR_REGION) is required"},
		"region mismatch":    {sub(`region = "eu-west-1"`, `region = "us-east-1"`), "", "registry.ecr.region (LUX_REGISTRY_ECR_REGION) \"us-east-1\": registry.host is in eu-west-1"},
		"env region":         {goodRegistry, "LUX_REGISTRY_ECR_REGION=us-east-1", "registry.ecr.region (LUX_REGISTRY_ECR_REGION) \"us-east-1\""},
		"arn shape":          {sub(`arn:aws:iam::123456789012:role/lux-registry-pull`, `lux-registry-pull`), "", "registry.ecr.pull_role_arn"},
		"arn not a role":     {sub(`:role/lux-registry-pull`, `:user/lux`), "", "registry.ecr.pull_role_arn"},
		"env arn":            {goodRegistry, "LUX_REGISTRY_ECR_PULL_ROLE_ARN=nope", "registry.ecr.pull_role_arn (LUX_REGISTRY_ECR_PULL_ROLE_ARN) \"nope\""},
		"owner uppercase":    {sub(`owner = "acme"`, `owner = "Acme"`), "", "registry.grants[0].owner"},
		"owner slash":        {sub(`owner = "acme"`, `owner = "acme/x"`), "", "registry.grants[0].owner"},
		"owner empty":        {sub(`owner = "acme"`, `owner = ""`), "", "registry.grants[0].owner"},
		"owner too long":     {sub(`owner = "acme"`, `owner = "`+strings.Repeat("a", 51)+`"`), "", "registry.grants[0].owner \"" + strings.Repeat("a", 51) + "\": provider ecr allows at most 50"},
		"duplicate owner":    {sub(`owner = "team.b"`, `owner = "acme"`), "", `registry.grants[1].owner "acme": duplicate`},
		"empty tenants":      {sub(`tenants = ["ten_1", "ten_2"]`, `tenants = []`), "", "registry.grants[1].tenants"},
		"blank tenant":       {sub(`tenants = ["ten_1", "ten_2"]`, `tenants = [" "]`), "", "registry.grants[1].tenants"},
		"no grants":          {strings.Split(goodRegistry, "[[registry.grants]]")[0], "", "registry.grants"},
		"unknown key":        {sub(`provider = "ecr"`, "provider = \"ecr\"\nrole = \"x\""), "", "registry.role"},
		"env host":           {goodRegistry, "LUX_REGISTRY_HOST=quay.io", "registry.host"},
		"env grants json":    {goodRegistry, "LUX_REGISTRY_GRANTS=acme", "LUX_REGISTRY_GRANTS"},
		"env grants unknown": {goodRegistry, `LUX_REGISTRY_GRANTS=[{"owner":"a","team":"x"}]`, "LUX_REGISTRY_GRANTS"},
		"env grants owner":   {goodRegistry, `LUX_REGISTRY_GRANTS=[{"owner":"A B"}]`, "registry.grants[0].owner"},
	} {
		t.Run(name, func(t *testing.T) {
			registryEnvClear(t)
			if err := os.WriteFile(path, []byte(bad.file), 0o600); err != nil {
				t.Fatal(err)
			}
			if bad.env != "" {
				k, v, _ := strings.Cut(bad.env, "=")
				t.Setenv(k, v)
			}
			if _, err := loadConfig(path); err == nil || !strings.Contains(err.Error(), bad.want) {
				t.Fatalf("error %v, want it to name %q", err, bad.want)
			}
		})
	}
}

// The host check is generic: any registry host[:port] passes it, so only
// the provider's own check refuses a non-ECR host for provider ecr.
func TestRegistryHostGeneric(t *testing.T) {
	registryEnvClear(t)
	path := filepath.Join(t.TempDir(), "luxd.toml")
	for host, generic := range map[string]bool{
		"ghcr.io": true, "registry.example.com:5000": true, "10.0.0.5:5000": true,
		"https://ghcr.io": false, "ghcr.io/acme": false, "GHCR.IO": false, "localhost:5000": false,
	} {
		file := strings.Replace(goodRegistry, `host = "123456789012.dkr.ecr.eu-west-1.amazonaws.com"`, `host = "`+host+`"`, 1)
		if err := os.WriteFile(path, []byte(file), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := loadConfig(path)
		if err == nil {
			t.Fatalf("%s accepted for provider ecr", host)
		}
		genericErr := strings.Contains(err.Error(), "want a registry host with an optional port")
		ecrErr := strings.Contains(err.Error(), "provider ecr wants")
		if genericErr == generic || !ecrErr {
			t.Errorf("%s: %v; want the generic check to %s and ECR's to refuse", host, err, map[bool]string{true: "pass", false: "refuse"}[generic])
		}
	}
}
