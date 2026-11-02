package main

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// pgx itself must read the cap back from whatever DATABASE_URL the task is given.
func TestEachStrikePoolIsCapped(t *testing.T) {
	for _, dsn := range []string{
		"postgres://u:p@db.internal:5432/matching_backend?sslmode=require",
		"postgres://u:p@db.internal:5432/matching_backend?sslmode=require&pool_max_conns=50",
		"host=db.internal user=u dbname=matching_backend pool_max_conns=50",
	} {
		cfg, err := pgxpool.ParseConfig(withPoolMaxConns(dsn, optionPoolMaxConns))
		if err != nil {
			t.Fatalf("%s: %v", dsn, err)
		}
		if cfg.MaxConns != optionPoolMaxConns {
			t.Fatalf("%s: MaxConns %d", dsn, cfg.MaxConns)
		}
	}
}
