#!/usr/bin/env python3
"""Assert `control/pulumi/Pulumi.yaml` still converges the same topology as `docker-compose.yml`.

    python3 control/pulumi/parity.py          # from the repo root

WHY THIS EXISTS, AND WHEN IT GOES. ADR 0052 replaces `docker-compose.yml` with the Pulumi program;
until that file is deleted the two are two spellings of one topology, and a drift between them is
invisible — each one works on its own. This is the only thing that reads both. Delete it in the same
commit that deletes `docker-compose.yml` (ADR 0052 Consequences).

IT COMPARES RESOLVED OUTPUT ON BOTH SIDES, never the source files:

  * `docker compose --env-file .env.quickstart config --format json` — interpolation done,
    `${KONTRA_BIND:-127.0.0.1}` already `127.0.0.1`, the `*orchestrator-env` anchor already expanded
    into all three consumers.
  * `pulumi preview --json` — config defaults applied, `fn::split` already a list, and every input
    already through the provider's own `Check`, so a misspelled property name has already failed.

The `wait`/`waitTimeout` block is the part worth reading. `waitTimeout` DEFAULTS TO 60 SECONDS and
compose's gates have no timeout, so this asserts every healthchecked container carries an explicit
one that is at least `start_period + retries x (interval + timeout)` — the point at which Docker
itself gives up. Below that, a first boot fails as an intermittent flake and gets diagnosed as a
Pulumi problem rather than as a slow Postgres.
"""
import json
import os
import subprocess
import sys
import tempfile

REPO = os.path.abspath(os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", ".."))
PROGRAM = os.path.join(REPO, "control", "pulumi")

# Pulumi resource key -> compose service name. The keys are camelCase because Pulumi YAML resource
# names are referenced as `${name}` and a dash there reads as an operator.
SERVICES = {
    "postgres": "postgres",
    "temporalConfig": "temporal-dynamicconfig",
    "temporal": "temporal",
    "redis": "redis",
    "registry": "registry",
    "seaweed": "seaweed",
    "victoriaMetrics": "victoriametrics",
    "victoriaLogs": "victorialogs",
    "logship": "logship",
    "orchestratorApi": "orchestrator-api",
    "orchestratorInfra": "orchestrator-infra",
    "orchestratorProbe": "orchestrator-probe",
    "cli": "cli",
}

# compose bind-mounts three scripts out of control/images/; the program uploads them instead,
# because §3's install has no checkout to bind-mount from. Keyed by the path inside the container.
EXPECTED_UPLOADS = {
    "/docker-entrypoint-initdb.d/01-ducklake.sh",
    "/logship.sh",
    "/logline.py",
}

problems: list[str] = []
notes: list[str] = []


def bad(msg: str) -> None:
    problems.append(msg)


def resolved_compose() -> dict:
    out = subprocess.run(
        ["docker", "compose", "--env-file", ".env.quickstart", "config", "--format", "json"],
        cwd=REPO,
        capture_output=True,
        text=True,
    )
    if out.returncode != 0:
        sys.exit(f"docker compose config failed:\n{out.stderr}")
    return json.loads(out.stdout)


def previewed_pulumi(workspaces: str) -> dict:
    # A scratch file:// backend and a throwaway stack: this must never touch ~/.kontra/state, and a
    # preview against an empty state is every resource as a create, which is what we want to read.
    #
    # TWO THINGS THIS MUST NOT LEAVE BEHIND, and both are next to the program rather than in the
    # backend: `pulumi stack init` writes `Pulumi.parity.yaml` beside `Pulumi.yaml`, and
    # `pulumi config set` would write into it. `workspaces` therefore arrives as `-c` on the preview
    # itself, and the stack file is removed on the way out — a check that only reads must not dirty
    # the working tree, least of all in a repo where another agent is holding uncommitted work.
    stack_file = os.path.join(PROGRAM, "Pulumi.parity.yaml")
    preexisting = os.path.exists(stack_file)
    with tempfile.TemporaryDirectory() as tmp:
        env = dict(
            os.environ,
            PULUMI_BACKEND_URL=f"file://{tmp}",
            PULUMI_CONFIG_PASSPHRASE="parity",
        )

        def run(*args: str):
            return subprocess.run(
                ["pulumi", "-C", PROGRAM, *args, "--non-interactive"],
                capture_output=True,
                text=True,
                env=env,
            )

        try:
            r = run("stack", "init", "parity")
            if r.returncode != 0:
                sys.exit(f"pulumi stack init failed:\n{r.stderr}")
            r = run("preview", "--json", "-c", f"workspaces={workspaces}")
            if r.returncode != 0:
                sys.exit(f"pulumi preview failed:\n{r.stdout[-4000:]}\n{r.stderr}")
            return json.loads(r.stdout)
        finally:
            if not preexisting and os.path.exists(stack_file):
                os.remove(stack_file)


def dur(x) -> str:
    """The provider's Check fills startPeriod with "0s" where compose omits the key. Same
    instruction to Docker, different JSON."""
    return "0s" if x in (None, "", "0s") else x


def seconds(x) -> int:
    return int(dur(x).rstrip("s") or 0)


def main() -> int:
    compose = resolved_compose()
    workspaces = compose["x-workspaces"]
    preview = previewed_pulumi(workspaces)

    steps = [s for s in preview["steps"] if s.get("newState")]
    containers, images, volumes, networks = {}, {}, {}, {}
    for s in steps:
        ns = s["newState"]
        key = ns["urn"].split("::")[-1]
        {
            "docker:index/container:Container": containers,
            "docker:index/remoteImage:RemoteImage": images,
            "docker:index/volume:Volume": volumes,
            "docker:index/network:Network": networks,
        }.get(ns["type"], {})[key] = ns

    notes.append(
        f"containers={len(containers)} volumes={len(volumes)} "
        f"images={len(images)} networks={len(networks)}"
    )
    if len(containers) != len(compose["services"]):
        bad(f"container count {len(containers)} != {len(compose['services'])} compose services")
    if len(volumes) != len(compose["volumes"]):
        bad(f"volume count {len(volumes)} != {len(compose['volumes'])} compose volumes")

    if {v["name"] for v in compose["volumes"].values()} != {
        v["inputs"]["name"] for v in volumes.values()
    }:
        bad("volume names differ — a rename here is silent data loss that looks like a fresh install")
    if len(networks) != 1 or list(networks.values())[0]["inputs"]["name"] != (
        compose["networks"]["default"]["name"]
    ):
        bad("network name differs")

    image_name = {k: v["inputs"]["name"] for k, v in images.items()}

    for pkey, svc in SERVICES.items():
        if pkey not in containers:
            bad(f"no pulumi container for compose service {svc}")
            continue
        c = compose["services"][svc]
        p = containers[pkey]["inputs"]
        deps = [u.split("::")[-1] for u in containers[pkey].get("dependencies", [])]

        if p.get("name") != c.get("container_name"):
            bad(f"{svc}: container name {p.get('name')!r} != {c.get('container_name')!r}")

        # `image` is an imageId at converge time, so resolve it back through the RemoteImage the
        # container depends on. Exactly one, or the reference is ambiguous.
        img = [d for d in deps if d in image_name]
        if len(img) != 1:
            bad(f"{svc}: expected one RemoteImage dependency, got {img}")
        elif image_name[img[0]] != c["image"]:
            bad(f"{svc}: image {image_name[img[0]]!r} != compose {c['image']!r}")

        if str(p.get("restart")) != str(c.get("restart")):
            bad(f"{svc}: restart {p.get('restart')!r} != {c.get('restart')!r}")
        if c.get("hostname") != p.get("hostname"):
            bad(f"{svc}: hostname {p.get('hostname')!r} != {c.get('hostname')!r}")
        if (c.get("command") or None) != (p.get("command") or None):
            bad(f"{svc}: command differs\n    compose={c.get('command')!r}\n    pulumi={p.get('command')!r}")
        if (c.get("entrypoint") or None) != (p.get("entrypoints") or None):
            bad(f"{svc}: entrypoint differs — this provider splits `entrypoints` from `command`")

        # ENV, KEY BY KEY. This is the check that earns the file: `x-orchestrator-env` reached three
        # services through a YAML merge key and the port reaches them through `fn::split`, so the
        # two spellings have nothing in common and only the resolved lists can be compared.
        cenv = c.get("environment") or {}
        penv: dict[str, str] = {}
        for entry in p.get("envs") or []:
            k, _, v = entry.partition("=")
            if k in penv:
                bad(f"{svc}: duplicate env key {k} — Docker's last-wins is not a specification")
            penv[k] = v
        for k in sorted(set(cenv) | set(penv)):
            if cenv.get(k) != penv.get(k):
                bad(f"{svc}: env {k}: compose={cenv.get(k)!r} pulumi={penv.get(k)!r}")

        clabels = c.get("labels") or {}
        plabels = {lbl["label"]: lbl["value"] for lbl in (p.get("labels") or [])}
        if clabels != plabels:
            bad(f"{svc}: labels {plabels} != {clabels} — logship selects on kontra.logs")

        cports = sorted(
            (int(x["target"]), int(x["published"]), x.get("host_ip"), x.get("protocol"))
            for x in (c.get("ports") or [])
        )
        pports = sorted(
            (int(x["internal"]), int(x["external"]), x.get("ip"), x.get("protocol"))
            for x in (p.get("ports") or [])
        )
        if cports != pports:
            bad(f"{svc}: ports {pports} != {cports}")

        # MOUNTS. compose names a volume by its short key and resolves the prefix separately; this
        # provider names the real volume. Compare the real names.
        cmounts, pmounts = set(), set()
        for m in c.get("volumes") or []:
            if m["type"] == "volume":
                cmounts.add(
                    ("vol", compose["volumes"][m["source"]]["name"], m["target"], bool(m.get("read_only")))
                )
            else:
                cmounts.add(("bind", m["source"], m["target"], bool(m.get("read_only"))))
        for m in p.get("volumes") or []:
            if m.get("volumeName"):
                pmounts.add(("vol", m["volumeName"], m["containerPath"], bool(m.get("readOnly"))))
            else:
                pmounts.add(("bind", m["hostPath"], m["containerPath"], bool(m.get("readOnly"))))
        uploads = {u["file"] for u in p.get("uploads") or []}
        if uploads - EXPECTED_UPLOADS:
            bad(f"{svc}: unexpected upload(s) {sorted(uploads - EXPECTED_UPLOADS)}")
        for m in sorted(cmounts - pmounts):
            if m[0] == "bind" and m[2] in uploads:
                notes.append(f"  {svc}: bind {m[1]} became upload {m[2]}")
                continue
            bad(f"{svc}: mount present in compose, absent here: {m}")
        for m in sorted(pmounts - cmounts):
            bad(f"{svc}: mount present here, absent in compose: {m}")

        # THE NETWORK ALIAS IS THE SERVICE NAME. compose gave every container both its
        # container_name and its service name as DNS; this provider gives only the container_name,
        # and every in-cluster URL in the stack uses the service name.
        na = p.get("networksAdvanced") or []
        if len(na) != 1:
            bad(f"{svc}: expected exactly one network, got {len(na)}")
        elif svc not in (na[0].get("aliases") or []):
            bad(f"{svc}: network alias {na[0].get('aliases')} does not include the service name")

        # HEALTHCHECK, then the wait contract.
        ch, ph = c.get("healthcheck"), p.get("healthcheck")
        if bool(ch) != bool(ph):
            bad(f"{svc}: healthcheck present={bool(ph)}, compose={bool(ch)}")
        elif ch:
            if ch["test"] != ph["tests"]:
                bad(f"{svc}: healthcheck test differs\n    compose={ch['test']}\n    pulumi={ph['tests']}")
            for cf, pf in (("interval", "interval"), ("timeout", "timeout"), ("start_period", "startPeriod")):
                if dur(ch.get(cf)) != dur(ph.get(pf)):
                    bad(f"{svc}: healthcheck {cf} {ph.get(pf)!r} != {ch.get(cf)!r}")
            if int(ch.get("retries", 0)) != int(ph.get("retries", 0)):
                bad(f"{svc}: healthcheck retries {ph.get('retries')} != {ch.get('retries')}")

        if ch and pkey != "temporalConfig":
            if p.get("wait") is not True:
                bad(f"{svc}: has a healthcheck but wait is {p.get('wait')!r} — no gate for a dependent")
            budget = seconds(ch.get("start_period")) + int(ch["retries"]) * (
                seconds(ch["interval"]) + seconds(ch["timeout"])
            )
            wt = p.get("waitTimeout")
            if not wt:
                bad(f"{svc}: wait:true with no explicit waitTimeout — the 60s default is the flake")
            elif int(wt) < budget:
                bad(f"{svc}: waitTimeout {wt} < Docker's own budget {budget} — Pulumi gives up first")
            else:
                notes.append(f"  {svc}: waitTimeout={wt} >= docker-budget={budget}")
        # The program's own healthcheck, not compose's: this is a claim about what the provider does
        # with THIS input. "the Docker container is waited for being healthy state after creation.
        # This requires your container to have a healthcheck, otherwise this provider will error."
        if not ph and p.get("wait") is True:
            bad(f"{svc}: wait:true with no healthcheck — the provider errors on this")

    # THE ORDERING GRAPH, EDGE FOR EDGE.
    for pkey, svc in SERVICES.items():
        if pkey not in containers:
            continue
        cdeps = set((compose["services"][svc].get("depends_on") or {}).keys())
        for d in cdeps:
            if d not in compose["services"]:
                bad(f"{svc}: depends_on names a service that does not exist: {d}")
        pdeps = {
            SERVICES[k]
            for k in (u.split("::")[-1] for u in containers[pkey].get("dependencies", []))
            if k in SERVICES
        }
        if cdeps != pdeps:
            bad(f"{svc}: dependsOn {sorted(pdeps)} != compose depends_on {sorted(cdeps)}")

    # AND EACH EDGE'S CONDITION.
    edges = 0
    for pkey, svc in SERVICES.items():
        for dep, spec in (compose["services"][svc].get("depends_on") or {}).items():
            edges += 1
            dkey = next(k for k, v in SERVICES.items() if v == dep)
            d = containers[dkey]["inputs"]
            cond = spec["condition"]
            if cond == "service_healthy":
                if d.get("wait") is not True or not d.get("healthcheck"):
                    bad(f"{svc} -> {dep}: service_healthy needs healthcheck + wait:true on {dep}")
            elif cond == "service_started":
                if d.get("wait") is True:
                    bad(
                        f"{svc} -> {dep}: service_started must be dependsOn alone. wait:true on "
                        f"{dep} turns the cli/orchestrator-api soft cycle into a real deadlock"
                    )
            elif cond == "service_completed_successfully":
                if d.get("attach") is not True or d.get("mustRun") is not False:
                    bad(f"{svc} -> {dep}: needs attach:true + mustRun:false so the exit code is the gate")
            else:
                bad(f"{svc} -> {dep}: unhandled condition {cond}")
    notes.append(f"ordering edges checked={edges}")

    # LOOPBACK, the same assertion `scripts/assert-loopback-publish.py` makes against a live stack —
    # including its rule that zero bindings is a failure and not a pass.
    ips = {port.get("ip") for ctr in containers.values() for port in (ctr["inputs"].get("ports") or [])}
    published = sum(len(ctr["inputs"].get("ports") or []) for ctr in containers.values())
    if not ips:
        bad("no port bindings at all — this check would be vacuous")
    if ips - {"127.0.0.1"}:
        bad(f"a port is published beyond loopback: {sorted(ips - {'127.0.0.1'})}")
    expected_published = sum(len(s.get("ports") or []) for s in compose["services"].values())
    if published != expected_published:
        bad(f"published port count {published} != compose's {expected_published}")
    notes.append(f"published ports={published}, all bound to {sorted(ips)}")

    # The three written refusals in the compose file. Each is a container with no port, on purpose.
    for key, why in (
        ("victoriaMetrics", "a host firewall does not protect a DNATed port"),
        ("victoriaLogs", "no auth at all — tenancy is a request header"),
        ("orchestratorInfra", "holds the rw docker socket and the cloud credential"),
    ):
        if containers[key]["inputs"].get("ports"):
            bad(f"{key} must have no published port: {why}")

    print("\n".join(notes))
    print()
    if problems:
        print(f"FAIL — {len(problems)} problem(s):")
        for p in problems:
            print(" -", p)
        return 1
    print("OK — the Pulumi program and docker-compose.yml converge the same topology")
    return 0


if __name__ == "__main__":
    sys.exit(main())
