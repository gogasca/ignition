package api_test

import (
	"go/build"
	"strings"
	"testing"
)

func TestAPIImportGraphExcludesKubernetes(t *testing.T) {
	forbidden := map[string]struct{}{
		"ignition.dev/ignition/internal/k8s":        {},
		"ignition.dev/ignition/internal/controller": {},
	}
	seen := map[string]struct{}{}
	var walk func(path string)
	walk = func(path string) {
		if _, ok := seen[path]; ok {
			return
		}
		seen[path] = struct{}{}
		pkg, err := build.Default.Import(path, ".", 0)
		if err != nil {
			t.Fatalf("import %s: %v", path, err)
		}
		for _, imp := range pkg.Imports {
			if _, bad := forbidden[imp]; bad {
				t.Fatalf("%s imports forbidden package %s", path, imp)
			}
			if strings.HasPrefix(imp, "ignition.dev/ignition/") {
				walk(imp)
			}
		}
	}
	walk("ignition.dev/ignition/internal/api")
	walk("ignition.dev/ignition/cmd/ignition-api")
}
