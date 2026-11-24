package credentials

import (
	"context"
	"os"
	"testing"

	"github.com/hoangndst/buddy-sandboxes/internal/store"
)

func newCredentialTestDatabase(t *testing.T) (*store.Postgres, string, *KeySet) {
	t.Helper()
	dsn := os.Getenv("BUDDY_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("BUDDY_TEST_POSTGRES_DSN is required for PostgreSQL integration tests")
	}
	ctx := t.Context()
	db, err := store.Connect(ctx, dsn)
	if err != nil {
		t.Fatal("connect to the test PostgreSQL database")
	}
	t.Cleanup(db.Close)
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	userID, err := randomCredentialID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, "INSERT INTO users(id,display_name,role) VALUES($1,'Credential test','admin')", userID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := db.Pool.Exec(context.WithoutCancel(ctx), "DELETE FROM users WHERE id=$1", userID); err != nil {
			t.Error(err)
		}
	})
	return db, userID, &KeySet{activeVersion: 1, keys: map[int][]byte{1: make([]byte, 32)}}
}

func TestCleanupGitBindingPreservesReplacement(t *testing.T) {
	db, userID, keys := newCredentialTestDatabase(t)
	ctx := t.Context()
	git := NewGitStore(db.Pool, keys)
	const rawURL = "https://github.com/example/test.git"
	const name = "recreated"
	repo, err := ParseGitHubRepository(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	if err := git.Set(ctx, userID, rawURL, ScopeSandbox, name, "test-token"); err != nil {
		t.Fatal(err)
	}
	old, found, err := git.PrepareBinding(ctx, userID, userID, name, "first", repo)
	if err != nil || !found {
		t.Fatalf("prepare original binding: found=%t, err=%v", found, err)
	}
	if err := git.Bind(ctx, old.ID, "old-uid"); err != nil {
		t.Fatal(err)
	}

	replacement, found, err := git.PrepareBinding(ctx, userID, userID, name, "second", repo)
	if err != nil || !found || replacement.CredentialID != old.CredentialID {
		t.Fatalf("prepare replacement binding for the same credential: found=%t, err=%v", found, err)
	}
	if err := git.CleanupBinding(ctx, old.ID, userID, name, "old-uid", "first"); err != nil {
		t.Fatal(err)
	}
	if err := git.Bind(ctx, replacement.ID, "new-uid"); err != nil {
		t.Fatalf("bind replacement after cleanup: %v", err)
	}
	remaining, err := git.List(ctx, userID)
	if err != nil || len(remaining) != 1 {
		t.Fatalf("list credentials after original cleanup: count=%d, err=%v", len(remaining), err)
	}
	if err := git.CleanupBinding(ctx, replacement.ID, userID, name, "new-uid", "second"); err != nil {
		t.Fatal(err)
	}
	remaining, err = git.List(ctx, userID)
	if err != nil || len(remaining) != 0 {
		t.Fatalf("list credentials after final cleanup: count=%d, err=%v", len(remaining), err)
	}
}

func TestStoreProviderCredentialOptionsPostgres(t *testing.T) {
	db, userID, keys := newCredentialTestDatabase(t)
	providers := NewStore(db.Pool, keys)
	for _, tc := range []struct{ scope, sandbox string }{
		{scope: ScopeUser},
		{scope: ScopeSandbox, sandbox: "workspace"},
	} {
		t.Run(tc.scope, func(t *testing.T) {
			ctx := t.Context()
			if err := providers.Set(ctx, userID, ServiceOpenAI, tc.scope, tc.sandbox, "initial-token"); err != nil {
				t.Fatalf("create credential without options: %v", err)
			}
			initial, err := providers.GetRecordForSandbox(ctx, userID, ServiceOpenAI, tc.sandbox)
			if want := (Record{Key: "initial-token"}); err != nil || initial != want {
				t.Fatalf("read credential without options: %v", err)
			}
			opts := Options{BaseURL: "https://api.openai.com/v1", DefaultModel: "test-model", AuthType: "bearer"}
			if err := providers.Set(ctx, userID, ServiceOpenAI, tc.scope, tc.sandbox, "updated-token", opts); err != nil {
				t.Fatalf("update credential with options: %v", err)
			}
			want := Record{Key: "updated-token", BaseURL: opts.BaseURL, DefaultModel: opts.DefaultModel, AuthType: opts.AuthType}
			updated, err := providers.GetRecordForSandbox(ctx, userID, ServiceOpenAI, tc.sandbox)
			if err != nil || updated != want {
				t.Fatalf("read updated credential with options: %v", err)
			}
			if err := providers.Set(ctx, userID, ServiceOpenAI, tc.scope, tc.sandbox, "rotated-token"); err != nil {
				t.Fatalf("rotate credential without options: %v", err)
			}
			want.Key = "rotated-token"
			rotated, err := providers.GetRecordForSandbox(ctx, userID, ServiceOpenAI, tc.sandbox)
			if err != nil || rotated != want {
				t.Fatalf("read rotated credential with retained options: %v", err)
			}
		})
	}
}
