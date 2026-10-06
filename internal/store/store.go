// Package store owns the engine's Postgres schema and connection handling.
//
// Migrations are embedded in the binary so a self-hoster runs one command and
// gets a working database, with no separate migration tool to install and no
// way for the schema to drift from the code that expects it.
package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
	"github.com/pressly/goose/v3"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// migrationsDir is the path inside migrationsFS.
const migrationsDir = "migrations"

// migrationLock serialises migrators against one database.
//
// Two instances starting together both migrate at boot, and goose is not safe
// to run twice at once: the second finds a table the first has just created and
// fails with "already exists", which reads as a broken migration rather than as
// a race. An arbitrary constant, shared by every Ozymandis that talks to this
// database.
const migrationLock int64 = 0x796163687421

// goose keeps its dialect and filesystem in package-level state, so setting it
// once is both correct and the only way concurrent migrators avoid writing to
// the same variables at the same time.
var (
	gooseOnce sync.Once
	gooseErr  error
)

// Connect opens a pooled connection and verifies the database answers.
func Connect(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("store: parse dsn: %w", err)
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("store: connect: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: ping: %w", err)
	}
	return pool, nil
}

// Migrate applies any pending migrations.
//
// Uses database/sql rather than the pgx pool because that is what goose
// expects; the connection is short-lived and closed before returning.
func Migrate(ctx context.Context, dsn string, log *slog.Logger) error {
	if log == nil {
		log = slog.Default()
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("store: open for migration: %w", err)
	}
	defer db.Close()

	gooseOnce.Do(func() {
		goose.SetBaseFS(migrationsFS)
		goose.SetLogger(goose.NopLogger())
		gooseErr = goose.SetDialect("postgres")
	})
	if gooseErr != nil {
		return fmt.Errorf("store: set dialect: %w", gooseErr)
	}

	// Held on its own connection for the length of the migration. An advisory
	// lock is session-scoped, so a second migrator blocks here rather than
	// racing through and failing on a table this one is midway through
	// creating. Closing the connection would release it even if the unlock
	// below never ran.
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("store: open migration lock connection: %w", err)
	}
	defer conn.Close() //nolint:errcheck // releasing the lock is the point

	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock($1)", migrationLock); err != nil {
		return fmt.Errorf("store: take migration lock: %w", err)
	}
	defer func() {
		// WithoutCancel so a cancelled migration still gives the lock back
		// rather than leaving the next instance waiting on a dead session.
		_, _ = conn.ExecContext(
			context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", migrationLock)
	}()

	before, _ := goose.GetDBVersionContext(ctx, db)
	warnIfSchemaIsNewer(before, log)
	if err := goose.UpContext(ctx, db, migrationsDir); err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}
	after, err := goose.GetDBVersionContext(ctx, db)
	if err != nil {
		return fmt.Errorf("store: read schema version: %w", err)
	}

	if before == after {
		log.Debug("schema up to date", slog.Int64("version", after))
	} else {
		log.Info("schema migrated",
			slog.Int64("from", before),
			slog.Int64("to", after),
		)
	}
	return nil
}

// warnIfSchemaIsNewer says so when the database has been migrated past
// anything this binary knows about.
//
// That is what a rollback leaves behind: the newer release migrated, the
// previous binary was put back, and goose — finding nothing it has not already
// applied — starts it without a word on a schema it was not written for.
// Queries then fail one at a time at request time, which looks like anything
// but its cause. Not refused, because most migrations only add and the older
// binary runs fine on them; but it is the first thing to know when something
// odd follows a rollback, so it is said at error level, once, at startup.
func warnIfSchemaIsNewer(dbVersion int64, log *slog.Logger) {
	known, err := goose.CollectMigrations(migrationsDir, 0, goose.MaxVersion)
	if err != nil || len(known) == 0 {
		return
	}
	newest := known[len(known)-1].Version
	if dbVersion > newest {
		log.Error("the database schema is newer than this binary — it was migrated "+
			"by a later release, and this one may fail on tables it does not "+
			"expect. Upgrade again, or restore the dump upgrade.sh took",
			slog.Int64("database", dbVersion), slog.Int64("binary", newest))
	}
}
