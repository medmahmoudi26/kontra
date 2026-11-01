package audit

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"kontra.sh/kontra/internal/pgtest"
)

// Canonical is RFC 8785 for the value types a row allows: members sorted by UTF-16 code units, strings
// escaped as JSON.stringify does, integers plain.
func TestCanonical(t *testing.T) {
	at := time.Date(2026, 10, 25, 7, 8, 9, 123456789, time.FixedZone("x", 3600))
	cases := []struct {
		in   any
		want string
	}{
		{Fields{"b": 1, "a": "x", "c": nil, "d": true}, `{"a":"x","b":1,"c":null,"d":true}`},
		{Fields{"t": at, "nilt": (*time.Time)(nil), "pt": &at}, `{"nilt":null,"pt":"2026-10-25T06:08:09.123456Z","t":"2026-10-25T06:08:09.123456Z"}`},
		{Fields{"id": uuid.MustParse("0192f0b4-0000-7000-8000-000000000001")}, `{"id":"0192f0b4-0000-7000-8000-000000000001"}`},
		{Fields{"s": []string{"hire", "enroll:*"}, "e": []string{}}, `{"e":[],"s":["hire","enroll:*"]}`},
		{Fields{"n": Fields{"z": 1, "y": []any{"a", int64(-2), map[string]any{"k": false}}}}, `{"n":{"y":["a",-2,{"k":false}],"z":1}}`},
		// Escapes: only '"', '\\' and C0 controls; U+2028, U+2029, '<', '&' and non-ASCII stay literal.
		{"\"\\\b\f\n\r\t\x01\x1f<>&é  ", `"\"\\\b\f\n\r\t\u0001\u001f<>&é` + "  " + `"`},
		// RFC 8785 §3.2.3 sorting: by UTF-16 code units, so U+1F600 (a surrogate pair, D83D…) sorts before
		// U+FB33 (FB33), the reverse of their UTF-8 byte order.
		{Fields{"דּ": 1, "\U0001F600": 2, "a": 3}, "{\"a\":3,\"\U0001F600\":2,\"דּ\":1}"},
		{int64(1<<53 - 1), `9007199254740991`},
	}
	for _, tc := range cases {
		got, err := Canonical(tc.in)
		if err != nil || string(got) != tc.want {
			t.Errorf("Canonical(%v) = %s (%v), want %s", tc.in, got, err, tc.want)
		}
		if err == nil && !json.Valid(got) {
			t.Errorf("Canonical(%v) is not JSON: %s", tc.in, got)
		}
	}
	for _, bad := range []any{
		int64(1 << 53), 1.5, "\xff", Fields{"\xff": 1}, struct{}{}, []int{1},
	} {
		if _, err := Canonical(bad); err == nil {
			t.Errorf("Canonical(%#v) accepted", bad)
		}
	}
}

func TestHash(t *testing.T) {
	row := []byte(`{"a":1}`)
	first := Hash(nil, row)
	if first != sha256.Sum256(row) {
		t.Error("the first row's hash is not sha256(row_jcs)")
	}
	next := Hash(first[:], row)
	if next != sha256.Sum256(append(first[:], row...)) {
		t.Error("a later row's hash is not sha256(prev_hash || row_jcs)")
	}
}

// Append chains rows under the advisory lock, even when many transactions append at once; a rolled-back
// append leaves only a gap in n; Verify finds a tampered row.
func TestAppendChain(t *testing.T) {
	pool := pgtest.New(t)
	ctx := context.Background()
	appendOne := func(i int, commit bool) {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := Append(ctx, tx, "test.event", Fields{"i": i}); err != nil {
			t.Error(err)
			return
		}
		if commit {
			if err := tx.Commit(ctx); err != nil {
				t.Error(err)
			}
		}
	}
	appendOne(0, true)
	appendOne(-1, false) // rolled back
	var wg sync.WaitGroup
	for i := 1; i <= 20; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); appendOne(i, true) }()
	}
	wg.Wait()
	rows, err := Verify(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 21 {
		t.Fatalf("%d rows, want 21", len(rows))
	}
	if rows[0].PrevHash != nil || rows[1].N == rows[0].N+1 {
		t.Errorf("first row prev_hash %x; n %d then %d (the rolled-back append should leave a gap)", rows[0].PrevHash, rows[0].N, rows[1].N)
	}
	seen := map[float64]bool{}
	for _, r := range rows {
		var f map[string]any
		if err := json.Unmarshal(r.RowJCS, &f); err != nil {
			t.Fatal(err)
		}
		if f["event"] != "test.event" {
			t.Errorf("row %d event %v", r.N, f["event"])
		}
		at, err := time.Parse(time.RFC3339Nano, f["at"].(string))
		if err != nil || !at.Equal(r.At) {
			t.Errorf("row %d: at in the row %v, column %v", r.N, f["at"], r.At)
		}
		seen[f["i"].(float64)] = true
	}
	if len(seen) != 21 || seen[-1] {
		t.Errorf("rows hold %v", seen)
	}

	// Tampering with a row's content, or with a link, breaks the chain where it happened.
	mid := rows[10]
	if _, err := pool.Exec(ctx, `update kontra.audit set row_jcs = $2 where n = $1`, mid.N, bytes.Replace(mid.RowJCS, []byte("test.event"), []byte("test.evenT"), 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(ctx, pool); !errors.Is(err, ErrBroken) {
		t.Errorf("tampered content: %v", err)
	}
	if _, err := pool.Exec(ctx, `update kontra.audit set row_jcs = $2 where n = $1`, mid.N, mid.RowJCS); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `delete from kontra.audit where n = $1`, rows[5].N); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(ctx, pool); !errors.Is(err, ErrBroken) {
		t.Errorf("a deleted row: %v", err)
	}

	// An event needs a name; an unsupported value aborts the append.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := Append(ctx, tx, "", nil); err == nil {
		t.Error("an unnamed event appended")
	}
	if _, err := Append(ctx, tx, "x", Fields{"f": 1.5}); err == nil {
		t.Error("a float appended")
	}
}

// sqlAppend appends rowJCS through kontra.audit_append (migration 0004, D317) in a transaction of its own (opts: its
// isolation), committing it when commit is set, and returns the hash the function answered.
func sqlAppend(ctx context.Context, pool *pgxpool.Pool, rowJCS []byte, commit bool, opts pgx.TxOptions) ([]byte, error) {
	tx, err := pool.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var h []byte
	if err := tx.QueryRow(ctx, `select kontra.audit_append($1)`, rowJCS).Scan(&h); err != nil {
		return nil, err
	}
	if commit {
		return h, tx.Commit(ctx)
	}
	return h, nil
}

// operatorRow is an operator's row as it would write one: RFC 8785 bytes with event and at.
func operatorRow(t *testing.T, i int) []byte {
	t.Helper()
	b, err := Canonical(Fields{"event": "service.published", "at": time.Now(), "tenant": "acme", "service": "hi", "i": i})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// M5-G2 (D317): kontra.audit_append and the Go appends make one chain. Interleaved, then concurrent, appends of both
// writers verify as one chain (audit.Verify); the function's answer is the row's hash, sha256(prev || row) as Hash
// computes it; a rolled-back call leaves nothing; a transaction above read committed is refused; an operator's role
// granted only USAGE on kontra and EXECUTE on the function appends through it and can neither insert into nor read
// kontra.audit, and a role without the grant cannot call it; a raw insert that links a stale head is refused
// (audit_prev_hash), so no writer forks the chain.
func TestSQLAppendJoinsTheChain(t *testing.T) {
	pool := pgtest.New(t)
	ctx := context.Background()
	goAppend := func(i int) error {
		return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			_, err := Append(ctx, tx, "test.event", Fields{"i": i})
			return err
		})
	}
	if err := goAppend(0); err != nil {
		t.Fatal(err)
	}
	answers := map[string][]byte{} // hash → row_jcs, from the function's answers
	var mu sync.Mutex
	sql := func(i int, commit bool) {
		row := operatorRow(t, i)
		h, err := sqlAppend(ctx, pool, row, commit, pgx.TxOptions{})
		if err != nil {
			t.Error(err)
			return
		}
		if commit {
			mu.Lock()
			answers[string(h)] = row
			mu.Unlock()
		}
	}
	sql(1, true)
	if err := goAppend(2); err != nil {
		t.Fatal(err)
	}
	sql(-1, false) // rolled back
	sql(3, true)
	var wg sync.WaitGroup
	for i := 4; i < 44; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if i%2 == 0 {
				if err := goAppend(i); err != nil {
					t.Error(err)
				}
				return
			}
			sql(i, true)
		}()
	}
	wg.Wait()
	rows, err := Verify(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 44 || len(answers) != 22 {
		t.Fatalf("%d rows (want 44), %d answered by the function (want 22)", len(rows), len(answers))
	}
	for i, r := range rows {
		row, bySQL := answers[string(r.Hash)]
		if !bySQL {
			continue
		}
		if !bytes.Equal(row, r.RowJCS) {
			t.Errorf("row n %d: the function's hash names another row", r.N)
		}
		var prev []byte
		if i > 0 {
			prev = rows[i-1].Hash
		}
		if h := Hash(prev, row); !bytes.Equal(h[:], r.Hash) {
			t.Errorf("row n %d: the function's hash is not Hash(prev, row)", r.N)
		}
		delete(answers, string(r.Hash))
	}
	if len(answers) != 0 {
		t.Errorf("%d answers name no row", len(answers))
	}

	// Above read committed: refused, nothing appended.
	for _, iso := range []pgx.TxIsoLevel{pgx.RepeatableRead, pgx.Serializable} {
		var pe *pgconn.PgError
		if _, err := sqlAppend(ctx, pool, operatorRow(t, 99), true, pgx.TxOptions{IsoLevel: iso}); !errors.As(err, &pe) || pe.Code != "25000" {
			t.Errorf("%s: %v, want SQLSTATE 25000", iso, err)
		}
	}
	// Not a row: refused.
	for _, bad := range []string{`{"event":"x"}`, `[]`, `{"at":"2026-11-01T00:00:00Z","event":""}`} {
		var pe *pgconn.PgError
		if _, err := sqlAppend(ctx, pool, []byte(bad), true, pgx.TxOptions{}); !errors.As(err, &pe) || pe.Code != "22023" {
			t.Errorf("%s: %v, want SQLSTATE 22023", bad, err)
		}
	}
	if rows, err := Verify(ctx, pool); err != nil || len(rows) != 44 {
		t.Fatalf("after the refusals: %d rows, %v", len(rows), err)
	}

	// The grants of migration 0004's header: an operator's role appends through the function only.
	var r [6]byte
	_, _ = rand.Read(r[:])
	op, other := pgx.Identifier{"kontra_test_op_" + hex.EncodeToString(r[:])}.Sanitize(), pgx.Identifier{"kontra_test_no_" + hex.EncodeToString(r[:])}.Sanitize()
	for _, role := range []string{op, other} {
		if _, err := pool.Exec(ctx, "create role "+role+" nologin"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			c := context.Background()
			_, _ = pool.Exec(c, "drop owned by "+role)
			_, _ = pool.Exec(c, "drop role if exists "+role)
		})
		if _, err := pool.Exec(ctx, "grant usage on schema kontra to "+role); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, "grant execute on function kontra.audit_append(bytea) to "+op); err != nil {
		t.Fatal(err)
	}
	asRole := func(role string, fn func(tx pgx.Tx) error) error {
		return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, "set local role "+role); err != nil {
				return err
			}
			return fn(tx)
		})
	}
	denied := func(err error) bool {
		var pe *pgconn.PgError
		return errors.As(err, &pe) && pe.Code == "42501"
	}
	var opHash []byte
	if err := asRole(op, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `select kontra.audit_append($1)`, operatorRow(t, 100)).Scan(&opHash)
	}); err != nil || len(opHash) != 32 {
		t.Fatalf("the operator's role through the function: %x %v", opHash, err)
	}
	if err := asRole(op, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `insert into kontra.audit (prev_hash, row_jcs, hash) values ($1, 'x', sha256('raw'))`, opHash)
		return err
	}); !denied(err) {
		t.Errorf("the operator's role inserting into kontra.audit: %v, want permission denied", err)
	}
	if err := asRole(op, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `select count(*) from kontra.audit`)
		return err
	}); !denied(err) {
		t.Errorf("the operator's role reading kontra.audit without a select grant: %v, want permission denied", err)
	}
	if err := asRole(other, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `select kontra.audit_append($1)`, operatorRow(t, 101))
		return err
	}); !denied(err) {
		t.Errorf("a role without the execute grant: %v, want permission denied", err)
	}

	// A raw insert linked to a stale head is a unique violation, not a fork; the chain still verifies with the
	// operator's row last.
	var pe *pgconn.PgError
	if _, err := pool.Exec(ctx, `insert into kontra.audit (prev_hash, row_jcs, hash) values ($1, 'x', sha256('fork'))`, rows[10].Hash); !errors.As(err, &pe) || pe.Code != "23505" {
		t.Errorf("a raw insert linked to a stale head: %v, want a unique violation", err)
	}
	rows, err = Verify(ctx, pool)
	if err != nil || len(rows) != 45 || !bytes.Equal(rows[44].Hash, opHash) {
		t.Fatalf("the chain after every writer: %d rows, %v", len(rows), err)
	}
}
