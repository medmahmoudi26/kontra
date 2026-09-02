package kv

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// kv_clients_test.go — the tests that establish this is a SUBSTITUTION and not a lookalike.
//
// Everything in kv_test.go asserts bytes against a client written in the same file, which proves
// the store does what this package thinks it does. It cannot prove the thing that matters: that
// the three clients kontra actually ships — ioredis under `control/orchestrator/src/stateStore.ts`,
// go-redis under runtime/go, redis-py under runtime/python — connect, negotiate, and get right
// answers. Each of those libraries has a handshake this store never sees in a hand-written test,
// and two of them default to RESP3.
//
// So the tests below run the REAL clients, against unmodified source:
//
//  1. TestRealStateStoreReadsThisStore compiles `control/orchestrator/src/stateStore.ts` as-is and drives
//     its `createStateReader` through ioredis. Nothing in stateStore.ts is stubbed, patched, or
//     re-implemented — esbuild transpiles the file the API serves from.
//
//  2. TestActorkitSuitesPassAgainstThisStore runs the EXISTING integration suites that were
//     written against the container — runtime/go's rediskv and statekv tests and
//     tests/test_redis_kv.py — by pointing KONTRA_REDIS_HOST at this server. They already skip
//     when no store is reachable, which is exactly the hook needed, and they are the suites that
//     own the atomicity claims.
//
// (2) is behind KONTRA_KV_CROSS_SDK=1: it compiles another Go module and runs a Python venv,
// which is minutes and a lot of memory, and `go test ./cli/...` should stay quick. Run it with:
//
//	KONTRA_KV_CROSS_SDK=1 GOFLAGS=-p=1 go test -count=1 -run CrossSDK ./appliance/

// --- ioredis, through the real stateStore.ts ---------------------------------------------

// THE ACCEPTANCE TEST. stateStore.ts is the only orchestrator-side client of this tier, and it is
// the file this slice promised not to touch — so the proof has to be that the file, unmodified,
// reads the right answers off this server.
//
// What it exercises that a hand-written client does not:
//
//   - ioredis 6's handshake: HELLO 3 (it defaults to protocol 3), CLIENT SETINFO ×2, then the
//     `enableReadyCheck` INFO whose `loading` field gates the connection ever becoming ready.
//   - `lazyConnect` + `enableOfflineQueue: false`, so a command issued before the socket is open
//     fails rather than queueing — which makes the memoized `ensureConnected` load-bearing.
//   - The hash path (HGETALL + TTL in parallel) and the scan path (SCAN, then a PIPELINE of
//     N HGETs and N TTLs), including the RESP3 map frame HGETALL returns.
//   - THE ERROR LISTENER. stateStore.ts registers `client.on('error', () => {})` with a comment
//     saying why: without it, ioredis's EventEmitter turns a dropped socket into an uncaught
//     exception and kills the API over a diagnostic endpoint's dependency. The harness drops the
//     connection mid-life and asserts the process is still alive to report it.
//   - Durability, read by the real client: after a restart over the same data directory, a fresh
//     reader sees what the first one saw.
func TestRealStateStoreReadsThisStore(t *testing.T) {
	root := repoRoot(t)
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node on PATH")
	}
	esbuild := findEsbuild(t, root)
	// `backend/`, WHICH IS WHERE THE ORCHESTRATOR LIVES. It was `orchestrator/` until the tree
	// was renamed, and this join survived the rename because the directory name is a SEPARATE
	// ARGUMENT — nothing matching `orchestrator/` was here to rewrite. The result was worse than a
	// red test: `root/orchestrator` does not exist, so this skipped unconditionally and forever,
	// under a message that already said `backend/`. A test that cannot fail is not a test.
	backend := filepath.Join(root, "control", "orchestrator")
	if _, err := os.Stat(filepath.Join(backend, "node_modules", "ioredis")); err != nil {
		t.Skip("control/orchestrator/node_modules/ioredis is not installed (pnpm install)")
	}

	tmp := t.TempDir()
	bundle := filepath.Join(tmp, "stateStore.cjs")
	// --bundle with ioredis EXTERNAL: stateStore.ts and its `./state` import are compiled, the
	// client library is not, so the harness loads the same ioredis the orchestrator does.
	build := exec.Command(esbuild, filepath.Join(backend, "src", "stateStore.ts"),
		"--bundle", "--platform=node", "--format=cjs", "--external:ioredis", "--outfile="+bundle)
	build.Dir = backend
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("compiling stateStore.ts: %v\n%s", err, out)
	}
	harness := filepath.Join(tmp, "harness.cjs")
	if err := os.WriteFile(harness, []byte(stateStoreHarness), 0o600); err != nil {
		t.Fatal(err)
	}

	// --- the state a run would have left behind ---
	dir := t.TempDir()
	port := freeTestKVPort(t)
	s := startKVForTest(t, Options{DataDir: dir, Port: port})
	seed := dialKV(t, s.Address())
	// Tier 3, written the way global_state writes it: one hash per key, `data` + `ver`.
	for _, kv := range [][2]string{{"seen", `{"n":3}`}, {"frontier", `["a","b"]`}, {"note", "plain text"}} {
		seed.send("EVALSHA", casSHA, "1", "kontra-global:cachebuster:"+kv[0], kv[1], "")
	}
	// A different actor's global state, which must NOT appear in the read below.
	seed.send("EVALSHA", casSHA, "1", "kontra-global:otheractor:seen", `{"n":99}`, "")
	// Tiers 1+2: one hash, many fields, one TTL.
	seed.send("HSET", "kontra-actor:sess-1", "u0", `{"done":true}`, "u0-ckpt", `{"cursor":7}`, "s-name", `"crawl"`)
	seed.send("EXPIRE", "kontra-actor:sess-1", "86400")

	cmd := exec.Command(node, harness)
	cmd.Dir = backend
	cmd.Env = append(os.Environ(),
		"KONTRA_REDIS_HOST="+s.Address(),
		"NODE_PATH="+filepath.Join(backend, "node_modules"),
		"KONTRA_KV_BUNDLE="+bundle,
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = &testWriter{t}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()

	lines := bufio.NewScanner(stdout)
	lines.Buffer(make([]byte, 1<<20), 1<<20)
	next := func(tag string) map[string]any {
		t.Helper()
		for lines.Scan() {
			line := lines.Text()
			if !strings.HasPrefix(line, "{") {
				t.Logf("node: %s", line)
				continue
			}
			var m map[string]any
			if err := json.Unmarshal([]byte(line), &m); err != nil {
				t.Fatalf("node emitted %q: %v", line, err)
			}
			if m["phase"] != tag {
				t.Fatalf("expected phase %q, got %v", tag, m)
			}
			return m
		}
		t.Fatalf("node exited before phase %q", tag)
		return nil
	}

	// --- the global tier: SCAN, then a pipeline of HGET+TTL ---
	got := next("global")
	if src, _ := got["source"].(string); src != "redis://"+s.Address() {
		t.Errorf("source = %q, want redis://%s", src, s.Address())
	}
	entries := entriesByKey(t, got)
	if len(entries) != 3 {
		t.Fatalf("global read returned %d entries, want 3 (and none of the other actor's): %v", len(entries), entries)
	}
	// The prefix is stripped, the value is JSON-parsed, and the TTL is -1 because tier 3 has none.
	if v := entries["seen"]; fmt.Sprint(v["value"]) != "map[n:3]" || fmt.Sprint(v["ttl"]) != "-1" {
		t.Errorf("entry `seen` = %v, want the parsed object with ttl -1", v)
	}
	if v := entries["note"]; v["value"] != "plain text" {
		t.Errorf("a non-JSON value came back as %v, want the raw string", v["value"])
	}
	if got["truncated"] != false {
		t.Errorf("truncated = %v on a three-key read", got["truncated"])
	}

	// ioredis 6 defaults to `protocol: 3` and only falls back when HELLO ERRORS. That the
	// negotiation SUCCEEDED is the fact worth pinning: a store that refused HELLO would still
	// pass every assertion here — ioredis would quietly downgrade — while redis-py 8, whose
	// `on_connect` reads the HELLO reply with no try/except, would fail to connect at all.
	if n := s.resp3.Load(); n == 0 {
		t.Fatal("ioredis fell back to RESP2, so HELLO 3 was refused; redis-py would not have connected at all")
	}

	// --- one named global key: SCAN with an exact pattern ---
	got = next("global-one")
	entries = entriesByKey(t, got)
	if len(entries) != 1 || entries["frontier"] == nil {
		t.Fatalf("a named global read returned %v, want only `frontier`", entries)
	}

	// --- the actor tier: HGETALL + TTL ---
	got = next("actor")
	entries = entriesByKey(t, got)
	if len(entries) != 3 {
		t.Fatalf("actor read returned %d entries, want 3: %v", len(entries), entries)
	}
	for _, field := range []string{"u0", "u0-ckpt", "s-name"} {
		e := entries[field]
		if e == nil {
			t.Fatalf("field %q is missing from the actor read: %v", field, entries)
		}
		// Every field shares the HASH's TTL, which is the whole reason tiers 1+2 are a hash.
		if ttl, _ := e["ttl"].(float64); ttl < 86_000 || ttl > 86_400 {
			t.Errorf("field %q reports ttl %v, want the hash's ~86400", field, ttl)
		}
	}

	// --- a named field: HGET ---
	got = next("actor-one")
	entries = entriesByKey(t, got)
	if len(entries) != 1 || fmt.Sprint(entries["u0"]["value"]) != "map[done:true]" {
		t.Fatalf("a named actor read returned %v", entries)
	}

	// --- THE DROP. The API must survive losing its state store. ---
	if err := s.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	fmt.Fprintln(stdin, "DROP")
	got = next("dropped")
	if got["uncaught"] != nil {
		t.Fatalf("the dropped connection became an uncaught exception in the Node process: %v", got["uncaught"])
	}
	if got["rejected"] != true {
		t.Fatalf("a read against a stopped store did not reject: %v", got)
	}
	t.Logf("the drop surfaced as a rejected promise: %v", got["error"])

	// --- the restart, read by the real client ---
	restarted := startKVForTest(t, Options{DataDir: dir, Port: port})
	if restarted.Address() != s.Address() {
		t.Fatalf("the restarted store moved to %s", restarted.Address())
	}
	fmt.Fprintln(stdin, "RESTART")
	got = next("after-restart")
	entries = entriesByKey(t, got)
	if len(entries) != 3 || fmt.Sprint(entries["seen"]["value"]) != "map[n:3]" {
		t.Fatalf("after a restart the real client read %v, want the three keys written before it", entries)
	}

	stdin.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("the node harness exited with %v — a clean exit is part of the claim that nothing threw", err)
	}
}

func entriesByKey(t *testing.T, reply map[string]any) map[string]map[string]any {
	t.Helper()
	raw, ok := reply["entries"].([]any)
	if !ok {
		t.Fatalf("reply has no entries: %v", reply)
	}
	out := map[string]map[string]any{}
	for _, e := range raw {
		m, _ := e.(map[string]any)
		key, _ := m["key"].(string)
		out[key] = m
	}
	return out
}

type testWriter struct{ t *testing.T }

func (w *testWriter) Write(p []byte) (int, error) {
	w.t.Logf("node stderr: %s", strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// findEsbuild locates the transpiler pnpm installed for the orchestrator. It is used rather than
// `tsc` because it needs no project configuration and takes milliseconds, and rather than the
// checked-out `dist/` because a stale build would make this test assert against code the API is
// no longer running.
func findEsbuild(t *testing.T, root string) string {
	t.Helper()
	matches, _ := filepath.Glob(filepath.Join(root, "control", "orchestrator", "node_modules", ".pnpm",
		"esbuild@*", "node_modules", "esbuild", "bin", "esbuild"))
	if len(matches) == 0 {
		t.Skip("no esbuild under control/orchestrator/node_modules (pnpm install)")
	}
	return matches[0]
}

// stateStoreHarness drives the compiled stateStore.ts and reports each phase as one JSON line.
//
// It installs an `uncaughtException` handler that RECORDS rather than exits, because the thing
// being tested is whether one ever fires: a handler that swallowed it silently would make the
// drop phase pass for the wrong reason.
const stateStoreHarness = `
'use strict';
const readline = require('node:readline');
const { createStateReader } = require(process.env.KONTRA_KV_BUNDLE);

let uncaught = null;
process.on('uncaughtException', (err) => { uncaught = String(err && err.message || err); });
process.on('unhandledRejection', (err) => { uncaught = 'unhandledRejection: ' + String(err && err.message || err); });

const emit = (phase, extra) => process.stdout.write(JSON.stringify({ phase, ...extra }) + '\n');

const waitFor = (rl, want) => new Promise((resolve, reject) => {
  const onLine = (line) => {
    if (line.trim() === want) { rl.off('line', onLine); resolve(); }
  };
  rl.on('line', onLine);
  rl.once('close', () => reject(new Error('stdin closed while waiting for ' + want)));
});

(async () => {
  const rl = readline.createInterface({ input: process.stdin });
  const reader = createStateReader();
  if (!reader) throw new Error('createStateReader returned null: KONTRA_REDIS_HOST is unset');

  emit('global', await reader.read('global', 'cachebuster', ''));
  emit('global-one', await reader.read('global', 'cachebuster', '', 'frontier'));
  emit('actor', await reader.read('actor', 'cachebuster', 'sess-1'));
  emit('actor-one', await reader.read('actor', 'cachebuster', 'sess-1', 'u0'));

  await waitFor(rl, 'DROP');
  let rejected = false;
  let message = null;
  try {
    await reader.read('global', 'cachebuster', '');
  } catch (err) {
    rejected = true;
    message = String(err && err.message || err);
  }
  // Give the EventEmitter a few turns to deliver whatever it was going to deliver, so an
  // uncaught exception has actually had its chance to fire before this is called clean.
  await new Promise((r) => setTimeout(r, 250));
  emit('dropped', { rejected, error: message, uncaught });
  await reader.close().catch(() => {});

  await waitFor(rl, 'RESTART');
  const second = createStateReader();
  emit('after-restart', await second.read('global', 'cachebuster', ''));
  await second.close().catch(() => {});
  rl.close();
  process.exit(uncaught ? 1 : 0);
})().catch((err) => {
  process.stderr.write('harness failed: ' + (err && err.stack || err) + '\n');
  process.exit(1);
});
`

// --- go-redis and redis-py, through the suites that already own these claims ---------------

// The SDK-side suites were written against the container and skip when no store answers a PING.
// Pointing them at this server is the whole test: if `TestConcurrentCreateHasExactlyOneWinner`
// passes here, the Lua-shaped atomic this store reimplements in Go is genuinely atomic, and if
// the statekv suite passes then a Go actor's commit map round-trips.
//
// Behind KONTRA_KV_CROSS_SDK=1 because it compiles another module and runs a venv.
func TestCrossSDKSuitesPassAgainstThisStore(t *testing.T) {
	if os.Getenv("KONTRA_KV_CROSS_SDK") != "1" {
		t.Skip("set KONTRA_KV_CROSS_SDK=1 to run the runtime/{go,python} suites against the embedded store")
	}
	root := repoRoot(t)
	s := startKVForTest(t, Options{})

	for _, c := range []struct {
		name string
		dir  string
		bin  string
		args []string
		env  []string
	}{
		{
			name: "go-redis/rediskv", dir: filepath.Join(root, "runtime", "go"), bin: "go",
			args: []string{"test", "-count=1", "-p=1", "./rediskv/"},
			env:  []string{"GOWORK=off", "GOFLAGS=-p=1"},
		},
		{
			name: "go-redis/statekv", dir: filepath.Join(root, "runtime", "go"), bin: "go",
			args: []string{"test", "-count=1", "-p=1", "./statekv/"},
			env:  []string{"GOWORK=off", "GOFLAGS=-p=1"},
		},
		{
			name: "redis-py", dir: root, bin: filepath.Join(root, ".venv", "bin", "python"),
			args: []string{"-m", "pytest", "-q", "tests/test_redis_kv.py"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			bin := c.bin
			if !filepath.IsAbs(bin) {
				p, err := exec.LookPath(bin)
				if err != nil {
					t.Skipf("no %s on PATH", bin)
				}
				bin = p
			} else if _, err := os.Stat(bin); err != nil {
				t.Skipf("%s is not installed", bin)
			}
			ctx, cancel := contextWithTimeout(10 * time.Minute)
			defer cancel()
			cmd := exec.CommandContext(ctx, bin, c.args...)
			cmd.Dir = c.dir
			cmd.Env = append(append(os.Environ(), "KONTRA_REDIS_HOST="+s.Address()), c.env...)
			out, err := cmd.CombinedOutput()
			t.Logf("%s\n%s", strings.Join(append([]string{bin}, c.args...), " "), out)
			if err != nil {
				t.Fatalf("%s failed against the embedded store: %v", c.name, err)
			}
			// A suite that SKIPPED proves nothing, and these skip silently when no store answers.
			if strings.Contains(string(out), "no Redis at") || strings.Contains(string(out), "no reachable Redis") {
				t.Fatalf("%s skipped — it never reached the embedded store", c.name)
			}
		})
	}

	// THE ONE PATH NO EXISTING SUITE COVERS. tests/test_redis_kv.py exercises tier 3 from Python
	// and runtime/go's statekv suite exercises tiers 1+2 from Go, but nothing drives
	// sdk/python's ActorStateKV against a live store — so the Python half of the actor commit
	// map has never had an integration test at all. It is written here rather than left as a gap,
	// because it is the tier a run writes on EVERY unit and the one whose loss duplicates work
	// rather than failing it.
	t.Run("redis-py/statekv", func(t *testing.T) {
		py := filepath.Join(root, ".venv", "bin", "python")
		if _, err := os.Stat(py); err != nil {
			t.Skip(".venv/bin/python is not installed")
		}
		script := filepath.Join(t.TempDir(), "statekv_probe.py")
		if err := os.WriteFile(script, []byte(pythonStateKVProbe), 0o600); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := contextWithTimeout(2 * time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, py, script)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "KONTRA_REDIS_HOST="+s.Address())
		out, err := cmd.CombinedOutput()
		t.Logf("%s", out)
		if err != nil {
			t.Fatalf("sdk/python's ActorStateKV failed against the embedded store: %v", err)
		}
	})
}

// pythonStateKVProbe drives sdk/python's REAL ActorStateKV — the class the engine commits
// through — over redis-py, asserting the same round trip runtime/go's statekv suite asserts.
const pythonStateKVProbe = `
import asyncio, os, sys, uuid
sys.path.insert(0, os.path.join(os.getcwd(), "runtime", "python"))
from internals.statekv import ActorStateKV, actor_key, redis_client_factory, STATE_TTL_S

async def main():
    actor = "probe-" + uuid.uuid4().hex
    kv = ActorStateKV(redis_client_factory(), actor, ttl_s=STATE_TTL_S)
    c = redis_client_factory()()

    assert actor_key(actor) == "kontra-actor:" + actor, actor_key(actor)

    # None is a REAL stored value: a unit can commit null, so absence must be the field being
    # missing rather than a sentinel.
    assert await kv.get("u0", "MISSING") == "MISSING"
    await kv.set("u0", None)
    assert await kv.get("u0", "MISSING") is None
    await kv.set("u1", {"done": True, "n": 3})
    assert await kv.get("u1") == {"done": True, "n": 3}

    # One EXPIRE slides the whole hash, which is why tiers 1+2 are a hash.
    ttl = await c.ttl(actor_key(actor))
    assert 86000 < ttl <= 86400, ttl

    assert await kv.delete("u0") is True
    assert await kv.delete("u0") is False
    await kv.touch()
    assert await c.ttl(actor_key(actor)) > 86000

    # Drop is what a completed batch does; the hash must be GONE, not empty.
    await kv.drop()
    assert await c.exists(actor_key(actor)) == 0
    assert await c.ttl(actor_key(actor)) == -2
    await c.aclose()
    # ActorStateKV builds its own client lazily; close it too, or asyncio's GC prints an
    # "Event loop is closed" traceback that reads like a failure and is not one.
    await kv._client().aclose()
    print("sdk/python ActorStateKV: ok against", os.environ["KONTRA_REDIS_HOST"])

asyncio.run(main())
`

func contextWithTimeout(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}
