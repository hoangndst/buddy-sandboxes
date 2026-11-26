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

func TestServiceCredentialsMigrationPreservesProviderRecords(t *testing.T) {
	sql, err := migrationFiles.ReadFile("migrations/003_service_credentials.sql")
	if err != nil {
		t.Fatal(err)
	}
	content := string(sql)
	for _, required := range []string{"service_credentials", "git_sandbox_bindings", "git_clone_claims", "host = 'github.com'", "scope IN ('user', 'sandbox')", "outcome IN ('claimed', 'failed', 'succeeded')", "FROM provider_credentials", "ON CONFLICT (id) DO NOTHING"} {
		if !strings.Contains(content, required) {
			t.Errorf("Git credentials migration missing %q", required)
		}
	}
	if strings.Contains(content, "git_credentials (") {
		t.Fatal("GitHub credential values must use shared service credential storage")
	}
}

func TestSessionsMigrationDefinesSessionTableAndIndexes(t *testing.T) {
	sql, err := migrationFiles.ReadFile("migrations/004_sessions.sql")
	if err != nil {
		t.Fatal(err)
	}
	content := string(sql)
	for _, required := range []string{
		"sandbox_sessions",
		"user_id text NOT NULL REFERENCES users(id) ON DELETE CASCADE",
		"state text NOT NULL CHECK (state IN ('pending', 'running', 'completed', 'failed', 'terminated'))",
		"sandbox_sessions_sandbox_idx",
		"sandbox_sessions_user_idx",
		"sandbox_sessions_active_idx",
	} {
		if !strings.Contains(content, required) {
			t.Errorf("Sessions migration missing %q", required)
		}
	}
}

func TestTasksExecutionsMigrationDefinesTablesAndMutexIndexes(t *testing.T) {
	sql, err := migrationFiles.ReadFile("migrations/005_tasks_executions.sql")
	if err != nil {
		t.Fatal(err)
	}
	content := string(sql)
	for _, required := range []string{
		"agent_profiles",
		"tasks",
		"executions",
		"user_id text NOT NULL REFERENCES users(id) ON DELETE CASCADE",
		"task_id text NOT NULL REFERENCES tasks(id) ON DELETE CASCADE",
		"status text NOT NULL CHECK (status IN ('pending', 'running', 'succeeded', 'failed', 'cancelled'))",
		"status text NOT NULL CHECK (status IN ('scheduled', 'running', 'verifying', 'succeeded', 'failed', 'cancelled'))",
		"executions_active_workspace_idx",
		"executions_claim_idx",
		"executions_heartbeat_idx",
		"tasks_user_idx",
		"tasks_workspace_status_idx",
	} {
		if !strings.Contains(content, required) {
			t.Errorf("Tasks/Executions migration missing %q", required)
		}
	}
}

func TestProviderOptionsSyncMigrationPreservesMergedOptions(t *testing.T) {
	sql, err := migrationFiles.ReadFile("migrations/006_provider_options_sync.sql")
	if err != nil {
		t.Fatal(err)
	}
	content := string(sql)
	for _, required := range []string{
		"CREATE OR REPLACE FUNCTION sync_legacy_provider_credential()",
		"options=service_credentials.options || EXCLUDED.options",
	} {
		if !strings.Contains(content, required) {
			t.Errorf("Provider options sync migration missing %q", required)
		}
	}
}
