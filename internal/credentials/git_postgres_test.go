package credentials

import (
	"os"
	"testing"

	"github.com/hoangndst/buddy-sandboxes/internal/store"
)

func TestCleanupGitBindingPreservesReplacement(t *testing.T) {
	dsn := os.Getenv("BUDDY_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("BUDDY_TEST_POSTGRES_DSN is required for PostgreSQL integration tests")
	}
	ctx := t.Context()
	db, err := store.Connect(ctx, dsn)
	if err != nil {
		t.Fatal("connect to the test PostgreSQL database")
	}
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	userID, err := randomCredentialID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, "INSERT INTO users(id,display_name,role) VALUES($1,'Cleanup test','admin')", userID); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := db.Pool.Exec(ctx, "DELETE FROM users WHERE id=$1", userID); err != nil {
			t.Error(err)
		}
	}()

	git := NewGitStore(db.Pool, &KeySet{activeVersion: 1, keys: map[int][]byte{1: make([]byte, 32)}})
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
