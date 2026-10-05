package testdb

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hackclub/ari-webhooks/internal/db/migrate"
)

// New creates a throwaway database with ari's Prisma schema plus our goose
// migrations applied. Tests skip unless TEST_DATABASE_URL points at a server
// (e.g. postgres://localhost:5432/postgres).
func New(t *testing.T) *pgxpool.Pool {
	t.Helper()
	baseUrl := os.Getenv("TEST_DATABASE_URL")
	if baseUrl == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()

	admin, err := pgx.Connect(ctx, baseUrl)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	name := fmt.Sprintf("ariw_test_%d_%s", os.Getpid(), strings.ToLower(t.Name()))
	name = sanitize(name)
	if _, err := admin.Exec(ctx, "drop database if exists "+name); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "create database "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "drop database if exists "+name+" with (force)")
		admin.Close(context.Background())
	})

	cfg, err := pgxpool.ParseConfig(baseUrl)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	applyAriMigrations(t, pool)
	if err := migrate.Run(ctx, pool); err != nil {
		t.Fatalf("goose migrations: %v", err)
	}
	return pool
}

func applyAriMigrations(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	dir := os.Getenv("ARI_MIGRATIONS_DIR")
	if dir == "" {
		// Resolved from THIS source file, not the package under test, so tests at
		// any package depth find the sibling web repo checkout.
		_, self, _, _ := runtime.Caller(0)
		dir = filepath.Join(filepath.Dir(self), "..", "..", "..", "ari-next", "prisma", "migrations")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("web repo migrations not found at %s (set ARI_MIGRATIONS_DIR): %v", dir, err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	ctx := context.Background()
	for _, n := range names {
		sql, err := os.ReadFile(filepath.Join(dir, n, "migration.sql"))
		if err != nil {
			t.Fatalf("read migration %s: %v", n, err)
		}
		// Statement by statement, no wrapping transaction: Prisma applies Postgres
		// migrations unwrapped, and enum ADD VALUE cannot be used inside the same tx.
		for _, stmt := range splitStatements(string(sql)) {
			if _, err := pool.Exec(ctx, stmt); err != nil {
				t.Fatalf("apply migration %s: %v\nstatement: %s", n, err, stmt)
			}
		}
	}
}

// ConnString rebuilds a connection string for the pool's throwaway database,
// for code that opens its own dedicated connection (e.g. LISTEN).
func ConnString(pool *pgxpool.Pool) string {
	u, err := url.Parse(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		return ""
	}
	u.Path = "/" + pool.Config().ConnConfig.Database
	return u.String()
}

func splitStatements(sql string) []string {
	var out []string
	var cur strings.Builder
	inString := false
	inDollar := false
	for i := 0; i < len(sql); i++ {
		c := sql[i]
		if !inString && c == '$' && i+1 < len(sql) && sql[i+1] == '$' {
			inDollar = !inDollar // function bodies carry their own semicolons
			cur.WriteString("$$")
			i++
			continue
		}
		if inDollar {
			cur.WriteByte(c)
			continue
		}
		if !inString && c == '-' && i+1 < len(sql) && sql[i+1] == '-' {
			for i < len(sql) && sql[i] != '\n' {
				i++
			}
			cur.WriteByte('\n')
			continue
		}
		cur.WriteByte(c)
		switch {
		case c == '\'':
			inString = !inString
		case c == ';' && !inString:
			stmt := strings.TrimSpace(cur.String())
			if stmt != ";" && stmt != "" {
				out = append(out, stmt)
			}
			cur.Reset()
		}
	}
	if tail := strings.TrimSpace(cur.String()); tail != "" {
		out = append(out, tail)
	}
	return out
}

func sanitize(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s) && i < 60; i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' {
			out = append(out, c)
		} else {
			out = append(out, '_')
		}
	}
	return string(out)
}
