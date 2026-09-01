package store

import (
	"strings"
	"testing"
)

func TestSchemaIsCompleteBaseline(t *testing.T) {
	upper := strings.ToUpper(schemaSQL)
	for _, forbidden := range []string{"-- +GOOSE", "ALTER TABLE", "DROP TABLE"} {
		if strings.Contains(upper, forbidden) {
			t.Errorf("schema contains migration-only SQL %q", forbidden)
		}
	}

	statements := splitSQL(schemaSQL)
	if len(statements) == 0 {
		t.Fatal("embedded schema has no statements")
	}
	for _, table := range []string{
		"projects", "role_bindings", "images", "sandboxes", "operations",
		"processes", "idempotency_keys", "project_quota", "controller_leases",
	} {
		needle := "CREATE TABLE IF NOT EXISTS " + strings.ToUpper(table)
		if !strings.Contains(upper, needle) {
			t.Errorf("schema does not define %s", table)
		}
	}
}
