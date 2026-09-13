#!/bin/sh
# First boot initialises; every boot after it starts. Nothing else.
#
# ── WHY THE CONTAINER INITIALISES ITSELF ────────────────────────────────────────────────────────
#
# `kontra init` generates the console login and the four service tokens, prints the password ONCE
# and hashes it into `config.yaml`. On a host you run it yourself. In a copy-paste docker install
# there is no host binary to run it with — and a control plane that comes up with no account is one
# nobody can sign in to, which is where a three-line install stops being three lines.
#
# SO THE LOGIN GOES TO THE CONTAINER LOG, which is `docker compose logs kontra`. That is the only
# place it can go: printing it to a terminal nobody is attached to loses it, and writing it to a
# file inside a volume means telling somebody to `exec cat` a path. The log is where a person
# already looks after `up -d`.
#
# IDEMPOTENT, BECAUSE `init` IS. It writes `config.yaml` only when absent and says
# "already set up (config.yaml left alone)" otherwise — so a restart does not rotate the password
# out from under somebody, and a recreated container with its volume intact keeps the account it had.
#
# ── AND `up` INHERITS WHAT `init` WROTE ─────────────────────────────────────────────────────────
#
# `main()` calls `LoadAndApplyConfig()` before every command, which exports `config.yaml` into the
# process environment WITHOUT overriding anything already set. So the tokens and the console account
# reach `kontra up` and its orchestrator child by the ordinary path, and an operator who set a
# variable in `.env` still wins — environment first, file second, which is the precedence the config
# loader documents.
set -e

if [ -n "${KONTRA_SKIP_INIT:-}" ]; then
  # An escape hatch for an installation that manages `config.yaml` itself — a mounted one, or an
  # environment that supplies every token. Named rather than inferred from the file's presence,
  # because "the file is there" is also true one restart after a successful init.
  echo "kontra: skipping init (KONTRA_SKIP_INIT is set)"
else
  kontra init
fi

# `exec`, SO THE APPLIANCE IS PID 1. Without it this shell is PID 1, `docker stop` signals the
# shell, and `kontra up` — whose Ctrl-C path stops its orchestrator child first and then the
# embedded services — never receives the TERM it shuts down cleanly on. The symptom is a ten-second
# stop followed by a SIGKILL, and a Temporal database that was not closed.
exec kontra up --bind 0.0.0.0 "$@"
