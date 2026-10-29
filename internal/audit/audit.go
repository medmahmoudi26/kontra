// Package audit appends to the gate's audit hash chain, kontra.audit (PRD §12; migrations/0001_init.sql):
// enrollments, revocations, key and service changes, sealed-part rejections.
//
// A row is one event, an object of a closed set of value types, kept as its RFC 8785 (JCS) bytes in
// row_jcs. hash = sha256(prev_hash || row_jcs), where prev_hash is the hash of the row appended before it
// (empty for the first row, whose prev_hash column is null). Appends are serialized by a transaction-level
// advisory lock taken in the caller's transaction, so the chain head read under it is the last committed
// row and a rolled-back append leaves no trace but a gap in n; the chain is verified by prev_hash, never
// by n. Every row carries "event" (what happened) and "at" (the database's clock when it was appended,
// the same instant as the at column), so the time is under the hash too. The signed hourly checkpoint is
// v4.1.
//
// A row never carries a secret: callers pass ids, prefixes and names, never a key, a token or a hash.
package audit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// LockID is the advisory lock that serializes appends: "kontra" in ASCII, then 0x0100. The gate's other
// key is migrate.LockID (…0001); Kontra Cloud, which may share the database, uses …0003, …0005 and …0006
// for its own workers, so the gate's keys stay outside the low range.
const LockID int64 = 0x6b6f6e7472610100

// Events the gate appends. Each names its subject and what happened to it.
const (
	EventAPIKeyMinted  = "api_key.minted"
	EventAPIKeyRevoked = "api_key.revoked"
	// EventSealedRejected is a hosting node's refusal of a sealed part (ext-sealed.md §6: 400 with Kontra-Node:
	// sealed_rejected): the task fails sealed_rejected and is not retried. Its row names the task, the service, the
	// attempt and where it ran, and the node's reason when it named one of §6's (SealedRejected).
	EventSealedRejected = "sealed.rejected"
)

// Beginner opens a transaction (a *pgxpool.Pool, a *pgx.Conn).
type Beginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// SealedRejected appends EventSealedRejected in a transaction of its own: the rejection is the node's, recorded
// when the gate learns it, beside the task's failure (ext-sealed.md §5 "Gate", §6; relay.md §6.4). f names the
// task (task_id, tenant_id, service, protocol, attempt) and the node's reason (reason, "" when it named none of
// §6's), never a part's bytes.
func SealedRejected(ctx context.Context, db Beginner, f Fields) error {
	return pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		_, err := Append(ctx, tx, EventSealedRejected, f)
		return err
	})
}

// Fields are an event's facts. Values are strings, booleans, nil, integers (int, int64), uuid.UUID,
// time.Time and *time.Time (RFC 3339 in UTC with microseconds, nil as null), []string, []any and nested
// Fields or map[string]any. "event" and "at" are set by Append.
type Fields map[string]any

// Append adds one event to the chain inside tx, the transaction of the change it records, and returns
// the row's n. It takes the advisory lock (held until tx ends), reads the chain head and the database's
// clock under it, and inserts the row.
func Append(ctx context.Context, tx pgx.Tx, event string, f Fields) (int64, error) {
	if event == "" {
		return 0, errors.New("audit: an event needs a name")
	}
	if _, err := tx.Exec(ctx, "select pg_advisory_xact_lock($1)", LockID); err != nil {
		return 0, fmt.Errorf("audit: lock: %w", err)
	}
	var prev []byte
	var at time.Time
	err := tx.QueryRow(ctx, `select (select hash from kontra.audit order by n desc limit 1), clock_timestamp()`).Scan(&prev, &at)
	if err != nil {
		return 0, fmt.Errorf("audit: chain head: %w", err)
	}
	row := make(Fields, len(f)+2)
	for k, v := range f {
		row[k] = v
	}
	row["event"] = event
	row["at"] = at
	jcs, err := Canonical(row)
	if err != nil {
		return 0, fmt.Errorf("audit: %s: %w", event, err)
	}
	h := Hash(prev, jcs)
	var n int64
	err = tx.QueryRow(ctx, `insert into kontra.audit (at, prev_hash, row_jcs, hash) values ($1, $2, $3, $4) returning n`,
		at, prev, jcs, h[:]).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("audit: insert: %w", err)
	}
	return n, nil
}

// Hash is sha256(prev || rowJCS), the chain's link (prev is empty for the first row).
func Hash(prev, rowJCS []byte) [32]byte {
	h := sha256.New()
	h.Write(prev)
	h.Write(rowJCS)
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// Row is one audit row as read back.
type Row struct {
	N        int64
	At       time.Time
	PrevHash []byte
	RowJCS   []byte
	Hash     []byte
}

// ErrBroken is wrapped by Verify's error when a link of the chain does not hold.
var ErrBroken = errors.New("audit: chain broken")

// Verify reads the whole chain in order and checks every link: the first row has no prev_hash, every
// later row's prev_hash is the hash of the row before it, and every hash is sha256(prev_hash || row_jcs).
// It returns the rows it read.
func Verify(ctx context.Context, q interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}) ([]Row, error) {
	rows, err := q.Query(ctx, `select n, at, prev_hash, row_jcs, hash from kontra.audit order by n`)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Row, error) {
		var a Row
		err := r.Scan(&a.N, &a.At, &a.PrevHash, &a.RowJCS, &a.Hash)
		return a, err
	})
	if err != nil {
		return nil, err
	}
	var prev []byte
	for i, r := range out {
		if i == 0 && r.PrevHash != nil {
			return out, fmt.Errorf("%w: the first row (n %d) has a prev_hash", ErrBroken, r.N)
		}
		if i > 0 && !bytes.Equal(r.PrevHash, prev) {
			return out, fmt.Errorf("%w: row n %d does not name the hash of row n %d", ErrBroken, r.N, out[i-1].N)
		}
		if h := Hash(r.PrevHash, r.RowJCS); !bytes.Equal(h[:], r.Hash) {
			return out, fmt.Errorf("%w: row n %d: hash does not match its content", ErrBroken, r.N)
		}
		prev = r.Hash
	}
	return out, nil
}

// TimeFormat is how a time is written in a row: RFC 3339, UTC, microseconds (Postgres' precision).
const TimeFormat = "2006-01-02T15:04:05.000000Z07:00"

// Canonical returns the RFC 8785 (JCS) serialization of v for the value types Fields allows. Object
// members are sorted by their names' UTF-16 code units; strings are escaped as ECMAScript's
// JSON.stringify does (only '"', '\\' and the C0 controls, the latter as \b \t \n \f \r or \u00xx);
// integers must lie within ±(2^53 − 1), the range JCS numbers write exactly. Invalid UTF-8 is an error.
func Canonical(v any) ([]byte, error) {
	var b bytes.Buffer
	if err := writeJCS(&b, v, 0); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

const maxSafeInt = 1<<53 - 1

func writeJCS(b *bytes.Buffer, v any, depth int) error {
	if depth > 32 {
		return errors.New("value nested too deeply")
	}
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		b.WriteString(strconv.FormatBool(x))
	case string:
		return writeString(b, x)
	case int:
		return writeInt(b, int64(x))
	case int64:
		return writeInt(b, x)
	case uuid.UUID:
		return writeString(b, x.String())
	case time.Time:
		return writeString(b, x.UTC().Format(TimeFormat))
	case *time.Time:
		if x == nil {
			b.WriteString("null")
			return nil
		}
		return writeString(b, x.UTC().Format(TimeFormat))
	case []string:
		b.WriteByte('[')
		for i, s := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := writeString(b, s); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	case []any:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := writeJCS(b, e, depth+1); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	case Fields:
		return writeObject(b, x, depth)
	case map[string]any:
		return writeObject(b, x, depth)
	default:
		return fmt.Errorf("unsupported value of type %T", v)
	}
	return nil
}

func writeInt(b *bytes.Buffer, n int64) error {
	if n > maxSafeInt || n < -maxSafeInt {
		return fmt.Errorf("integer %d is outside the range JCS writes exactly", n)
	}
	b.WriteString(strconv.FormatInt(n, 10))
	return nil
}

func writeObject(b *bytes.Buffer, m map[string]any, depth int) error {
	keys := make([]string, 0, len(m))
	for k := range m {
		if !utf8.ValidString(k) {
			return fmt.Errorf("member name %q is not UTF-8", k)
		}
		keys = append(keys, k)
	}
	slices.SortFunc(keys, compareUTF16)
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		if err := writeString(b, k); err != nil {
			return err
		}
		b.WriteByte(':')
		if err := writeJCS(b, m[k], depth+1); err != nil {
			return fmt.Errorf("%s: %w", k, err)
		}
	}
	b.WriteByte('}')
	return nil
}

// compareUTF16 orders strings by their UTF-16 code units (RFC 8785 §3.2.3), which differs from byte
// order only for characters above U+FFFF against U+E000..U+FFFF.
func compareUTF16(a, b string) int {
	return slices.Compare(utf16.Encode([]rune(a)), utf16.Encode([]rune(b)))
}

func writeString(b *bytes.Buffer, s string) error {
	if !utf8.ValidString(s) {
		return fmt.Errorf("string %q is not UTF-8", s)
	}
	const hex = "0123456789abcdef"
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			b.WriteString(`\"`)
		case c == '\\':
			b.WriteString(`\\`)
		case c == '\b':
			b.WriteString(`\b`)
		case c == '\t':
			b.WriteString(`\t`)
		case c == '\n':
			b.WriteString(`\n`)
		case c == '\f':
			b.WriteString(`\f`)
		case c == '\r':
			b.WriteString(`\r`)
		case c < 0x20:
			b.WriteString(`\u00`)
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&0xf])
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return nil
}
