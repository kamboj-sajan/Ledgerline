package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// DB is the query interface shared by the packages that run SQL.
// *pgxpool.Pool, *pgx.Conn and pgx.Tx all satisfy it, so the same function
// can run inside or outside a transaction.
type DB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// CountAndList returns how many rows query produces and the first limit of
// them, ordered by the first column. query must select exactly one column and
// must be a fixed string written in this codebase, never user input. It is
// used by the audits, which report an exact count plus a sample.
func CountAndList[T any](ctx context.Context, db DB, query string, limit int) (int64, []T, error) {
	var n int64
	if err := db.QueryRow(ctx, `SELECT count(*) FROM (`+query+`) q`).Scan(&n); err != nil {
		return 0, nil, err
	}
	if n == 0 {
		return 0, nil, nil
	}
	rows, err := db.Query(ctx, `SELECT * FROM (`+query+`) q ORDER BY 1 LIMIT `+fmt.Sprint(limit))
	if err != nil {
		return 0, nil, err
	}
	defer rows.Close()
	var out []T
	for rows.Next() {
		var v T
		if err := rows.Scan(&v); err != nil {
			return 0, nil, err
		}
		out = append(out, v)
	}
	return n, out, rows.Err()
}
