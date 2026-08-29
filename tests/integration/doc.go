// Package integration_test is the hermetic API + controller path:
// one Memory store, HTTP/JSON, and k8s.Fake. It does not talk to GKE or Cloud SQL.
//
// Live cluster conformance belongs in tests/conformance once a shared SQL
// backend and typed Kubernetes client exist.
package integration_test
