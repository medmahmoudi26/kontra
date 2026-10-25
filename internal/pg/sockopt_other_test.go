//go:build !linux

package pg_test

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func checkClientSocket(t *testing.T, _ *pgxpool.Pool) {
	t.Log("client socket options are checked on Linux only")
}
