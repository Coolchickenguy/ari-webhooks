package migrate

import (
	"context"
	"database/sql"
	"embed"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

//go:embed sql/*.sql
var migrations embed.FS

func Run(ctx context.Context, pool *pgxpool.Pool) error {
	goose.SetBaseFS(migrations)
	goose.SetTableName("ariwMigrations") // must never collide with Prisma's _prisma_migrations
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("postgres"); err != nil {
		return err
	}
	db := sql.OpenDB(stdlib.GetPoolConnector(pool))
	defer db.Close()
	return goose.UpContext(ctx, db, "sql")
}
