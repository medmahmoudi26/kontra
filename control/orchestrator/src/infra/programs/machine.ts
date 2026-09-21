/**
 * Turning a bare Machine into a Worker — natively, with no Docker anywhere (ADR 0019).
 *
 * ═══ EVERY PATH IN HERE IS NAMED AFTER ITS WORKER, AND THAT IS SLICE 11 ═══
 *
 * ADR 0037: *"Several Workers may share a Machine, and they share its egress address."* Until slice
 * 11 this script wrote SINGLETONS — `/opt/kontra`, `/etc/kontra/worker.env`, `kontra-actor.service`
 * — so a second placement onto one **Machine** did not pack, it OVERWROTE: step 3's
 * `rm -rf "$ROOT/actor"` deleted the other **Worker**'s code, the unit file replaced its unit, and
 * the second install would have reported success. Packing is therefore not a scheduling change with
 * a shell script along for the ride; the namespacing IS the feature. Everything a **Worker** owns
 * now carries its actor's name:
 *
 *   /opt/kontra/w/<actor>/            the Bundle, unpacked. `rm -rf` inside it reaches nothing else
 *   /etc/kontra/worker-<actor>.env    its environment
 *   kontra-actor-<actor>.service      its two units
 *   kontra-handler-<actor>.service
 *   /etc/kontra/scrape.d/<actor>.json its metrics target (see `metricsPort`)
 *
 * WHAT IS STILL PER-MACHINE IS PER-MACHINE ON PURPOSE: the vmagent binary, its unit and its config,
 * because one agent scraping N targets is what a Machine needs and N agents remote-writing the same
 * series is not. `machineTeardown` therefore stops a **Worker**'s units and NOT vmagent — a teardown
 * that stopped it would take the co-tenant's metrics down with it, silently, which is the shape of
 * failure this whole slice is about.
 *
 * THE SINGLETON UNITS ARE DISARMED HERE, NOT ONLY ON TEARDOWN (step 8). A **Machine** placed before
 * this change carries `kontra-actor.service` under the OLD layout, and Pulumi's ordering between
 * "create the new resource" and "delete the old one" is not this file's to assume. Disarming them at
 * INSTALL time makes the transition order-independent, and it is safe for exactly one reason worth
 * stating: under the old layout a **Machine** could only ever hold ONE **Worker**, so those unit
 * names can never belong to a co-tenant.
 *
 * A container on a fleet Machine was a layer that earned nothing. The Machine is already the
 * isolation boundary — a fleet Machine's value is its unique egress address — so the container
 * bought an image registry, an insecure-registry exception in every Machine's Docker daemon, a
 * Docker install in cloud-init, and a supervision model (`--restart unless-stopped`) weaker than the
 * one the OS already ships.
 *
 * What replaces it is a **Bundle**: the actor's code, the actorkit seam and the handler binary,
 * identified by the sha256 of its own bytes, fetched from the Controller and VERIFIED on arrival.
 * That is the whole of what a Machine needs since ADR 0018 — the sidecar and its placement service
 * used to be installed here from a pinned upstream release, and both are gone: the actor process
 * polls Temporal directly, so a Machine now runs exactly the two processes that do the work.
 *
 * WHERE IT IS FETCHED FROM CHANGED AND HOW IT IS FETCHED DID NOT (ADR 0036). A Bundle is an OCI
 * artifact now, so `BUNDLE_URL` is the distribution spec's blob endpoint on the Controller's
 * registry rather than an object key on its store. It is still one anonymous GET of one tar.gz
 * whose sha is checked before anything is unpacked — `curl` and `sha256sum`, which this script
 * already installs. NOTHING OCI-AWARE RUNS ON A MACHINE, and that is a decision, not an
 * omission: putting `oras` here would add a runtime dependency to every Machine in every Fleet so
 * that it could make the request `curl` already makes. Resolving the tag, verifying the manifest
 * and reading the engine out of it happen once per run in the control plane, where the
 * placement is decided (`control/orchestrator/src/activities/fleet.ts:resolveBundle`).
 *
 * WHY THIS SCRIPT IS BUILT HERE AND NOT SENT BY THE CLIENT. It runs as root on every Machine in
 * the Fleet. If the CLI passed it as an argument, anyone holding the infra token could execute
 * arbitrary root commands across the fleet — a far larger authority than "may provision
 * machines". The route picks WHICH stack; it never picks WHAT the stack contains. So the caller
 * supplies six narrow, validated values and this file decides what runs.
 */

import * as pulumi from '@pulumi/pulumi';

export interface MachineActor {
  /** Actor name — also the systemd unit description and the /opt/kontra/actor/<name> path. */
  name: string;
  version: string;
  /** `py` runs actor.py under python; `go` runs the compiled binary beside its manifest. */
  engine: 'py' | 'go';
  /** Where the Bundle's bytes are: the registry's blob endpoint for its one layer, on the VPC. */
  bundleUrl: string;
  /** sha256 of the Bundle, bare hex. Checked on the Machine; a mismatch fails the placement.
   *  It is also the last path segment of `bundleUrl` — see validateMachineActor. */
  bundleSha: string;
  /** Where the Machine calls home: Temporal, the catalog, S3, Redis. */
  controller: string;
  /** The fleet's label, stamped on every metric so a per-Machine failure is attributable.
   *  Called `role` until it was renamed for claiming behaviour it never had — see FleetArgs.tag. */
  tag: string;
  /**
   * Live Sessions this Machine will hold at once — `KONTRA_MAX_PARALLEL_SESSIONS`.
   *
   * DENSITY, as opposed to `FleetArgs.machines` (how many Machines) and `PlacementArgs.workers`
   * (how many of them one Artifact lands on). It is the only knob that says how much work ONE
   * Worker takes concurrently, and since packing it is also the honest answer to "more throughput
   * for this Artifact on the Machines I have" — a second Worker of one `<actor>@<version>` on one
   * Machine is not expressible (ADR 0037; see `PlacementArgs.workers`).
   *
   * It was unreachable until now, and silently: both hosts default to 4
   * (`internals/temporal/host.py:max_parallel_sessions`, `temporalhost/host.go`), nothing wrote
   * the variable here, and a host at its cap REFUSES an open — retryably, so the task returns to
   * the shared queue and another Machine takes it. A fleet asked for more concurrency than 4×N
   * therefore did not fail, it just queued, which is indistinguishable from slow targets.
   * Omitted leaves the variable unset and the hosts on their own default.
   */
  maxSessions?: number;

  /**
   * Where this Worker's actor host serves its counters — `KONTRA_METRICS_ADDR`, loopback only.
   *
   * ═══ PACKING BREAKS A SHARED PORT SILENTLY, WHICH IS WHY THIS IS AN ARGUMENT ═══
   *
   * Both hosts default to :9110 and a **Machine** now holds several **Workers** (ADR 0037). MEASURED
   * by reading the host: `runtime/python/internals/metrics.py:serve` catches the bind failure, prints
   * one line to the actor's own journal and CARRIES ON — *"a metrics listener must never take the
   * actor down"*. So a second packed **Worker** would run perfectly, serve nothing, and be judged by
   * the **Warden** as `cannot tell` for ever (`cli/warden/sickworker.go`), with the round-3 failure this
   * repo already paid for invisible on exactly the Machines carrying the most work.
   *
   * `cli/warden/sickworker.go:metricsAddress` states the same fact from the other side for the `process`
   * driver: *"every Worker on the Machine would serve :9110 on the same loopback and the first one to
   * bind wins … `KONTRA_METRICS_ADDR` in the Worker's own environment is the one thing that makes it
   * answerable"*. This is that variable, chosen by the thing that knows how many Workers a Machine
   * is being given. Omitted leaves the host on its default, which is right for a Machine holding one.
   */
  metricsPort?: number;
}

/** The lowest port a packed Worker's counters are served on — both hosts' own default. */
export const METRICS_PORT_BASE = 9110;

/** Where a Worker's Bundle is unpacked. Named after the actor so `rm -rf` reaches nothing else. */
export function machineRoot(name: string): string {
  return `/opt/kontra/w/${name}`;
}

/** A Worker's two systemd units. */
export function machineUnits(name: string): { actor: string; handler: string } {
  return { actor: `kontra-actor-${name}.service`, handler: `kontra-handler-${name}.service` };
}

/** Everything interpolated into a root shell script is bounded first. A value that reaches
 * `sh -c` unvalidated is a command, not a string. */
const SAFE = {
  name: /^[a-z][a-z0-9-]{0,31}$/,
  version: /^[A-Za-z0-9][A-Za-z0-9._-]{0,31}$/,
  sha: /^[a-f0-9]{64}$/,
  host: /^[A-Za-z0-9][A-Za-z0-9.-]{0,253}$/,
  url: /^https?:\/\/[A-Za-z0-9.:_/-]+$/,
};

export function validateMachineActor(a: MachineActor): void {
  const bad = (f: string, v: string) =>
    new Error(`machine actor ${f}=${JSON.stringify(v)} is not safe to place on a Machine`);
  if (!SAFE.name.test(a.name)) throw bad('name', a.name);
  if (!SAFE.version.test(a.version)) throw bad('version', a.version);
  if (a.engine !== 'py' && a.engine !== 'go') throw bad('engine', a.engine);
  if (!SAFE.sha.test(a.bundleSha)) throw bad('bundleSha', a.bundleSha);
  if (!SAFE.url.test(a.bundleUrl)) throw bad('bundleUrl', a.bundleUrl);
  // THE URL AND THE SHA ARE ONE PAIR AND THIS IS WHERE THAT BECOMES CHECKABLE. An OCI blob is
  // addressed BY its digest, so the URL ends with the very value the Machine is about to verify
  // the download against — and a placement whose two halves disagree can be refused here instead
  // of after 65 MiB has been pulled onto every Machine at once. Under the object store these were
  // two independent strings and nothing could tell a mismatched pair from a correct one.
  if (!a.bundleUrl.endsWith(a.bundleSha)) {
    throw new Error(
      `machine actor bundleUrl ${JSON.stringify(a.bundleUrl)} does not serve bundleSha ` +
        `${JSON.stringify(a.bundleSha)} — an OCI blob is addressed by its digest, so a URL that ` +
        `ends with a different one is a placement that would fail its sha check on every Machine`
    );
  }
  if (!SAFE.host.test(a.controller)) throw bad('controller', a.controller);
  if (!SAFE.name.test(a.tag)) throw bad('tag', a.tag);
  // A count, not a string — it is interpolated into a root shell script like everything else
  // here, so "4; rm -rf /" must not survive being a number-typed field in TypeScript. The upper
  // bound is the measured working range: a Session is a whole worker process, and 20-40 units
  // per node is where this fleet performs (see SAFE_PAGE_MAX in the caller SDK).
  if (a.maxSessions !== undefined) {
    if (!Number.isInteger(a.maxSessions) || a.maxSessions < 1 || a.maxSessions > 64) {
      throw bad('maxSessions', String(a.maxSessions));
    }
  }
  // A PORT, and it is interpolated into a root shell script exactly like everything else here, so
  // `9110; rm -rf /` must not survive being a number-typed field in TypeScript. Above 1024 because
  // the actor host does not run as root inside its own namespace and a privileged port is a request
  // that would fail at bind time — silently, per `metricsPort`'s own note.
  if (a.metricsPort !== undefined) {
    if (!Number.isInteger(a.metricsPort) || a.metricsPort < 1024 || a.metricsPort > 65535) {
      throw bad('metricsPort', String(a.metricsPort));
    }
  }
}

/**
 * The units.
 *
 * Separate units, not one supervisor script — that is the point of being on a Machine. Each
 * process gets its own restart policy, its own journal, and an ordering the OS enforces.
 *
 * Two of them are the Worker (ADR 0018): the actor, which polls `{actor}-{version}-sessions`, and
 * the handler, which owns the workflow on the shared queue. There were four before, and the two
 * that went were a sidecar and its placement service, both serving a runtime Temporal provides.
 */
function units(a: MachineActor): Record<string, string> {
  const root = machineRoot(a.name);
  const names = machineUnits(a.name);
  const host =
    a.engine === 'go'
      ? `${root}/actor/${a.name}/${a.name}`
      : `/usr/bin/python3 ${root}/actor/${a.name}/actor.py`;

  return {
    [names.actor]: `[Unit]
Description=kontra actor host (${a.name}@${a.version})
After=network-online.target
Wants=network-online.target

[Service]
EnvironmentFile=/etc/kontra/worker-${a.name}.env
WorkingDirectory=${root}
ExecStart=${host}
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target`,

    // The handler is what `kontra workers list` counts. Without it the actor is up and
    // invisible, which is the most confusing state a Worker can be in — and `Requires=` makes the
    // converse true too: a handler alive without its actor would accept workflows whose RunBatch
    // nothing is polling for, so they would sit on the sessions queue until StartToClose.
    [names.handler]: `[Unit]
Description=kontra Temporal handler (${a.name}@${a.version})
After=${names.actor}
Requires=${names.actor}

[Service]
EnvironmentFile=/etc/kontra/worker-${a.name}.env
ExecStart=${root}/bin/handler
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target`,

    // Scrapes localhost and remote-writes to the Controller, because a fleet Machine accepts
    // no inbound connections and therefore cannot be scraped. `instance` is stamped with the
    // Machine's own hostname so a per-Machine failure is attributable — which is how the two
    // sick Machines in round 3 were eventually found.
    //
    // ONE AGENT PER MACHINE, N TARGETS — and its environment is the MACHINE's, not a Worker's.
    // It read `/etc/kontra/worker.env` and stamped `actor=${KONTRA_ACTOR_NAME}` on every series,
    // which was true while a Machine held one Worker and is a LIE the moment it packs: whichever
    // Worker's env file happened to be named would label the other one's counters as its own.
    // So the `actor` label comes from the target itself now (see `/etc/kontra/scrape.d`), and this
    // unit reads a file that says only what is true of the whole Machine.
    'kontra-vmagent.service': `[Unit]
Description=kontra metrics agent (scrape this Machine's Workers, push to the Controller)
After=network-online.target
Wants=network-online.target
ConditionPathExists=/opt/kontra/bin/vmagent

[Service]
EnvironmentFile=/etc/kontra/vmagent.env
ExecStart=/opt/kontra/bin/vmagent \
  -promscrape.config=/etc/kontra/vmagent.yml \
  -remoteWrite.url=http://\${KONTRA_CONTROLLER}:8428/api/v1/write \
  -remoteWrite.label=instance=%H \
  -remoteWrite.label=tag=\${KONTRA_TAG}
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target`,

    // The SAME SHAPE as the agent above, for the other signal (ADR 0050 §1). Workers accept no
    // inbound connections, so logs leave the same way metrics do: an agent on the Machine, pushing
    // outward. It reads journald directly, so `workflow.logger` writing to a host's stdout becomes
    // a journal entry becomes a shipped record — WITH NO WORKFLOW-SIDE CHANGE. The Temporal sandbox
    // forbids I/O; it does not forbid logging.
    //
    // THE DISK BUFFER IS THE REQUIREMENT, NOT A NICETY, and it is why this is vlagent rather than
    // something hand-rolled. The Machine whose last words matter most is the one that is failing or
    // about to be destroyed when its Lease drops — exactly when the Controller is least likely to be
    // answering. `-remoteWrite.tmpDataPath` buffers to disk across that window and
    // `-remoteWrite.maxDiskUsagePerURL` bounds it, so a Machine that cannot reach the Controller
    // neither loses lines nor fills its own disk and takes the Worker down with it.
    //
    // THE UNIT LIST IS A PREFIX MATCH ON PURPOSE. A packed Machine runs N Workers as
    // `kontra-actor-<name>` / `kontra-handler-<name>`, so naming units individually would need this
    // file rewritten per Worker — the same mistake the scrape config already avoids with a
    // directory. `_SYSTEMD_UNIT` globs cover every Worker a Machine ever packs, including ones
    // placed after this agent started.
    'kontra-vlagent.service': `[Unit]
Description=kontra logs agent (journald on this Machine, pushed to the Controller)
After=network-online.target
Wants=network-online.target
ConditionPathExists=/opt/kontra/bin/vlagent

[Service]
EnvironmentFile=/etc/kontra/vmagent.env
ExecStart=/opt/kontra/bin/vlagent \\
  -syslog.listenAddr.tcp= \\
  -journald \\
  -journald.matches='_SYSTEMD_UNIT=kontra-actor-*.service _SYSTEMD_UNIT=kontra-handler-*.service' \\
  -journald.streamFields='_SYSTEMD_UNIT,_HOSTNAME' \\
  -remoteWrite.url=http://\${KONTRA_CONTROLLER}:9428/internal/insert \\
  -remoteWrite.label=machine=%H \\
  -remoteWrite.label=tag=\${KONTRA_TAG} \\
  -remoteWrite.tmpDataPath=/var/lib/kontra/vlagent \\
  -remoteWrite.maxDiskUsagePerURL=512MB
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target`,

    // THERE IS NO WATCHDOG UNIT HERE ANY MORE, AND NO TIMER (ADR 0037). See the block above
    // `machineInstall` for what it did, what it could not do, and what took the duty.
    //
    // THERE IS NO tmux UNIT HERE ANY MORE (ADR 0020). A Machine's attachable session is converged
    // by `tmuxSessionWorkflow` on the infra queue, on demand, and this file no longer installs
    // tmux at all.
    //
    // What that deletes is a silent failure, not just a unit: the unit was
    // `ConditionPathExists=/usr/bin/tmux` beside an apt install this script was allowed to fail, so
    // a Machine could deploy "successfully" and never be viewable, with nothing in the deploy
    // output saying so. It also means a Machine deployed WITHOUT `--tmux` can be given a Terminal
    // on demand, with no re-deploy — which is why the workflow owns sessions rather than systemd.
    //
    // The invariant the old comment argued for still holds, and is now load-bearing rather than
    // stylistic: the Worker's two halves stay under systemd and only their JOURNALS go in panes.
    // ADR 0020's finding (2) measured a stalled viewer segfaulting a tmux server, which destroys
    // every session on that socket — so a Dashboard viewer can cost a Machine its view, and must
    // never be able to cost it its Worker. The thing that restarts a Worker that is sick but not
    // dead is the **Warden** now (ADR 0037), and it stops the Worker rather than the session; a
    // session must not be allowed to become what supervises anything.
    //
    // `kontra-tmux.service` is no longer named by any teardown this file writes — a teardown names
    // ONE Worker's units now, because a constant one would stop a co-tenant (see `machineTeardown`).
    // A Machine placed before ADR 0020 still carries the unit, and the resource that placed it
    // carries the OLD teardown in Pulumi's state, which is what runs when it is deleted.
  };
}

/**
 * Render the install. Idempotent throughout: a re-deploy runs it again on a Machine that
 * already has Chromium, and a half-applied Machine is worse than an unconfigured one.
 *
 * ═══ THE WATCHDOG IS GONE, AND IT HAD NEVER WORKED ═══
 *
 * Until ADR 0037 this file installed `/opt/kontra/bin/watchdog.sh` and a five-minute
 * `kontra-watchdog.timer`, and its header called it "the one that matters" — the round-3 run,
 * where one Machine failed **81 of 82 resource loads** and another 19 of 20 while their peers sat at
 * zero, each silently eating about an eighth of a sweep, and the runs still reported `completed`.
 * The script read the actor host's journal:
 *
 *     fails=$(… | grep -ci 'unit failed\|SessionLost\|engine dead')
 *     oks=$(…   | grep -ci 'unit ok\|committed')
 *     total=$((fails + oks));  [ "$total" -ge 10 ] || exit 0
 *
 * SWEPT 2026-08-30, with `grep -rn -a` over `runtime/`, `sdk/` and `handler/`: **`unit failed`,
 * `unit ok` and `engine dead` appear nowhere in this repo**, and every occurrence of `committed` is
 * a comment or a doc. No log line in either actor host emits any of the five. So `oks` was
 * permanently 0, `total` was `fails`, and the `MIN_UNITS=10` gate meant this timer could fire only
 * on ten stray `SessionLost` tracebacks in fifteen minutes — at which point the ratio was 100% by
 * construction. It could not count the shape it was written for, and it was a **Machine's** only
 * health authority for as long as it existed.
 *
 * The duty is the **Warden's** now, for the reason 0037 gives — "the thing that decides a Worker is
 * sick should be the thing that can restart it" — and `cli/warden/sickworker.go` is the first version of
 * this check that can produce the number in its name: it differences two readings of the Worker's
 * own `kontra_resource_loads_total` / `kontra_resource_load_failures_total` counters, so the window
 * is a fact about the Warden's clock rather than a hope about a log's retention. The counter names
 * are pinned by `shared/conformance/workerhealth.json`; the verdict is three-valued and `cannot tell`
 * restarts nothing.
 *
 * WHAT A MACHINE PLACED BY THIS SCRIPT NOW HAS is no health authority of its own — step 8 disables
 * the retired timer on every converge, for Machines placed before this change.
 * Installing the Warden is `kontra warden join`, not this file: the two are placed by different
 * authorities, and a Fleet Machine that has not enrolled has a Worker under `Restart=always` and a
 * `loads` chip that says `unknown` — which is the honest report of a Machine nobody is judging.
 */
export function machineInstall(a: MachineActor): pulumi.Input<string> {
  validateMachineActor(a);
  // Absent means absent — an empty `KONTRA_MAX_PARALLEL_SESSIONS=` would be read as a value by
  // the Go host (`os.Getenv(...) != ""` is its guard) and parsed as garbage, which is worse than
  // the default it is trying to leave alone.
  const sessionCap =
    a.maxSessions === undefined ? '' : `KONTRA_MAX_PARALLEL_SESSIONS=${a.maxSessions}\n`;
  // The same rule for the metrics address, and it earns its own line for the reason `metricsPort`
  // documents: a Machine holding ONE Worker leaves both hosts on their own :9110 default, and a
  // Machine that is PACKING must say a different port per Worker or the second listener dies quietly.
  const metricsAddr =
    a.metricsPort === undefined ? '' : `KONTRA_METRICS_ADDR=127.0.0.1:${a.metricsPort}\n`;
  const scrapePort = a.metricsPort ?? METRICS_PORT_BASE;
  const names = machineUnits(a.name);
  const unitFiles = Object.entries(units(a))
    .sort(([x], [y]) => (x < y ? -1 : 1)) // byte-stable: the script IS the command's trigger
    .map(([name, body]) => `cat > /etc/systemd/system/${name} <<'UNIT_EOF'\n${body}\nUNIT_EOF\n`)
    .join('\n');

  return `set -eu
ACTOR='${a.name}'
VERSION='${a.version}'
CONTROLLER='${a.controller}'
BUNDLE_URL='${a.bundleUrl}'
BUNDLE_SHA='${a.bundleSha}'
TAG='${a.tag}'
# NAMED AFTER THE WORKER, not a singleton — see this file's header. A second Worker on this Machine
# unpacks into its own root, writes its own units and its own env, and step 3's "rm -rf" below
# therefore cannot reach a co-tenant's code.
ROOT=${machineRoot(a.name)}
SHARED=/opt/kontra
VMAGENT_VERSION='1.106.1'
# vlagent ships in the VictoriaLogs repo, on its own release train — a separate pin, not a second
# spelling of the one above. Matches the victorialogs service in docker-compose.yml. (No backticks
# in this script: it is one TypeScript template literal and a backtick ends it mid-sentence.)
VLAGENT_VERSION='1.9.1'
log() { echo "[install] $*" >&2; }

# 1) base packages. A DO Ubuntu image has python3 but not pip, and cloud-init is usually STILL
#    running when the provider calls the Machine ready.
#
#    DPkg::Lock::Timeout IS NOT SUFFICIENT, and this cost two whole fleet runs. It waits for the
#    DPKG lock. "apt-get update" does not take that lock — it takes /var/lib/apt/lists/lock,
#    which is a different lock with no timeout option at all. Measured on kf-reddit-01, twice in
#    a row, 33 seconds after boot:
#      E: Could not get lock /var/lib/apt/lists/lock. It is held by process 1371 (apt-get)
#      E: Unable to lock directory /var/lib/apt/lists/
#    and under "set -eu" that ends the whole install before the actor deploy.sh ever runs. The
#    Fleet reports it as a failed stackUp with a 30-line shell script in the message and no hint
#    which line died. Whether it happens is pure timing against unattended-upgrades, which is why
#    this read as a flaky fleet rather than a broken one.
#
#    So: let cloud-init finish, then RETRY rather than trust a flag that covers the other lock.
#    A shell loop around "fuser" was the obvious alternative and is wrong twice over: psmisc is
#    not installed on a minimal image, so "fuser" fails, and "fuser ... || break" reads that
#    failure as "the lock is free" and proceeds into the collision it was meant to avoid.
apt_retry() {
  n=0
  while [ "$n" -lt 30 ]; do
    if apt-get -o DPkg::Lock::Timeout=60 "$@"; then return 0; fi
    n=$((n + 1))
    log "apt busy (attempt $n/30), waiting 10s"
    sleep 10
  done
  return 1
}
if ! command -v pip3 >/dev/null 2>&1 || ! command -v curl >/dev/null 2>&1; then
  export DEBIAN_FRONTEND=noninteractive
  command -v cloud-init >/dev/null 2>&1 && cloud-init status --wait >/dev/null 2>&1 || true
  apt_retry update
  apt_retry install -y --no-install-recommends \
    python3 python3-pip curl ca-certificates tar
fi
#    tmux is deliberately NOT installed here (ADR 0020). It used to be, best-effort, for --tmux —
#    and a best-effort install feeding a unit with ConditionPathExists is how a Machine deployed
#    "successfully" and was never viewable. The session converge installs what a session needs, at
#    the moment somebody asks for one.

# 2) the container runtime, and the id range a user namespace is allocated out of.
#
#    ADR 0036 collapses the Target axis to one and it is a container, so a Machine that cannot run
#    one cannot hold a Worker. ADR 0037 gives that runtime its only caller: the Warden, which
#    reconciles the Workers on this Machine through the podman driver (cli/warden/driver_podman.go).
#
#    THE SUBUID ALLOCATION IS THE PART THAT IS EASY TO MISS AND IMPOSSIBLE TO WORK AROUND. Rootful
#    podman is asked for --userns=auto, which allocates a DISTINCT range per pod out of the
#    "containers" entry in /etc/subuid and /etc/subgid — distinct per pod being the point, since two
#    Workers sharing a range could reach each other's files as root. A stock Ubuntu image has no
#    such entry, so podman fails at "pod create" with "no subuid ranges found for user containers"
#    and the driver REFUSES rather than degrading to host root. That refusal is correct and it is
#    also a Machine that can hold no Workers at all.
#
#    IT BELONGS HERE AND NOT IN A RUNBOOK. It was added by hand to the box slice 02 was written on
#    and flagged as a provisioning prerequisite; a prerequisite that lives in a document is one
#    that is true on whichever machine somebody happened to read the document on.
#
#    16777216 is 256 ranges of 65536, which is podman's own allocation size — 256 concurrent
#    Workers on one Machine, well past anything slice 11's packing will ask for. The start is
#    524288 to sit clear of the 100000-165535 range adduser hands to ordinary users.
if ! command -v podman >/dev/null 2>&1; then
  export DEBIAN_FRONTEND=noninteractive
  command -v cloud-init >/dev/null 2>&1 && cloud-init status --wait >/dev/null 2>&1 || true
  apt_retry update
  apt_retry install -y --no-install-recommends podman uidmap
fi
#    Appended only when absent, and matched on the "containers:" PREFIX rather than on the whole
#    line: a SECOND allocation for the same user is what podman reads as a conflict, so an install
#    that ran twice must not write it twice.
for f in /etc/subuid /etc/subgid; do
  touch "$f"
  grep -q '^containers:' "$f" || echo 'containers:524288:16777216' >> "$f"
done

# 2b) what the Warden's EGRESS POLICY needs from the Machine, for the same reason the subuid
#     allocation is here rather than in a runbook.
#
#     ADR 0036 names egress as the exposure it does not close — "renting the platform to scan a
#     third party from kontra's addresses" — and hands it to the Warden, which installs an
#     nftables ruleset in this Machine's OWN network namespace (cli/warden/warden_egress.go). Two things
#     have to be true on the Machine for that to work, and neither is the Warden's to arrange.
#     (No backticks in any of this. The whole script is a TypeScript template literal and one of
#     those ends it mid-sentence — the same trap driver_podman_test.go's fixture records.)
#
#     nft         the Warden shells out to it. Without it the install fails, the policy is never
#                 enforced, and the reconcile loop then starts NO Worker at all — which is the
#                 safe direction and is also a Machine that holds nothing. A stock Ubuntu image
#                 carries iptables-nft but not necessarily the nft binary, so it is named here.
#
#     bridge-nf-  whether traffic between two containers on ONE bridge traverses the ip forward
#     call-       hook. It is the kernel default where br_netfilter is loaded, and br_netfilter is
#     iptables    loaded lazily — so on a Machine that has not yet run a container it is simply
#                 absent, and a rule written against it enforces nothing between the Workers this
#                 Machine is packing (ADR 0037: several Workers may share a Machine). Measured on
#                 this checkout's Controller: with it on, a container reaching another container's
#                 address on the same bridge is DROPPED by the policy; the module is loaded here so
#                 that is true from the Machine's first boot rather than from its first container.
if ! command -v nft >/dev/null 2>&1; then
  export DEBIAN_FRONTEND=noninteractive
  command -v cloud-init >/dev/null 2>&1 && cloud-init status --wait >/dev/null 2>&1 || true
  apt_retry update
  apt_retry install -y --no-install-recommends nftables
fi
modprobe br_netfilter 2>/dev/null || true
mkdir -p /etc/modules-load.d /etc/sysctl.d
grep -q '^br_netfilter$' /etc/modules-load.d/kontra-warden.conf 2>/dev/null \
  || echo br_netfilter > /etc/modules-load.d/kontra-warden.conf
cat > /etc/sysctl.d/99-kontra-warden.conf <<'SYSCTL'
# The Warden's egress policy is an nftables ruleset on the ip forward hook; without these two,
# traffic between containers on one bridge never reaches it. See cli/warden/warden_egress.go.
net.bridge.bridge-nf-call-iptables = 1
net.bridge.bridge-nf-call-ip6tables = 1
SYSCTL
#     THIS FILE ONLY, not "sysctl --system". Re-reading every sysctl.d entry on a Machine kontra
#     does not own is a side effect nobody asked this install for; the two keys written above are the
#     two it is entitled to set. It is best-effort because br_netfilter may not have loaded on a
#     kernel that has it built differently, and the reconcile loop is not the place to find that out.
sysctl -q -p /etc/sysctl.d/99-kontra-warden.conf >/dev/null 2>&1 || true

# 3) the Bundle, verified. An Artifact that is not content-checked is not content-pinned, and
#    "what is actually running" stops being answerable from the deploy command — the failure
#    this whole build/deploy split exists to prevent.
#
#    BUNDLE_URL is an OCI blob endpoint (ADR 0036) — /v2/bundles/<actor>/blobs/sha256:<sha> on
#    the Controller's registry — and BUNDLE_SHA is the digest in that very path. So this is still
#    one anonymous GET of one tar.gz, checked before it is unpacked, and there is deliberately no
#    OCI client here: the manifest was resolved and verified in the control plane, which is the
#    half that has to make a choice. An airgapped install changes only where the registry is.
mkdir -p "$ROOT/bin" "$SHARED/bin"
if [ ! -f "$ROOT/.bundle-$BUNDLE_SHA" ]; then
  log "fetching bundle $BUNDLE_SHA"
  curl -fsSL "$BUNDLE_URL" -o /tmp/kontra-bundle.tar.gz
  got=$(sha256sum /tmp/kontra-bundle.tar.gz | cut -d' ' -f1)
  [ "$got" = "$BUNDLE_SHA" ] || { echo "bundle sha mismatch: $got != $BUNDLE_SHA" >&2; exit 1; }
  rm -rf "$ROOT/actor" "$ROOT/actorkit"
  mkdir -p "$ROOT"
  tar -xzf /tmp/kontra-bundle.tar.gz -C "$ROOT"
  rm -f /tmp/kontra-bundle.tar.gz "$ROOT"/.bundle-*
  touch "$ROOT/.bundle-$BUNDLE_SHA"
  chmod +x "$ROOT/bin/handler"
fi

# 4) the ACTOR HOST's runtime. In a container these come from the shared python base image
#    (infra/Dockerfile.pyworker); a Machine has no base image, so the same set is installed
#    here. Deliberately NOT in the actor's deploy.sh: this is the framework's dependency, not
#    the actor's, and every actor would otherwise have to repeat it.
#
#    temporalio because the actor process IS a Temporal activity worker and imports the SDK
#    directly. Nothing here serves HTTP, so there is no web framework in the set.
if ! python3 -c 'import temporalio, redis, boto3, pydantic' >/dev/null 2>&1; then
  log "installing the actor host runtime"
  python3 -m pip install --no-cache-dir --break-system-packages \
      temporalio redis boto3 pydantic 2>/dev/null \
    || python3 -m pip install --no-cache-dir \
      temporalio redis boto3 pydantic
fi

# 5) the actor's own dependencies — the SAME deploy.sh a container build runs. One script, both
#    Targets; an actor whose dependencies live only in a Dockerfile cannot be placed here.
if [ -f "$ROOT/actor/$ACTOR/deploy.sh" ]; then
  log "running the actor's deploy.sh"
  KONTRA_TARGET=machine sh "$ROOT/actor/$ACTOR/deploy.sh"
fi

# 6) environment. Everything points at the Controller, which is what makes the whole Fleet share
#    ONE state store instead of each Machine being an island — it used to take a directory of
#    component manifests rewritten by sed to say the same thing.
#
#    EVERY VARIABLE HERE NAMES A SERVICE THE CONTROLLER ACTUALLY RUNS. This block also wrote a
#    schema-registry endpoint on port 8081 of the Controller. Nothing binds 8081 there, that
#    service is in no compose file, and no client anywhere read the variable — so every Droplet in
#    every Fleet was handed the address of a component that does not exist, which is exactly the
#    kind of fact a reader debugging a registration problem would believe (ADR 0027 removed it).
mkdir -p /etc/kontra
cat > /etc/kontra/worker-$ACTOR.env <<ENV_EOF
KONTRA_ACTOR_NAME=$ACTOR
KONTRA_ACTOR_VERSION=$VERSION
KONTRA_ADDRESS=$CONTROLLER:7233
KONTRA_ORCHESTRATOR_URL=http://$CONTROLLER:8088
KONTRA_S3_ENDPOINT=http://$CONTROLLER:8333
KONTRA_REDIS_HOST=$CONTROLLER:6379
KONTRA_CONTROLLER=$CONTROLLER
KONTRA_TAG=\${TAG}
${sessionCap}${metricsAddr}PYTHONPATH=$ROOT/sdk/python:$ROOT/runtime/python
PYTHONUNBUFFERED=1
PLAYWRIGHT_BROWSERS_PATH=/opt/ms-playwright
ENV_EOF
#    …and the MACHINE's own, which is the vmagent's. Separate from the Worker's for the reason the
#    unit's comment gives: an agent reading a Worker's env file labels every packed Worker's series
#    with whichever one it happened to read.
cat > /etc/kontra/vmagent.env <<ENV_EOF
KONTRA_CONTROLLER=$CONTROLLER
KONTRA_TAG=\${TAG}
ENV_EOF

# 7) the metrics agent. Workers accept NO inbound connections, so nothing can scrape them from
#    the Controller — the agent has to scrape 127.0.0.1 and push outward. One static binary and
#    a config; this replaces the Alloy role in the Ansible layer it succeeded.
#
#    THE BINARY AND THE CONFIG ARE THE MACHINE'S, THE TARGET IS THE WORKER'S. A packed Machine runs
#    ONE agent over N targets, so the target list cannot live in a file every Worker's install
#    overwrites — the last one to run would leave the Machine scraping one Worker and silently not
#    the others. So the config names a DIRECTORY (file_sd_configs, which vmagent re-reads on its
#    own) and each Worker writes exactly one file in it, carrying its own actor label; the teardown
#    removes that one file, which is how a Worker leaving stops being scraped without touching the
#    co-tenant's target or the agent itself. (No backticks anywhere in this script: the whole thing
#    is a TypeScript template literal and one of those ends it mid-sentence.)
if [ ! -x "$SHARED/bin/vmagent" ]; then
  curl -fsSL "https://github.com/VictoriaMetrics/VictoriaMetrics/releases/download/v\${VMAGENT_VERSION}/vmutils-linux-amd64-v\${VMAGENT_VERSION}.tar.gz" \
    | tar -xz -C "$SHARED/bin" vmagent-prod 2>/dev/null && mv "$SHARED/bin/vmagent-prod" "$SHARED/bin/vmagent" || log "vmagent unavailable; continuing without fleet metrics"
  [ -f "$SHARED/bin/vmagent" ] && chmod +x "$SHARED/bin/vmagent"
fi
mkdir -p /etc/kontra/scrape.d
cat > /etc/kontra/vmagent.yml <<'SCRAPE_EOF'
global:
  scrape_interval: 30s
scrape_configs:
  # The actor hosts' own counters (internals/metrics.py). One file per Worker under scrape.d, so a
  # Machine packing several of them scrapes all of them; the sidecar's job on 9090 went with the
  # sidecar, and the handler and actor now report through OTLP.
  - job_name: kontra-actor
    file_sd_configs: [{files: ['/etc/kontra/scrape.d/*.json']}]
SCRAPE_EOF
cat > /etc/kontra/scrape.d/$ACTOR.json <<TARGET_EOF
[{"targets": ["127.0.0.1:${scrapePort}"],
  "labels": {"actor": "$ACTOR", "actor_version": "$VERSION"}}]
TARGET_EOF

# 7b) the logs agent (ADR 0050 §1). Same reason, same direction, different signal: nothing can
#     reach a Worker from the Controller, so the agent reads this Machine's journal and pushes.
#
#     IT IS NOT FATAL IF IT IS MISSING, matching vmagent one block up: a Machine that cannot fetch
#     the binary still runs its Worker. It says so rather than failing the deploy, because a Fleet
#     that will not come up because a log shipper 404'd is a worse outcome than a Fleet with no logs
#     — and the line above is the only place anybody would learn which one happened.
#
#     THE BUFFER DIRECTORY IS CREATED HERE, not left to the unit: systemd would start the agent with
#     a tmpDataPath it cannot write, which vlagent reports once at startup and then never again.
if [ ! -x "$SHARED/bin/vlagent" ]; then
  curl -fsSL "https://github.com/VictoriaMetrics/VictoriaLogs/releases/download/v\${VLAGENT_VERSION}/vlutils-linux-amd64-v\${VLAGENT_VERSION}.tar.gz" \
    | tar -xz -C "$SHARED/bin" vlagent-prod 2>/dev/null && mv "$SHARED/bin/vlagent-prod" "$SHARED/bin/vlagent" || log "vlagent unavailable; continuing without fleet logs"
  [ -f "$SHARED/bin/vlagent" ] && chmod +x "$SHARED/bin/vlagent"
fi
mkdir -p /var/lib/kontra/vlagent

# 8) units. There is no watchdog here any more (ADR 0037) — see the block above machineInstall for
#    what it did, why it never did it, and what took the duty. A re-converged Machine that HAD the
#    old pair is left with two inert unit files it will never start again; they are stopped and
#    disabled below, and removing them here would mean this script deleting files it no longer
#    writes, which is a different job with a different failure mode.
${unitFiles}
systemctl daemon-reload
systemctl disable --now kontra-watchdog.timer kontra-watchdog.service >/dev/null 2>&1 || true
#    THE PRE-PACKING SINGLETON UNITS, DISARMED AT INSTALL TIME. See this file's header: a Machine
#    placed before slice 11 carries kontra-actor.service, and leaving it running beside the
#    namespaced pair would be TWO pollers on one queue — the failure the seam exists to prevent.
#    Doing it here rather than only in the old resource's teardown makes the transition independent
#    of Pulumi's create/delete ordering, and it cannot reach a co-tenant because under the old layout
#    a Machine could hold only one Worker.
systemctl disable --now kontra-actor.service kontra-handler.service >/dev/null 2>&1 || true
systemctl enable ${names.actor} ${names.handler} >/dev/null 2>&1
[ -x "$SHARED/bin/vmagent" ] && systemctl enable --now kontra-vmagent.service >/dev/null 2>&1 || true
# Same terms as vmagent, and machineTeardown leaves this one running for the same reason: a
# teardown is not the end of the Machine, and a co-tenant's logs must not stop because a neighbour
# left. It is also the moment its lines matter most.
[ -x "$SHARED/bin/vlagent" ] && systemctl enable --now kontra-vlagent.service >/dev/null 2>&1 || true
# Actor first: the handler Requires= it, and a handler polling for workflows whose RunBatch has
# no listener is the one failure that reports as a healthy Worker.
systemctl restart ${names.actor}
systemctl restart ${names.handler}

sleep 5
for u in ${names.actor} ${names.handler}; do
  systemctl is-active --quiet $u || {
    echo "$u did not start:" >&2
    journalctl -u $u -n 40 --no-pager >&2
    exit 1
  }
done
log "worker up: $ACTOR@$VERSION on $(hostname)"
`;
}

/**
 * Stopping ONE Worker. Ordered so the handler stops polling before the host it drives goes.
 *
 * ═══ IT TAKES A NAME NOW, AND THAT IS THE WHOLE OF THE CO-TENANT SAFETY ═══
 *
 * This was a constant, and a constant is a teardown that stops EVERY Worker on the Machine. That was
 * exactly right while a Machine held one; the moment it packs (ADR 0037) it is the bug this slice
 * exists to avoid — Pulumi deletes the placement a converge stopped mentioning, and a constant
 * teardown would take the co-tenant's Worker with it, with nothing raising on either side. Naming
 * the Worker makes the blast radius the placement rather than the Machine.
 *
 * VMAGENT IS DELIBERATELY NOT IN THE LIST, and it used to be. One agent serves the whole Machine
 * (see `units`), so stopping it here would end the co-tenant's metrics as well; what leaves with a
 * Worker is its TARGET FILE, which is one `rm` and takes exactly this Worker's series out of the
 * scrape. The agent then runs with one fewer target, which is the correct desired state.
 *
 * THE RETIRED SINGLETONS ARE NOT IN THE LIST EITHER — `kontra-tmux.service` (ADR 0020),
 * `kontra-watchdog.timer` (ADR 0037) and the pre-packing `kontra-actor.service` pair. Those belong
 * to a Machine placed before this change, and the resource that placed it carries the OLD teardown
 * in Pulumi's state, which is what runs when it is deleted. `machineInstall` step 8 disarms them on
 * every converge as well, so neither ordering leaves one armed.
 */
export function machineTeardown(name: string): string {
  const units = machineUnits(name);
  return `systemctl stop ${units.handler} ${units.actor} 2>/dev/null || true
systemctl disable ${units.handler} ${units.actor} 2>/dev/null || true
rm -f /etc/systemd/system/${units.handler} /etc/systemd/system/${units.actor} \\
  /etc/kontra/worker-${name}.env /etc/kontra/scrape.d/${name}.json 2>/dev/null || true
systemctl daemon-reload 2>/dev/null || true`;
}
