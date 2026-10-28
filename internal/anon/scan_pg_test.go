package anon_test

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"kontra.sh/kontra/internal/anon"
	"kontra.sh/kontra/internal/enroll"
	"kontra.sh/kontra/internal/pgtest"
	"kontra.sh/kontra/internal/tunnel"
)

// addressForms are the ways a client address can be written: an IPv4 address dotted, IPv4-mapped (dotted and hex,
// compressed and expanded); an IPv6 address compressed, uncompressed and expanded. Lower case: the scan lowers what
// it reads.
func addressForms(a netip.Addr) []string {
	a = a.Unmap()
	var out []string
	if a.Is4() {
		b := a.As4()
		hexw := fmt.Sprintf("%02x%02x:%02x%02x", b[0], b[1], b[2], b[3])
		mapped := netip.AddrFrom16(netip.AddrFrom16(a.As16()).As16())
		out = append(out, a.String(), "::ffff:"+a.String(), "::ffff:"+strings.TrimLeft(hexw[:4], "0")+":"+strings.TrimLeft(hexw[5:], "0"),
			"0:0:0:0:0:ffff:"+hexw, "0000:0000:0000:0000:0000:ffff:"+hexw, mapped.StringExpanded())
	} else {
		b := a.As16()
		var groups, short []string
		for i := 0; i < 16; i += 2 {
			g := fmt.Sprintf("%02x%02x", b[i], b[i+1])
			groups = append(groups, g)
			short = append(short, strings.TrimLeft(g, "0"))
		}
		for i, s := range short {
			if s == "" {
				short[i] = "0"
			}
		}
		out = append(out, a.String(), a.StringExpanded(), strings.Join(groups, ":"), strings.Join(short, ":"))
	}
	for i := range out {
		out[i] = strings.ToLower(out[i])
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// scanColumn is one column the scan reads.
type scanColumn struct{ table, column, typ, udt string }

// scanAddresses reads every column of schema kontra of type text, varchar, text[], json, jsonb, bytea (escaped, and
// for the address's 4 or 16 bytes), inet and cidr, audit.row_jcs included, for any form of any of addrs, and returns
// "table.column" for each that holds one.
func scanAddresses(t *testing.T, pool *pgxpool.Pool, addrs ...netip.Addr) (hits []string, read []scanColumn) {
	t.Helper()
	ctx := context.Background()
	rows, err := pool.Query(ctx, `select table_name, column_name, data_type, udt_name from information_schema.columns
		where table_schema = 'kontra' and (data_type in ('text', 'character varying', 'json', 'jsonb', 'bytea', 'inet', 'cidr')
		   or (data_type = 'ARRAY' and udt_name in ('_text', '_varchar')))
		  and table_name in (select table_name from information_schema.tables where table_schema = 'kontra' and table_type = 'BASE TABLE')
		order by table_name, column_name`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var c scanColumn
		if err := rows.Scan(&c.table, &c.column, &c.typ, &c.udt); err != nil {
			t.Fatal(err)
		}
		read = append(read, c)
	}
	rows.Close()
	var patterns []string
	var bins [][]byte
	for _, a := range addrs {
		for _, f := range addressForms(a) {
			patterns = append(patterns, "%"+f+"%")
		}
		if a.Unmap().Is4() {
			b := a.Unmap().As4()
			bins = append(bins, b[:])
		}
		b := a.As16()
		bins = append(bins, b[:])
	}
	for _, c := range read {
		col := `"` + c.column + `"`
		var expr string
		switch c.typ {
		case "json", "jsonb", "inet", "cidr":
			expr = col + "::text"
		case "ARRAY":
			expr = "array_to_string(" + col + ", ' ')"
		case "bytea":
			expr = "encode(" + col + ", 'escape')"
		default:
			expr = col
		}
		where := "lower(" + expr + ") like any($1::text[])"
		args := []any{patterns}
		if c.typ == "bytea" {
			for _, b := range bins {
				args = append(args, b)
				where += fmt.Sprintf(" or position($%d::bytea in %s) > 0", len(args), col)
			}
		}
		var n int
		q := fmt.Sprintf(`select count(*) from kontra.%q where %s`, c.table, where)
		if err := pool.QueryRow(ctx, q, args...).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		if n > 0 {
			hits = append(hits, c.table+"."+c.column)
		}
	}
	return hits, read
}

type node struct {
	priv      *ecdh.PrivateKey
	recipient string
	kid       string
}

func newNode(t *testing.T) *node {
	t.Helper()
	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	r, err := tunnel.FormatRecipient(priv.PublicKey())
	if err != nil {
		t.Fatal(err)
	}
	return &node{priv: priv, recipient: r, kid: tunnel.KeyID(r)}
}

func (n *node) admit(t *testing.T, s *enroll.Store, client netip.Addr, services ...string) *enroll.Decision {
	t.Helper()
	var nonce [32]byte
	_, _ = rand.Read(nonce[:])
	h := &tunnel.Hello{T: tunnel.MsgHello, V: tunnel.Version, Origin: "https://gate.example", KID: n.kid, Recipient: n.recipient,
		Name: "laptop", Nonce: hex.EncodeToString(nonce[:]), Agent: "kontra/test", Anonymous: true, Public: services}
	for _, s := range services {
		h.Services = append(h.Services, tunnel.Service{Kind: tunnel.KindA2A, Name: s})
	}
	d, err := s.Admit(context.Background(), enroll.Request{Hello: h, PubKey: n.priv.PublicKey().Bytes(), SessionID: uuid.Must(uuid.NewV7()),
		ReplicaID: "r-scan", Client: client})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// spec/tunnel.md §6.2, §16 anon_lifecycle (DM4-20, the M4 audit's address scan): no table holds a client's address
// in any form, while its anonymous session lives and after the purge; the network is stored as a CIDR. The scan reads
// every text, varchar, text[], json, jsonb, bytea (escaped, and the address's bytes), inet and cidr column of schema
// kontra, audit.row_jcs included, for the dotted, IPv4-mapped, compressed and expanded forms; a positive control
// shows it finds each form in each type.
func TestNoTableHoldsTheClientAddress(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Pool(t, pgtest.Migrated(t))
	v4, v6 := netip.MustParseAddr("203.0.113.77"), netip.MustParseAddr("2001:db8:77::5")

	// The positive control: a scratch table with each type, each form of each address in one row of its own.
	if _, err := pool.Exec(ctx, `create table kontra.scan_control (t text, v varchar(100), a text[], j json, jb jsonb, b bytea, i inet, c cidr)`); err != nil {
		t.Fatal(err)
	}
	for _, a := range []netip.Addr{v4, v6} {
		for _, f := range addressForms(a) {
			if _, err := pool.Exec(ctx, `delete from kontra.scan_control`); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `insert into kontra.scan_control (t, v, a, j, jb, b) values ($1::text, $1::text, array['x', $1::text],
				json_build_object('from', $1::text), jsonb_build_array(upper($1::text)), convert_to('addr=' || $1::text, 'UTF8'))`, f); err != nil {
				t.Fatal(err)
			}
			hits, _ := scanAddresses(t, pool, a)
			if want := []string{"scan_control.a", "scan_control.b", "scan_control.j", "scan_control.jb", "scan_control.t", "scan_control.v"}; !slices.Equal(hits, want) {
				t.Fatalf("the control holding %q: the scan found %v, want %v", f, hits, want)
			}
		}
		b := a.Unmap().AsSlice()
		if _, err := pool.Exec(ctx, `delete from kontra.scan_control`); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `insert into kontra.scan_control (b, i, c) values ($1, $2::inet, $3::cidr)`,
			append([]byte("\x00\x01"), b...), a.String(), netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen()).String()); err != nil {
			t.Fatal(err)
		}
		if hits, _ := scanAddresses(t, pool, a); !slices.Equal(hits, []string{"scan_control.b", "scan_control.c", "scan_control.i"}) {
			t.Fatalf("the control holding %s as bytes, inet and cidr: the scan found %v", a, hits)
		}
	}
	if _, err := pool.Exec(ctx, `drop table kontra.scan_control`); err != nil {
		t.Fatal(err)
	}

	// A lifecycle through the real admission, whose request carries the address: open, resume, a second session from
	// IPv6, a task, the expiry, the purge.
	lim := anon.Defaults
	lim.PurgeAfter = time.Minute
	s := enroll.NewStore(pool, enroll.Options{PublicURL: "https://gate.example", Multi: true, Anonymous: &lim})
	n1, n2 := newNode(t), newNode(t)
	d1 := n1.admit(t, s, v4, "hello")
	n1.admit(t, s, netip.MustParseAddr("::ffff:203.0.113.77"), "hello", "other")
	d2 := n2.admit(t, s, v6, "echo")
	if _, err := pool.Exec(ctx, `insert into kontra.tasks (id, tenant_id, service, protocol, buyer_id, principal, via, context_id, state,
		version, snapshot, status_at) values ($1, $2, 'hello', 'a2a', $3, 'anonymous', 'anonymous', 'ctx', 'working', 1, '{}', now())`,
		uuid.Must(uuid.NewV7()), d1.Key.TenantID, uuid.New()); err != nil {
		t.Fatal(err)
	}
	hits, read := scanAddresses(t, pool, v4, v6)
	if len(hits) != 0 {
		t.Fatalf("while the sessions live, the client's address is in %v", hits)
	}
	cols := map[string]bool{}
	for _, c := range read {
		cols[c.table+"."+c.column] = true
	}
	for _, must := range []string{"audit.row_jcs", "tenants.created_from", "tenants.slug", "services.allow", "services.annotations",
		"tasks.snapshot", "task_events.payload", "tunnel_keys.node_name", "anon_calls.replica_id"} {
		if !cols[must] {
			t.Errorf("the scan does not read %s", must)
		}
	}
	var froms []string
	rows, err := pool.Query(ctx, `select created_from::text from kontra.tenants where kind = 'anonymous' order by 1`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var f string
		_ = rows.Scan(&f)
		froms = append(froms, f)
	}
	rows.Close()
	if !slices.Equal(froms, []string{"2001:db8:77::/48", "203.0.113.0/24"}) {
		t.Fatalf("created_from %v", froms)
	}

	if _, err := pool.Exec(ctx, `update kontra.tenants set expires_at = now() - interval '2 minutes' where id = any($1)`,
		[]uuid.UUID{d1.Key.TenantID, d2.Key.TenantID}); err != nil {
		t.Fatal(err)
	}
	sw := anon.NewSweeper(pool, lim, nil, nil)
	if n, err := sw.Expire(ctx); err != nil || n != 2 {
		t.Fatalf("expire %d %v", n, err)
	}
	if n, err := sw.Purge(ctx); err != nil || n != 2 {
		t.Fatalf("purge %d %v", n, err)
	}
	if hits, _ := scanAddresses(t, pool, v4, v6); len(hits) != 0 {
		t.Fatalf("after the purge, the client's address is in %v", hits)
	}
	var keys, tombstones int
	if err := pool.QueryRow(ctx, `select (select count(*) from kontra.tunnel_keys),
		(select count(*) from kontra.tenants where kind = 'anonymous' and deleted_at is not null and created_from is not null)`).Scan(&keys, &tombstones); err != nil {
		t.Fatal(err)
	}
	if keys != 0 || tombstones != 2 {
		t.Fatalf("after the purge: %d keys, %d tombstones", keys, tombstones)
	}
}
