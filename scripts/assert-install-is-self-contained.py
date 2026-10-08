#!/usr/bin/env python3
"""Assert that an install directory contains nothing a `curl` would not have put there.

    scripts/assert-install-is-self-contained.py <install-dir>

The quickstart is two commands:

    curl -O https://raw.githubusercontent.com/medmahmoudi26/kontra/dev/docker-compose.yml
    docker compose up -d --wait

Everything that file needs has to be IN it or in an image it pulls. This is the check that keeps
that true, and it exists because the failure mode is silent in a specific and expensive way.

── WHY A BIND MOUNT IS THE BUG AND NOT AN ERROR ─────────────────────────────────────────────────

Docker does not fail on a bind-mount source that does not exist. It CREATES it, as an empty
directory. So `./control/images/postgres-init.sh` in a directory that has no `control/` gets an
empty directory mounted at `/docker-entrypoint-initdb.d/01-ducklake.sh`; postgres runs it, nothing
happens, the container reports healthy, and the DuckLake catalog is never created. The failure
surfaces later, on the first Dataset write, against a database that does not exist — several
services away from anything that mentions postgres.

That shipped. `scripts/install-cluster.macos.sh` copied `postgres-init.sh` and not the log shipper's
two files, so logship crash-looped; it had no healthcheck, `docker compose up -d --wait` therefore
never waited on it, and the script exited 0 while shipping zero logs. An empty logs rail is
indistinguishable from a Run that logged nothing.

Both are fixed — the SQL is an inline `configs:` entry, the shipper is `kontra-logship` — and this
file is what stops the third one. CI copies whatever it needs into its install directory, so without
this check the quickstart can break for everybody while CI stays green.

── THREE CHECKS, AND THE THIRD IS THE SUBTLE ONE ────────────────────────────────────────────────

  1. the directory holds only `docker-compose.yml` and (optionally) `.env`
  2. no service bind-mounts a host path other than the Docker socket and the workspace
  3. every `configs:` entry declares an explicit `mode`

The third is not about self-containedness. Compose defaults an omitted config mode to 0444, and
postgres's entrypoint `.`-sources a `*.sh` it cannot execute rather than running it
(`docker-entrypoint.sh:183`) — which runs that script's `set -eu` in the ENTRYPOINT's shell, and
that shell is deliberately `set -Eeo pipefail` with no `-u`, carrying upstream's own
"TODO swap to -Eeuo pipefail above (after handling all potentially-unset variables)". So a `-u`
leaks into `docker_temp_server_stop` and the rest of first boot, and when upstream takes that TODO
the result is a first boot that dies AFTER the cluster exists.

Measured both ways: at 0444 the entrypoint shell has `-u` set afterwards, at 0555 it does not.
Neither fails on postgres:16-alpine today, which is exactly why a person writing the next
`configs:` entry would not notice. An omitted mode is therefore an error here rather than a default.
"""

import json
import os
import subprocess
import sys

# The mounts that are legitimate. The socket is host-level Docker authority and is a documented
# decision (`docker-compose.yml`'s header); the workspace is where the operator's own code lives and
# is the one host path this install is FOR; and the install directory itself, handled at the check
# because it is only known at runtime.
SOCKET = "/var/run/docker.sock"
ALLOWED_FILES = {"docker-compose.yml", ".env"}


def fail(title: str, *lines: str) -> None:
    """Emit a GitHub Actions error annotation when running under Actions, plain text otherwise."""
    if os.environ.get("GITHUB_ACTIONS"):
        print(f"::error title={title}::")
    else:
        print(f"ERROR: {title}", file=sys.stderr)
    for line in lines:
        print(f"  {line}", file=sys.stderr)
    sys.exit(1)


def main(argv: list[str]) -> None:
    if len(argv) == 2 and argv[1] in ("-h", "--help"):
        print(__doc__)
        sys.exit(0)
    if len(argv) != 2:
        # A USAGE ERROR GOES TO STDERR AS ONE LINE, and `--help` is what prints the docstring.
        #
        # This printed `__doc__` to stdout and exited 2 for both cases. In a CI log that is sixty
        # lines of prose about bind mounts under a red X, which reads as "the self-containedness check
        # is broken" — the check reporting its own findings and the check being called wrong look
        # identical. They are different failures and only one of them is about the install.
        print(f"usage: {os.path.basename(argv[0])} <install-dir>", file=sys.stderr)
        print("       --help for what this asserts and why", file=sys.stderr)
        sys.exit(2)
    install = os.path.abspath(argv[1])

    # ── 1. only the files a curl produces ────────────────────────────────────────────────────────
    extra = sorted(
        os.path.relpath(os.path.join(root, name), install)
        for root, dirs, files in os.walk(install)
        for name in list(dirs) + list(files)
        if os.path.relpath(os.path.join(root, name), install) not in ALLOWED_FILES
    )
    # `workspaces/` is the one exception and only when it is INSIDE the install directory, which is
    # compose's default (`${PWD}/workspaces`). It is created by the install rather than fetched, so
    # its presence is the install working, not a host dependency. Anything under it is the
    # operator's code.
    extra = [p for p in extra if p != "workspaces" and not p.startswith("workspaces" + os.sep)]
    if extra:
        fail(
            "the install needs a file a curl would not have",
            *extra,
            "",
            "Carry it in docker-compose.yml as a `configs:` entry with inline `content:`,",
            "or bake it into an image (see control/images/Dockerfile.logship).",
        )

    # ── resolve the file, which is also a syntax check ───────────────────────────────────────────
    #
    # `PWD` IS PASSED EXPLICITLY AND `cwd=` ALONE IS NOT ENOUGH. Compose interpolates `${PWD}` from
    # the ENVIRONMENT VARIABLE, not from the process's working directory, and `subprocess(cwd=…)`
    # changes the second without touching the first. `docker-compose.yml` defaults the workspace to
    # `${KONTRA_WORKSPACES:-${PWD}/workspaces}`, so with an inherited `PWD` this resolved the
    # workspace against whatever directory the caller happened to be standing in — measured:
    # `/root/oss/workspaces` for an install directory several levels away. The checks then modelled
    # a different file than the one `docker compose up` would run here.
    out = subprocess.run(
        ["docker", "compose", "config", "--format", "json"],
        cwd=install,
        capture_output=True,
        text=True,
        env={**os.environ, "PWD": install},
    )
    if out.returncode != 0:
        fail("docker compose config failed in the install directory", out.stderr.strip())
    cfg = json.loads(out.stdout)

    # THE WORKSPACE PATH AS COMPOSE RESOLVED IT, not as this script would guess. `KONTRA_WORKSPACES`
    # may be blank (then it is `${PWD}/workspaces`), relative, or an absolute path somewhere else
    # entirely, and every service that reads code mounts it at the SAME path inside and out.
    workspaces = cfg["services"]["cli"]["environment"]["KONTRA_WORKSPACES"]

    # ── 2. no host path but the socket and the workspace ─────────────────────────────────────────
    binds = []
    for name, svc in cfg["services"].items():
        for vol in svc.get("volumes") or []:
            if vol.get("type") != "bind":
                continue
            src = vol.get("source", "")
            if src == SOCKET or src == workspaces or src.startswith(workspaces + os.sep):
                continue
            # THE INSTALL DIRECTORY ITSELF IS NOT A HOST PATH THIS CHECK IS ABOUT. It is the
            # directory the operator curl'd into and is standing in, it always exists, and `cli`
            # mounts it at the same path inside and out so that `kontra deploy --actor ./x` means
            # the same thing on both sides. The hazard named below is a source that does NOT
            # exist — Docker invents an empty directory for it — and this one cannot be that.
            #
            # DELIBERATELY NOT "ANYTHING UNDER THE INSTALL". `$PWD/runtime/python` is under it and
            # is exactly the failure: absent from a curl-only install, invented empty, and mounted
            # over the SDK the image ships. Refusing that while allowing the root is the whole
            # distinction, so this compares for equality and does not walk the prefix.
            if src == install:
                continue
            binds.append(f"{name}: {src} -> {vol.get('target')}")
    if binds:
        fail(
            "compose bind-mounts a host path that is not the workspace or the socket",
            *binds,
            "",
            "Docker CREATES a missing bind source as an empty directory rather than failing, so this",
            "is an install that comes up healthy and is broken somewhere it does not mention.",
        )

    # ── 3. every config declares a mode ──────────────────────────────────────────────────────────
    modeless = [
        f"{name}: {c.get('source')} -> {c.get('target')}"
        for name, svc in cfg["services"].items()
        for c in svc.get("configs") or []
        if c.get("mode") is None
    ]
    if modeless:
        fail(
            "a configs: entry has no explicit mode",
            *modeless,
            "",
            "Compose defaults to 0444, which makes postgres SOURCE an init script instead of running",
            "it — leaking that script's `set -eu` into an entrypoint written without `-u`. Say 0555.",
        )

    print(
        f"install is self-contained: only {sorted(ALLOWED_FILES)} on disk, "
        f"mounts only the socket, {install} and {workspaces}, "
        f"{sum(len(s.get('configs') or []) for s in cfg['services'].values())} config(s) with explicit modes"
    )


if __name__ == "__main__":
    main(sys.argv)
