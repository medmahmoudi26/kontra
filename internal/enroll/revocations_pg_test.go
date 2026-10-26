package enroll

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"kontra.sh/kontra/internal/pg"
	"kontra.sh/kontra/internal/tunnel"
)

// listenRevoke opens a connection listening on kontra_revoke.
func (e *pgEnv) listenRevoke() *pgx.Conn {
	e.t.Helper()
	ctx := context.Background()
	c, err := pgx.Connect(ctx, e.dsn)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { c.Close(context.Background()) })
	if _, err := c.Exec(ctx, "listen "+string(pg.ChannelRevoke)); err != nil {
		e.t.Fatal(err)
	}
	return c
}

// nextNotify returns the next payload within d, or "" when none arrives.
func nextNotify(t *testing.T, c *pgx.Conn, d time.Duration) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	n, err := c.WaitForNotification(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return ""
		}
		t.Fatal(err)
	}
	return n.Payload
}

// enrolled enrolls a fresh key for the services with service tokens.
func (e *pgEnv) enrolled(services ...string) *testKey {
	e.t.Helper()
	k := newTestKey(e.t)
	var decl []tunnel.Service
	var toks []string
	for _, s := range services {
		decl = append(decl, a2a(s))
		toks = append(toks, e.token(e.team, s))
	}
	h := hello(k, decl...)
	h.Enroll = toks
	e.mustAdmit(k, h)
	return k
}

// failAudit makes every audit append fail until the returned func runs.
func (e *pgEnv) failAudit() func() {
	e.exec(`create function kontra.test_fail_audit() returns trigger language plpgsql as $$
		begin raise exception 'audit refused by the test'; end $$`)
	e.exec(`create trigger test_fail_audit before insert on kontra.audit for each row execute function kontra.test_fail_audit()`)
	return func() {
		e.exec(`drop trigger test_fail_audit on kontra.audit`)
		e.exec(`drop function kontra.test_fail_audit()`)
	}
}

// RevokeKey: revoked_at, the audit row and NOTIFY 'key:<key_id>' in one transaction; again is a no-op; an
// unknown key is ErrKeyNotFound. When the audit append fails, nothing is revoked and nothing is notified.
func TestRevokeKey(t *testing.T) {
	e := newPGEnv(t)
	ctx := context.Background()
	e.service(e.team, "hello", "a2a", false)
	k := e.enrolled("hello")
	listen := e.listenRevoke()

	restore := e.failAudit()
	if err := e.store.RevokeKey(ctx, k.kid, ReasonOperator, "test"); err == nil {
		t.Fatal("RevokeKey succeeded with a failing audit append")
	}
	restore()
	if n := e.count(`select count(*) from kontra.tunnel_keys where key_id = $1 and revoked_at is null`, k.kid); n != 1 {
		t.Error("the key was revoked without its audit row")
	}
	if p := nextNotify(t, listen, 300*time.Millisecond); p != "" {
		t.Errorf("NOTIFY %q from a transaction that rolled back", p)
	}

	before := len(e.verifyAudit())
	if err := e.store.RevokeKey(ctx, k.kid, ReasonOperator, "kontra-gate enroll --revoke"); err != nil {
		t.Fatal(err)
	}
	if p := nextNotify(t, listen, 2*time.Second); p != "key:"+k.kid {
		t.Errorf("NOTIFY %q", p)
	}
	var reason string
	if err := e.pool.QueryRow(ctx, `select revoked_reason from kontra.tunnel_keys where key_id = $1 and revoked_at is not null`, k.kid).Scan(&reason); err != nil || reason != ReasonOperator {
		t.Errorf("revoked_reason %q %v", reason, err)
	}
	ev := e.verifyAudit()
	if len(ev) != before+1 || ev[before]["event"] != EventKeyRevoked || ev[before]["key_id"] != k.kid ||
		ev[before]["reason"] != ReasonOperator || ev[before]["actor"] != "kontra-gate enroll --revoke" {
		t.Errorf("audit %v", ev[before:])
	}
	// Again: nothing changes.
	if err := e.store.RevokeKey(ctx, k.kid, ReasonOwner, "test"); err != nil {
		t.Errorf("revoking twice: %v", err)
	}
	if p := nextNotify(t, listen, 300*time.Millisecond); p != "" {
		t.Errorf("a second NOTIFY %q", p)
	}
	if n := len(e.verifyAudit()); n != before+1 {
		t.Errorf("%d audit rows after revoking twice", n)
	}
	for _, id := range []string{"k1_" + "aaaaaaaaaaaaaaaaaaaaaaaaaa", "not-a-key"} {
		if err := e.store.RevokeKey(ctx, id, ReasonOperator, "t"); !errors.Is(err, ErrKeyNotFound) {
			t.Errorf("RevokeKey(%s) = %v", id, err)
		}
	}
	if err := e.store.RevokeKey(ctx, k.kid, ReasonRotated, "t"); err == nil {
		t.Error("RevokeKey with reason rotated")
	}
	// A rotated key inside its overlap: revoking it ends the overlap at once.
	old := e.enrolled("hello")
	nk := newTestKey(t)
	if _, err := e.admit(nk, hello(nk, a2a("hello")), old); err != nil {
		t.Fatal(err)
	}
	if err := e.store.RevokeKey(ctx, old.kid, ReasonOwner, "t"); err != nil {
		t.Fatal(err)
	}
	if p := nextNotify(t, listen, 2*time.Second); p != "key:"+old.kid {
		t.Errorf("NOTIFY %q", p)
	}
	if n := e.count(`select count(*) from kontra.tunnel_keys where key_id = $1 and revoked_at is not null and rotated_to = $2`, old.kid, nk.kid); n != 1 {
		t.Error("the rotated key was not revoked")
	}
	ev = e.verifyAudit()
	if last := ev[len(ev)-1]; last["was_rotated"] != true {
		t.Errorf("audit %v", last)
	}
	k2, err := e.store.LookupKey(ctx, old.kid)
	if err != nil || k2.RevokedAt == nil || k2.RotatedTo != nk.kid || k2.Tenant != "default" {
		t.Errorf("LookupKey %+v %v", k2, err)
	}
	if _, err := e.store.LookupKey(ctx, "k1_aaaaaaaaaaaaaaaaaaaaaaaaab"); !errors.Is(err, ErrKeyNotFound) {
		t.Errorf("LookupKey of an unknown key: %v", err)
	}
}

// RevokeBinding: key_services.revoked_at, the audit row and NOTIFY 'binding:<key_id>/<service>' in one
// transaction; the key keeps its other bindings.
func TestRevokeBinding(t *testing.T) {
	e := newPGEnv(t)
	ctx := context.Background()
	e.service(e.team, "hello", "a2a", false)
	e.service(e.team, "recon", "a2a", false)
	k := e.enrolled("hello", "recon")
	listen := e.listenRevoke()

	restore := e.failAudit()
	if err := e.store.RevokeBinding(ctx, k.kid, "recon", "t"); err == nil {
		t.Fatal("RevokeBinding succeeded with a failing audit append")
	}
	restore()
	if n := e.count(`select count(*) from kontra.key_services where revoked_at is not null`); n != 0 {
		t.Error("a binding was revoked without its audit row")
	}
	before := len(e.verifyAudit())
	if err := e.store.RevokeBinding(ctx, k.kid, "recon", "kontra-gate enroll --revoke"); err != nil {
		t.Fatal(err)
	}
	if p := nextNotify(t, listen, 2*time.Second); p != "binding:"+k.kid+"/recon" {
		t.Errorf("NOTIFY %q", p)
	}
	if n := e.count(`select count(*) from kontra.key_services where key_id = $1 and service = 'recon' and revoked_at is not null`, k.kid); n != 1 {
		t.Error("recon's binding is not revoked")
	}
	if n := e.count(`select count(*) from kontra.key_services where key_id = $1 and service = 'hello' and revoked_at is null`, k.kid); n != 1 {
		t.Error("hello's binding was touched")
	}
	ev := e.verifyAudit()
	if len(ev) != before+1 || ev[before]["event"] != EventBindingRevoked || ev[before]["service"] != "recon" {
		t.Errorf("audit %v", ev[before:])
	}
	if err := e.store.RevokeBinding(ctx, k.kid, "recon", "t"); err != nil {
		t.Errorf("twice: %v", err)
	}
	if p := nextNotify(t, listen, 300*time.Millisecond); p != "" {
		t.Errorf("a second NOTIFY %q", p)
	}
	if err := e.store.RevokeBinding(ctx, k.kid, "nope", "t"); !errors.Is(err, ErrBindingNotFound) {
		t.Errorf("an unbound service: %v", err)
	}
	if err := e.store.RevokeBinding(ctx, "k1_aaaaaaaaaaaaaaaaaaaaaaaaaa", "hello", "t"); !errors.Is(err, ErrKeyNotFound) {
		t.Errorf("an unknown key: %v", err)
	}
}

// EndOverlaps revokes the rotated keys past overlap_until (audit row and NOTIFY each), and leaves the others.
func TestEndOverlaps(t *testing.T) {
	e := newPGEnv(t)
	ctx := context.Background()
	e.service(e.team, "hello", "a2a", false)
	due, later := e.enrolled("hello"), e.enrolled("hello")
	for _, old := range []*testKey{due, later} {
		nk := newTestKey(t)
		if _, err := e.admit(nk, hello(nk, a2a("hello")), old); err != nil {
			t.Fatal(err)
		}
	}
	e.exec(`update kontra.tunnel_keys set overlap_until = now() - interval '1 second' where key_id = $1`, due.kid)
	listen := e.listenRevoke()
	before := len(e.verifyAudit())
	ended, err := e.store.EndOverlaps(ctx)
	if err != nil || len(ended) != 1 || ended[0] != due.kid {
		t.Fatalf("EndOverlaps = %v %v", ended, err)
	}
	if p := nextNotify(t, listen, 2*time.Second); p != "key:"+due.kid {
		t.Errorf("NOTIFY %q", p)
	}
	if n := e.count(`select count(*) from kontra.tunnel_keys where key_id = $1 and revoked_at is not null and revoked_reason = 'rotated'`, due.kid); n != 1 {
		t.Error("the due key was not revoked")
	}
	if n := e.count(`select count(*) from kontra.tunnel_keys where key_id = $1 and revoked_at is null`, later.kid); n != 1 {
		t.Error("a key inside its overlap was revoked")
	}
	ev := e.verifyAudit()
	if len(ev) != before+1 || ev[before]["event"] != EventOverlapEnded || ev[before]["key_id"] != due.kid {
		t.Errorf("audit %v", ev[before:])
	}
	if ended, err := e.store.EndOverlaps(ctx); err != nil || len(ended) != 0 {
		t.Errorf("a second pass: %v %v", ended, err)
	}
}

// revocationsFor starts a view (and, with notify, a LISTEN connection routed to it) and waits for its
// first read; revs receives what it emits.
func (e *pgEnv) revocationsFor(interval time.Duration, notify bool) (*Revocations, chan Revocation) {
	e.t.Helper()
	r := NewRevocations(e.pool)
	r.interval = interval
	revs := make(chan Revocation, 64)
	r.Subscribe(func(rev Revocation) { revs <- rev })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{}, 2)
	e.t.Cleanup(func() {
		cancel()
		<-done
		if notify {
			<-done
		}
	})
	go func() { _ = r.Run(ctx); done <- struct{}{} }()
	var l *pg.Listener
	if notify {
		l = pg.NewListener(e.pool.Config().ConnConfig)
		l.OnChannel(pg.ChannelRevoke, r.Notify)
		l.OnReconnect(r.Catchup)
		go func() { _ = l.Run(ctx); done <- struct{}{} }()
	}
	deadline := time.Now().Add(10 * time.Second)
	for !r.Fresh() || (l != nil && !l.Healthy()) {
		if time.Now().After(deadline) {
			e.t.Fatal("the view never read")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return r, revs
}

func waitRevocation(t *testing.T, revs chan Revocation, within time.Duration) (Revocation, time.Duration) {
	t.Helper()
	start := time.Now()
	select {
	case rev := <-revs:
		return rev, time.Since(start)
	case <-time.After(within):
		t.Fatalf("no revocation within %s", within)
	}
	return Revocation{}, 0
}

// With NOTIFY, a subscribed view learns a revocation within 1 s (its re-read interval here is an hour, so
// only the notification can deliver it): a key, a binding, and a rotated key whose overlap the sweep ended.
// Between re-reads, a rotated key stops being usable at overlap_until.
func TestRevocationsNotify(t *testing.T) {
	e := newPGEnv(t)
	ctx := context.Background()
	e.service(e.team, "hello", "a2a", false)
	e.service(e.team, "recon", "a2a", false)
	k := e.enrolled("hello", "recon")
	other := e.enrolled("hello")
	old := e.enrolled("hello")
	nk := newTestKey(t)
	if _, err := e.admit(nk, hello(nk, a2a("hello")), old); err != nil {
		t.Fatal(err)
	}
	r, revs := e.revocationsFor(time.Hour, true)
	now := time.Now()
	if !r.KeyUsable(k.kid, now) || !r.KeyUsable(old.kid, now) || r.KeyUsable(old.kid, now.Add(RotationOverlap+time.Minute)) {
		t.Fatal("the first read: a rotated key inside its overlap is usable until overlap_until, and only until")
	}

	if err := e.store.RevokeBinding(ctx, k.kid, "recon", "t"); err != nil {
		t.Fatal(err)
	}
	rev, took := waitRevocation(t, revs, time.Second)
	if rev != (Revocation{KeyID: k.kid, Service: "recon"}) || !r.BindingRevoked(k.kid, "recon") || r.BindingRevoked(k.kid, "hello") ||
		!r.KeyUsable(k.kid, time.Now()) {
		t.Errorf("binding: %+v after %s", rev, took)
	}
	if err := e.store.RevokeKey(ctx, k.kid, ReasonOwner, "t"); err != nil {
		t.Fatal(err)
	}
	rev, took = waitRevocation(t, revs, time.Second)
	if rev != (Revocation{KeyID: k.kid, Reason: ReasonOwner}) || r.KeyUsable(k.kid, time.Now()) || !r.KeyUsable(other.kid, time.Now()) {
		t.Errorf("key: %+v after %s", rev, took)
	}

	// The overlap passes: the view stops using the key at its next re-read, before the sweep; the sweep's
	// NOTIFY is a revocation with reason rotated.
	e.exec(`update kontra.tunnel_keys set overlap_until = now() - interval '1 second' where key_id = $1`, old.kid)
	r.Catchup()
	if r.KeyUsable(old.kid, time.Now()) {
		t.Error("a key past its overlap is usable")
	}
	if ended, err := e.store.EndOverlaps(ctx); err != nil || len(ended) != 1 {
		t.Fatalf("EndOverlaps %v %v", ended, err)
	}
	rev, took = waitRevocation(t, revs, time.Second)
	if rev != (Revocation{KeyID: old.kid, Reason: ReasonRotated}) {
		t.Errorf("overlap ended: %+v after %s", rev, took)
	}
	// Nothing is emitted twice: the next re-read finds the same revocations.
	r.Catchup()
	select {
	case rev := <-revs:
		t.Errorf("emitted again: %+v", rev)
	case <-time.After(200 * time.Millisecond):
	}
}

// Without NOTIFY (lost), the re-read every 3 s (3/5 of 5 s) delivers a revocation within 5 s.
func TestRevocationsLostNotify(t *testing.T) {
	e := newPGEnv(t)
	ctx := context.Background()
	e.service(e.team, "hello", "a2a", false)
	k := e.enrolled("hello")
	b := e.enrolled("hello")
	r, revs := e.revocationsFor(RevocationRefresh, false)
	if err := e.store.RevokeKey(ctx, k.kid, ReasonOperator, "t"); err != nil {
		t.Fatal(err)
	}
	rev, took := waitRevocation(t, revs, RevocationRefresh)
	if rev != (Revocation{KeyID: k.kid, Reason: ReasonOperator}) || r.KeyUsable(k.kid, time.Now()) {
		t.Errorf("%+v after %s", rev, took)
	}
	if err := e.store.RevokeBinding(ctx, b.kid, "hello", "t"); err != nil {
		t.Fatal(err)
	}
	rev, took = waitRevocation(t, revs, RevocationRefresh)
	if rev != (Revocation{KeyID: b.kid, Service: "hello"}) || !r.BindingRevoked(b.kid, "hello") {
		t.Errorf("%+v after %s", rev, took)
	}
	if !r.Fresh() {
		t.Error("not fresh while re-reads succeed")
	}
}

// A binding enrolled again after its revocation is forgotten by the next re-read, and a NOTIFY for a binding
// whose row is not revoked marks nothing. Usable reads the rows themselves.
func TestRevocationsReenrolledBinding(t *testing.T) {
	e := newPGEnv(t)
	ctx := context.Background()
	e.service(e.team, "hello", "a2a", false)
	e.service(e.team, "recon", "a2a", false)
	k := e.enrolled("hello", "recon")
	r := NewRevocations(e.pool)
	r.Catchup()
	r.Notify("binding:" + k.kid + "/hello") // held at once (fail closed), then its row, not revoked, clears it
	if !r.BindingRevoked(k.kid, "hello") {
		t.Error("a binding NOTIFY is not held until its row is read")
	}
	r.drainers.Wait()
	if r.BindingRevoked(k.kid, "hello") {
		t.Error("an active binding marked revoked from a NOTIFY")
	}
	if ok, err := r.Usable(ctx, k.kid, "hello"); err != nil || !ok {
		t.Errorf("Usable(active) = %v %v", ok, err)
	}
	if err := e.store.RevokeBinding(ctx, k.kid, "hello", "t"); err != nil {
		t.Fatal(err)
	}
	r.Catchup()
	if !r.BindingRevoked(k.kid, "hello") {
		t.Fatal("the revoked binding is not held")
	}
	if ok, err := r.Usable(ctx, k.kid, "hello"); err != nil || ok {
		t.Errorf("Usable(revoked binding) = %v %v", ok, err)
	}
	if ok, err := r.Usable(ctx, k.kid, ""); err != nil || !ok {
		t.Errorf("Usable(key alone) = %v %v", ok, err)
	}
	h := hello(k, a2a("hello"), a2a("recon"))
	h.Enroll = []string{e.token(e.team, "hello")}
	e.mustAdmit(k, h)
	r.Catchup()
	if r.BindingRevoked(k.kid, "hello") {
		t.Error("a binding enrolled again is still held as revoked")
	}
	// A key whose row is gone (its tenant deleted) is revoked as far as the view knows.
	gone := "k1_aaaaaaaaaaaaaaaaaaaaaaaaaa"
	r.Notify("key:" + gone)
	r.drainers.Wait()
	if r.KeyUsable(gone, time.Now()) {
		t.Error("a NOTIFY for a key with no row left it usable")
	}
	if ok, err := r.Usable(ctx, gone, ""); err != nil || ok {
		t.Errorf("Usable(unknown) = %v %v", ok, err)
	}
	if err := e.store.RevokeKey(ctx, k.kid, ReasonOperator, "t"); err != nil {
		t.Fatal(err)
	}
	if ok, err := r.Usable(ctx, k.kid, "recon"); err != nil || ok {
		t.Errorf("Usable(revoked key) = %v %v", ok, err)
	}
}

// A binding revoked, enrolled again and revoked again is two revocations, and the view emits each, by
// NOTIFY or by re-read: an enrollment sends no NOTIFY, so the view may still hold the first revocation when
// the second arrives, and tells them apart by revoked_at.
func TestRevocationsBindingRevokedAgain(t *testing.T) {
	for _, notify := range []bool{true, false} {
		e := newPGEnv(t)
		ctx := context.Background()
		e.service(e.team, "hello", "a2a", false)
		e.service(e.team, "recon", "a2a", false)
		k := e.enrolled("hello", "recon")
		r, revs := e.revocationsFor(time.Hour, notify)
		want := Revocation{KeyID: k.kid, Service: "recon"}
		for i := range 3 {
			if err := e.store.RevokeBinding(ctx, k.kid, "recon", "t"); err != nil {
				t.Fatal(err)
			}
			if !notify {
				r.Catchup()
			}
			select {
			case rev := <-revs:
				if rev != want {
					t.Errorf("notify %v, revocation %d: %+v", notify, i+1, rev)
				}
			case <-time.After(2 * time.Second):
				t.Fatalf("notify %v: revocation %d of the same binding was never emitted", notify, i+1)
			}
			if !r.BindingRevoked(k.kid, "recon") {
				t.Errorf("notify %v, revocation %d: not held", notify, i+1)
			}
			h := hello(k, a2a("hello"), a2a("recon"))
			h.Enroll = []string{e.token(e.team, "recon")}
			if d := e.mustAdmit(k, h); len(d.Served) != 2 {
				t.Fatalf("re-enrolling recon: served %v", served(d))
			}
		}
		// The next re-read forgets the binding enrolled again, and emits nothing.
		r.Catchup()
		if r.BindingRevoked(k.kid, "recon") {
			t.Errorf("notify %v: the binding enrolled again is still held as revoked after a re-read", notify)
		}
		select {
		case rev := <-revs:
			t.Errorf("notify %v: emitted again: %+v", notify, rev)
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// Notify never waits on the database (pg.Listener runs every channel's callbacks on one goroutine): with the
// view's pool exhausted it returns at once, the key and the binding are refused at once (fail closed), and
// each revocation is emitted once, with the key's reason, when a connection is free.
func TestRevocationsNotifyDoesNotBlock(t *testing.T) {
	e := newPGEnv(t)
	ctx := context.Background()
	e.service(e.team, "hello", "a2a", false)
	k := e.enrolled("hello")
	b := e.enrolled("hello")
	cfg, err := pgxpool.ParseConfig(e.dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	r := NewRevocations(pool)
	revs := make(chan Revocation, 8)
	r.Subscribe(func(rev Revocation) { revs <- rev })
	r.Catchup()
	if err := e.store.RevokeKey(ctx, k.kid, ReasonOwner, "t"); err != nil {
		t.Fatal(err)
	}
	if err := e.store.RevokeBinding(ctx, b.kid, "hello", "t"); err != nil {
		t.Fatal(err)
	}
	conn, err := pool.Acquire(ctx) // the view's only connection
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	r.Notify(KeyRevokePayload(k.kid))
	r.Notify(BindingRevokePayload(b.kid, "hello"))
	took := time.Since(start)
	refused := !r.KeyUsable(k.kid, time.Now()) && r.BindingRevoked(b.kid, "hello")
	conn.Release()
	if took > 250*time.Millisecond {
		t.Errorf("Notify waited %s on an exhausted pool", took)
	}
	if !refused {
		t.Error("the key or the binding is usable until its row is read")
	}
	got := map[Revocation]int{}
	for range 2 {
		rev, _ := waitRevocation(t, revs, 3*time.Second)
		got[rev]++
	}
	for _, want := range []Revocation{{KeyID: k.kid, Reason: ReasonOwner}, {KeyID: b.kid, Service: "hello"}} {
		if got[want] != 1 {
			t.Errorf("emitted %v, want %+v once", got, want)
		}
	}
	r.Catchup()
	select {
	case rev := <-revs:
		t.Errorf("emitted again: %+v", rev)
	case <-time.After(200 * time.Millisecond):
	}
}
