package audit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

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
