package prpaths

import (
	"go/build"
	"sort"
	"strings"
	"testing"
)

const modulePrefix = "ignition.dev/ignition/"

// internalDeps walks the transitive import graph of an import path and returns
// every ignition.dev/ignition/internal/<pkg> package it reaches. Same technique
// as internal/api/import_graph_test.go.
func internalDeps(t *testing.T, root string) map[string]struct{} {
	t.Helper()
	deps := map[string]struct{}{}
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
			if !strings.HasPrefix(imp, modulePrefix) {
				continue
			}
			if rel := strings.TrimPrefix(imp, modulePrefix); strings.HasPrefix(rel, "internal/") {
				deps[rel] = struct{}{}
			}
			walk(imp)
		}
	}
	walk(root)
	return deps
}

func TestComponentsCoverImportGraph(t *testing.T) {
	// The map key <-> cmd binary mapping. "gateway" is the data plane.
	binaries := map[string]string{
		"api":        "cmd/ignition-api",
		"controller": "cmd/ignition-controller",
		"gateway":    "cmd/ignition-gateway",
	}

	for component, cmdDir := range binaries {
		globs := Components[component]
		if globs == nil {
			t.Fatalf("prpaths.Components has no entry for %q", component)
		}
		have := map[string]struct{}{}
		for _, g := range globs {
			have[g] = struct{}{}
		}

		// Required: the binary's own folder, and the module manifests.
		var missing []string
		for _, req := range []string{cmdDir + "/**", "go.mod", "go.sum"} {
			if _, ok := have[req]; !ok {
				missing = append(missing, req)
			}
		}
		// Required: every internal package the binary compiles in.
		for dep := range internalDeps(t, modulePrefix+cmdDir) {
			glob := dep + "/**" // e.g. internal/store/**
			if _, ok := have[glob]; !ok {
				missing = append(missing, glob)
			}
		}

		if len(missing) > 0 {
			sort.Strings(missing)
			t.Errorf("prpaths.Components[%q] is missing globs (a dependency changed?):\n  %s",
				component, strings.Join(missing, "\n  "))
		}
	}
}

func TestComponentsHaveNoUnknownComponent(t *testing.T) {
	known := map[string]bool{"api": true, "controller": true, "gateway": true}
	for c := range Components {
		if !known[c] {
			t.Errorf("prpaths.Components has unexpected component %q", c)
		}
	}
}

func TestIncludedFiles(t *testing.T) {
	got := IncludedFiles("api")
	if !strings.Contains(got, "cmd/ignition-api/**,internal/api/**,") {
		t.Fatalf("IncludedFiles(api) = %q", got)
	}
	if strings.HasPrefix(got, ",") || strings.HasSuffix(got, ",") {
		t.Fatalf("IncludedFiles has a stray comma: %q", got)
	}
	if IncludedFiles("nope") != "" {
		t.Fatalf("IncludedFiles(unknown) should be empty")
	}
}
