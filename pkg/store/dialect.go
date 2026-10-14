package store

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// The store speaks two SQL dialects: embedded SQLite (default, tests) and PostgreSQL
// (production). Queries are written once with "?" placeholders; the wrappers below
// rewrite them to "$n" for PostgreSQL, so the rest of the package stays dialect-free.

type conn struct {
	*sql.DB
	pg bool
}

type txn struct {
	*sql.Tx
	pg bool
}

// execer is what both a connection and a transaction offer.
type execer interface {
	ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row
}

func (c *conn) q(query string) string {
	if c.pg {
		return rebind(query)
	}
	return query
}

func (c *conn) ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error) {
	return c.DB.ExecContext(ctx, c.q(q), args...)
}

func (c *conn) QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	return c.DB.QueryContext(ctx, c.q(q), args...)
}

func (c *conn) QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row {
	return c.DB.QueryRowContext(ctx, c.q(q), args...)
}

func (c *conn) BeginTx(ctx context.Context, opts *sql.TxOptions) (*txn, error) {
	tx, err := c.DB.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &txn{Tx: tx, pg: c.pg}, nil
}

func (t *txn) q(query string) string {
	if t.pg {
		return rebind(query)
	}
	return query
}

func (t *txn) ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error) {
	return t.Tx.ExecContext(ctx, t.q(q), args...)
}

func (t *txn) QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row {
	return t.Tx.QueryRowContext(ctx, t.q(q), args...)
}

// rebind turns "?" placeholders into "$1, $2, ..." (question marks inside string literals are kept).
func rebind(q string) string {
	if !strings.Contains(q, "?") {
		return q
	}
	var b strings.Builder
	b.Grow(len(q) + 8)
	n, quoted := 0, false
	for i := 0; i < len(q); i++ {
		switch c := q[i]; {
		case c == '\'':
			quoted = !quoted
			b.WriteByte(c)
		case c == '?' && !quoted:
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// insertID runs an INSERT and returns the id of the new row.
func insertID(ctx context.Context, e execer, pg bool, q string, args ...any) (int64, error) {
	if pg {
		var id int64
		err := e.QueryRowContext(ctx, q+` RETURNING id`, args...).Scan(&id)
		return id, err
	}
	res, err := e.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func IsPostgresURL(target string) bool {
	return strings.HasPrefix(target, "postgres://") || strings.HasPrefix(target, "postgresql://")
}

// openPostgres connects to PostgreSQL, waiting a little for the database to accept connections.
func openPostgres(url string) (*sql.DB, error) {
	db, err := sql.Open("pgx", url)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(10)
	db.SetConnMaxLifetime(30 * time.Minute)
	var last error
	for i := 0; i < 30; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		last = db.PingContext(ctx)
		cancel()
		if last == nil {
			return db, nil
		}
		time.Sleep(time.Second)
	}
	_ = db.Close()
	return nil, fmt.Errorf("postgres is unreachable: %w", last)
}

var integerRe = regexp.MustCompile(`\bINTEGER\b`)

// pgSchema converts a SQLite migration into PostgreSQL DDL.
func pgSchema(ddl string) string {
	ddl = strings.ReplaceAll(ddl, "INTEGER PRIMARY KEY AUTOINCREMENT", "BIGSERIAL PRIMARY KEY")
	ddl = integerRe.ReplaceAllString(ddl, "BIGINT") // unix seconds overflow int4 in 2038
	ddl = strings.ReplaceAll(ddl, "BLOB", "BYTEA")
	ddl = strings.ReplaceAll(ddl, " COLLATE NOCASE", "")
	return ddl
}

// pgExtra holds PostgreSQL-only statements applied after the migrations (idempotent).
var pgExtra = []string{
	// SQLite gets case-insensitive e-mails from COLLATE NOCASE; PostgreSQL from this index.
	`CREATE UNIQUE INDEX IF NOT EXISTS users_email_lower ON users (lower(email))`,
}

func isConstraintErr(err error) bool {
	if err == nil {
		return false
	}
	m := err.Error()
	return strings.Contains(m, "constraint failed") || strings.Contains(m, "violates")
}
