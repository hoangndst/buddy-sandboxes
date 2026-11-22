package store

import (
	"regexp"
	"strings"
	"testing"
)

func TestIdentityMigrationReservesNamespacesAcrossRoles(t *testing.T) {
	sql, err := migrationFiles.ReadFile("migrations/001_identity_credentials.sql")
	if err != nil {
		t.Fatal(err)
	}
	statement := regexp.MustCompile(`(?i)create\s+unique\s+index\s+if\s+not\s+exists\s+users_namespace_unique\s+on\s+users\s*\(\s*namespace\s*\)\s+where\s+namespace\s+is\s+not\s+null`).FindString(string(sql))
	if statement == "" {
		t.Fatalf("identity migration must reserve every non-null namespace assignment; migration SQL:\n%s", strings.TrimSpace(string(sql)))
	}
}

func TestCustomProviderEndpointsMigrationAddsColumns(t *testing.T) {
	sql, err := migrationFiles.ReadFile("migrations/002_custom_provider_endpoints.sql")
	if err != nil {
		t.Fatal(err)
	}
	content := string(sql)
	if !strings.Contains(content, "base_url") || !strings.Contains(content, "default_model") || !strings.Contains(content, "auth_type") {
		t.Fatalf("custom provider endpoints migration missing expected columns: %s", content)
	}
}
