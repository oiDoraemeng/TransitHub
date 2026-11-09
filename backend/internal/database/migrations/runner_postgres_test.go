package migrations

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestRunAppliesAllMigrationsOnEmptySchema 在独立的临时 schema 中从零执行全部迁移（需要 TEST_DATABASE_URL），
// 并确认重复执行是幂等的。
func migrationTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := strings.TrimSpace(os.Getenv("TEST_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is required for PostgreSQL migration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	adminPool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("connect PostgreSQL: %v", err)
	}
	schema := fmt.Sprintf("migrations_test_%d", time.Now().UnixNano())
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err := adminPool.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		if _, err := adminPool.Exec(context.Background(), "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Errorf("drop schema: %v", err)
		}
		adminPool.Close()
	})
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("connect schema: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestRunAppliesAllMigrationsOnEmptySchema(t *testing.T) {
	pool := migrationTestPool(t)
	ctx := context.Background()

	if err := Run(ctx, pool); err != nil {
		t.Fatalf("run migrations: %v", err)
	}
	if err := Run(ctx, pool); err != nil {
		t.Fatalf("second run must be a no-op: %v", err)
	}
	entries, err := migrationFiles.ReadDir(".")
	if err != nil {
		t.Fatalf("read embedded migrations: %v", err)
	}
	want := 0
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".sql") {
			want++
		}
	}
	var applied int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM schema_migrations").Scan(&applied); err != nil || applied != want {
		t.Fatalf("applied migrations = %d, want %d (%v)", applied, want, err)
	}
}

func TestRunUpgradesCustomProxySchemaWithoutLosingRoutes(t *testing.T) {
	pool := migrationTestPool(t)
	ctx := context.Background()
	// These migrations arrived with upstream v1.6.0. The existing deployment
	// already has the original source schema and all custom proxy migrations.
	upstreamAdditions := []string{
		"000019_create_system_settings.sql", "000020_create_user_activity.sql",
		"000021_create_profit.sql", "000022_create_profit_price_changes.sql",
		"000023_add_sync_started_by.sql", "000024_create_client_configs.sql",
		"000025_add_client_min_version.sql", "000026_create_tickets.sql",
		"000027_add_client_tool_recommends.sql", "000028_add_more_client_tool_recommends.sql",
		"000029_create_leads.sql", "000030_create_sales_exclusions.sql", "000031_create_vip_watch.sql",
	}
	if err := ensureMigrationsTable(ctx, pool); err != nil {
		t.Fatal(err)
	}
	entries, err := migrationFiles.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".sql") || slices.Contains(upstreamAdditions, name) {
			continue
		}
		body, err := migrationFiles.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, string(body)); err != nil {
			t.Fatalf("legacy migration %s: %v", name, err)
		}
		if _, err := pool.Exec(ctx, "INSERT INTO schema_migrations (version) VALUES ($1)", strings.TrimSuffix(name, ".sql")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO proxy_routes
		(id, user_id, admin_account_id, name, site_id, group_id, group_name, upstream_key_ciphertext, concurrency_limit)
		VALUES ('existing-route', 'user', 'workspace', 'existing', 'site', '1', 'default', 'encrypted-existing-key', 50)`); err != nil {
		t.Fatal(err)
	}
	if err := Run(ctx, pool); err != nil {
		t.Fatalf("upgrade existing installation: %v", err)
	}
	if err := Run(ctx, pool); err != nil {
		t.Fatalf("repeat upgraded migrations: %v", err)
	}
	var key string
	var concurrency int
	if err := pool.QueryRow(ctx, "SELECT upstream_key_ciphertext, concurrency_limit FROM proxy_routes WHERE id = 'existing-route'").Scan(&key, &concurrency); err != nil {
		t.Fatal(err)
	}
	if key != "encrypted-existing-key" || concurrency != 50 {
		t.Fatalf("existing route changed: key=%q concurrency=%d", key, concurrency)
	}
	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM schema_migrations").Scan(&count); err != nil || count != len(entries) {
		t.Fatalf("all migrations must be recorded by full filename: count=%d want=%d err=%v", count, len(entries), err)
	}
}
