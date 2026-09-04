// Package prpaths is the single source of truth for the per-component PR-build
// path filters used by the Cloud Build triggers in deploy/PIPELINE.md.
//
// A PR trigger for a component fires only when a changed file matches one of its
// globs, so each container is rebuilt only when its own code — or a package it
// compiles in — changes. paths_test.go recomputes the real transitive import
// graph and fails CI if a dependency is not covered here.
package prpaths

// Components maps each buildable control-plane container to the repo-relative
// path globs (Cloud Build `includedFiles` syntax) that must retrigger its PR
// build. Over-listing is safe (an extra glob just causes an occasional
// unnecessary build); under-listing is a bug the test catches.
var Components = map[string][]string{
	"api": {
		"cmd/ignition-api/**",
		"internal/api/**",
		"internal/adminz/**",
		"internal/auth/**",
		"internal/config/**",
		"internal/store/**",
		"internal/imagecatalog/**",
		"internal/id/**",
		"go.mod",
		"go.sum",
		"deploy/docker/ignition-api.Dockerfile",
		"deploy/cloudbuild/pr.yaml",
		"deploy/cloudbuild/prpaths/**",
	},
	"controller": {
		"cmd/ignition-controller/**",
		"internal/controller/**",
		"internal/adminz/**",
		"internal/k8s/**",
		"internal/gpuid/**",
		"internal/capacity/**",
		"internal/secrets/**",
		"internal/config/**",
		"internal/store/**",
		"internal/id/**",
		"go.mod",
		"go.sum",
		"deploy/docker/ignition-controller.Dockerfile",
		"deploy/cloudbuild/pr.yaml",
		"deploy/cloudbuild/prpaths/**",
	},
	"gateway": {
		"cmd/ignition-gateway/**",
		"internal/gateway/**",
		"internal/config/**",
		"internal/store/**",
		"internal/id/**",
		"go.mod",
		"go.sum",
		"deploy/docker/ignition-gateway.Dockerfile",
		"deploy/cloudbuild/pr.yaml",
		"deploy/cloudbuild/prpaths/**",
	},
}

// IncludedFiles returns the comma-joined glob list for a component, ready to
// pass to `gcloud builds triggers create --included-files=`.
func IncludedFiles(component string) string {
	globs := Components[component]
	out := ""
	for i, g := range globs {
		if i > 0 {
			out += ","
		}
		out += g
	}
	return out
}
