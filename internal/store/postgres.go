package store

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

const migrationLockID int64 = 0x4255444459534258

// Postgres owns the shared identity and credential state used by API replicas.
type Postgres struct {
	Pool *pgxpool.Pool
}

// Connect opens a PostgreSQL connection pool and verifies the connection.
func Connect(ctx context.Context, connectionString string) (*Postgres, error) {
	if strings.TrimSpace(connectionString) == "" {
		return nil, fmt.Errorf("database connection string is required")
	}
	config, err := pgxpool.ParseConfig(connectionString)
	if err != nil {
		return nil, fmt.Errorf("parse database connection string: %w", err)
	}
	config.MaxConns = 12
	config.MinConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("open PostgreSQL pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect to PostgreSQL: %w", err)
	}
	return &Postgres{Pool: pool}, nil
}

// Close releases the connection pool.
func (p *Postgres) Close() {
	if p != nil && p.Pool != nil {
		p.Pool.Close()
	}
}

// Ready reports whether PostgreSQL is currently reachable.
func (p *Postgres) Ready(ctx context.Context) error {
	if p == nil || p.Pool == nil {
		return fmt.Errorf("PostgreSQL is not configured")
	}
	return p.Pool.Ping(ctx)
}

// RequireCurrentSchema rejects startup until the one-shot migration job has completed.
func (p *Postgres) RequireCurrentSchema(ctx context.Context) error {
	var applied bool
	err := p.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version='007_execution_diff')`).Scan(&applied)
	if err != nil {
		return fmt.Errorf("check identity schema version: %w", err)
	}
	if !applied {
		return fmt.Errorf("identity schema is not current; run buddy-api migrate")
	}
	return nil
}

// Migrate applies embedded, versioned SQL migrations under a PostgreSQL advisory lock.
// It is intended for a one-shot migration job, not API startup.
func (p *Postgres) Migrate(ctx context.Context) error {
	conn, err := p.Pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire migration connection: %w", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrationLockID); err != nil {
		return fmt.Errorf("lock schema migrations: %w", err)
	}
	defer func() { _, _ = conn.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", migrationLockID) }()
	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version text PRIMARY KEY,
		applied_at timestamptz NOT NULL DEFAULT now()
	)`); err != nil {
		return fmt.Errorf("create schema migration table: %w", err)
	}
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		return fmt.Errorf("list schema migrations: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		version := strings.TrimSuffix(entry.Name(), ".sql")
		var applied bool
		if err := conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version=$1)", version).Scan(&applied); err != nil {
			return fmt.Errorf("check migration %s: %w", version, err)
		}
		if applied {
			continue
		}
		sql, err := migrationFiles.ReadFile("migrations/" + entry.Name())
		if err != nil {
			return fmt.Errorf("read migration %s: %w", version, err)
		}
		tx, err := conn.BeginTx(ctx, pgx.TxOptions{})
		if err != nil {
			return fmt.Errorf("begin migration %s: %w", version, err)
		}
		if _, err = tx.Exec(ctx, string(sql)); err == nil {
			_, err = tx.Exec(ctx, "INSERT INTO schema_migrations(version) VALUES ($1)", version)
		}
		if err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("apply migration %s: %w", version, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit migration %s: %w", version, err)
		}
	}
	return nil
}
