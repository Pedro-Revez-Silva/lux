package server

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// registryProviderFiles are the RegistryTokenSource implementations, the
// only registry files that may import a cloud SDK.
var registryProviderFiles = []string{"registry_ecr.go"}

// registryDeliveryFiles carry credentials to runners without a registry*
// name; they are checked along with every registry*.go file.
var registryDeliveryFiles = []string{"hub.go", "server.go"}

// The delivery and minter code compiles without the AWS SDK: no non-test
// file of package server named registry*.go, other than a provider's, nor
// hub.go or server.go, imports github.com/aws/... under any name.
func TestRegistryCoreImportsNoAWS(t *testing.T) {
	for _, name := range append(slices.Clone(registryProviderFiles), registryDeliveryFiles...) {
		if _, err := os.Stat(name); err != nil {
			t.Errorf("%s is named by the import check but is missing: %v", name, err)
		}
	}
	matches, err := filepath.Glob("registry*.go")
	if err != nil {
		t.Fatal(err)
	}
	checked := slices.Clone(registryDeliveryFiles)
	for _, name := range matches {
		if !strings.HasSuffix(name, "_test.go") && !slices.Contains(registryProviderFiles, name) {
			checked = append(checked, name)
		}
	}
	if !slices.Contains(checked, "registry.go") {
		t.Fatalf("registry.go not among the checked files %v", checked)
	}
	for _, name := range checked {
		f, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		if f.Name.Name != "server" {
			continue
		}
		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			if path == "github.com/aws" || strings.HasPrefix(path, "github.com/aws/") {
				t.Errorf("%s imports %s: provider code belongs behind RegistryTokenSource", name, path)
			}
		}
	}
}
