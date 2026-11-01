# M4's gate runs (WP-09)

The owner's M4 gate (26 Oct; "From M4 planning" in [`decisions.md`](decisions.md)) names four items: **`tck_*` green
bar with expected failures**, **`anon_limits`**, **the demo from a clean laptop against staging** and **the team gate
up on a clean VPS**. D282 makes the last two runs by hand, not tests in CI, and has them recorded here; D306 lists what
WP-09 had still to do. This file records those runs: what ran, at which commit, when, the commands, what was seen, the
ids, and how everything was torn down.

The runs were made on **31 Oct 2026**, at two commits of `m4-mcp-anon-tck` and, after the merge, at the merge commit.
Times are UTC.

- **`3b890f3`** (`3b890f3794e4ea43532c16d8134754ccdbd46b0c`, the head after the second review, D307–D311, checked out
  clean as `m4-gate-runs`): the staging gate, `make test-kits`, the clean laptop, the TCK, `anon_limits` and the first
  VPS run. The TCK's direct variant and `anon_limits` ran last (20:27Z–20:31Z), from the same checkout while only this
  record and the three documents that point at it were edited: the code they built was `3b890f3`'s.
- **`f9f5e61`** (`f9f5e61b3ae684dc7adab804c145b80606739eff`, the head with the third review's fix, D312 at
  `7033e0a`, the compose fix, D313 at `2eb8041`, and this record's first version): the VPS rerun, from this worktree
  of the branch, clean before and after the run.
- **`4831e40`** (`4831e40fc3931c4aadb2f5bce0dce6c8d7b8fb7f`, [pull request #8](https://github.com/medmahmoudi26/kontra/pull/8)
  squash-merged to `main` at 21:56:00Z): production's deploy, staging's redeploy and the published clean laptop, from a
  clean detached checkout of `origin/main`. Its tree is the pull request's head, `4a53750`, which adds only this
  record's second version and the documents that point at it to `f9f5e61`; the pull request's CI, the `tck` workflow
  among it, ran at `4a53750`.

## Verdicts

| Gate item | Verdict | Taken at | What remains |
|---|---|---|---|
| [`tck_*` green bar with expected failures](#the-tck-tck_) | **Green in CI** on pull request #8, [`tck` run 41243927553](https://github.com/medmahmoudi26/kontra/actions/runs/41243927553): the direct, tunnel and caddy jobs, both bindings each, 0 failed, 0 xpassed, every listed expected failure xfailed and every listed skip skipped ([per variant](#in-ci-on-the-pull-request)). **Green on this host** before, for the same three variants | `3b890f3` (this host), **`4a53750`** (CI; the merge commit's tree) | nothing: the nightly `auth` variant's first run passed ([below](#the-nightly-auth-variant)) |
| [`anon_limits`](#anon_limits) | **Passed**: `TestAnonLimits` on its own with `-v`, and in the second review's full `make test-e2e` on the same tree; then in CI's `e2e` workflow on pull request #8 (`--- PASS: TestAnonLimits (64.33s)`) | `3b890f3`, **`4a53750`** (CI) | nothing |
| Demo from a clean laptop against staging ([staging](#the-staging-gate-gatewaykontrarun), [the laptop](#the-clean-laptop-against-staging)) | **Passed** in source mode against the staging gate deployed from `3b890f3`, and **passed in published mode at the merge commit** ([the published run](#the-published-run-at-the-merge-commit)): `go install kontra.sh/kontra/cmd/kontra@latest` installed `v0.0.0-20261031215600-4831e40fc393`, against the staging gate [redeployed from `4831e40`](#redeployed-from-the-merge-commit); commands 1 to 10 and the host checks passed, exit 0 | `3b890f3` (source), **`4831e40`** (published; the laptop and the staging gate) | **An anonymous tenant's purge observed on staging**: `gate-v4` was retired before its tenants' purge fell due, so the observation moved to staging's current gate, due 2026-11-02 23:43:01Z ([the purge](#the-purge-after-gate-v4s-retirement)) |
| [Team gate up on a clean VPS](#the-team-gate-on-a-clean-vps) | **Failed at `3b890f3`** at `docker compose up -d --build --wait`, on a defect of `compose.yaml` (two services built one image tag, which races on a fresh Ubuntu 24.04's containerd image store). **Passed at `f9f5e61`** with D313's fix: one build on Docker 29.1.3's containerd store, a Let's Encrypt certificate, a hire and an MCP `search` through the gate from another machine. Both droplets were torn down and the teardowns checked | `3b890f3` (failed), **`f9f5e61` (passed)** | nothing |

Every gate item has a passing run, and D306's condition on the merge, the published clean-laptop run at the merge
commit, passed at `4831e40` (22:05:54Z–22:08:42Z) against staging redeployed from it. **One item of D306's list is
still open**: an anonymous tenant's purge, observed on a running staging gate. `gate-v4` was retired (M5 PR-7) before
its first tenant's purge fell due, so the observation moved to staging's current gate; it falls due at 2026-11-02
23:43:01Z ([the purge](#the-purge-after-gate-v4s-retirement)). M4 is gated when it is confirmed. The nightly `auth`
variant's first run passed ([below](#the-nightly-auth-variant)). Production, `gateway.kontra.sh`, has run the merge commit since
21:58:48Z ([below](#production-gatewaykontrash-at-4831e40)).

Between the two commits the gate's code changed once: D312 (`7033e0a`) has `/mcp` refuse an `initialize` without an
`id` (400, never forwarded). D313 (`2eb8041`) changed `compose.yaml`, `scripts/team-gate-vps.sh` and their tests, and
`f9f5e61` adds this record. The TCK, `anon_limits` and the clean laptop were not run again on this host at `f9f5e61`;
`make check` and `make test-kits` passed with D313's change ([below](#make-test-kits)), the pull request's CI ran the
TCK and `anon_limits` at its head, `4a53750` (`f9f5e61`'s code), and the published laptop run was at the merge commit.

## The host

The runs were made from the project's build host: 2 CPUs shared with other work, Docker 24.0.5 with Compose 2.24.6 and
buildx 0.19.3 on the classic `overlay2` store, `GOTOOLCHAIN=go1.26.10`, one heavy run at a time. The staging manifests,
the Postgres password, the card-signing keys, the Cloudflare token and the DigitalOcean credentials stay outside the
repository; nothing in this file is a secret. Production (`gateway.kontra.sh`, namespace `kontra`), Traefik and the
FastAPI staging objects were not changed by any run: `gateway.kontra.sh/healthz` answered 200 after the staging apply,
at the end of the first runs, after the VPS rerun (21:20Z) and after the post-merge runs (22:09Z). Production was
deployed from the merge commit as its own step, before the post-merge runs, which only read it
([below](#production-gatewaykontrash-at-4831e40)).

## The staging gate, `gateway.kontra.run`

The plan's run B1: the M4 gate as the staging environment, in the cluster's `kontra-staging` namespace beside the
FastAPI staging objects, kept running afterwards as the README demo's target until M5.

| | |
|---|---|
| When | pre-flight 19:05:40Z; DNS 19:07:56Z; image built (139 s) and imported 19:09:29Z; server dry run and diff 19:09:47Z; apply 19:10:09Z, ready in 47 s; checks 19:11:07Z–19:11:43Z |
| Image | `BUILDX_BUILDER=default docker build -t kontra-gate:m4-3b890f3 --build-arg VERSION=m4-3b890f3 .` from the clean worktree: image id `sha256:90dac0bff928e47c8b562997160ac0eb164c865b414ca5d1d2ca6042f63f9e00` (37.8 MB); `docker save … \| gzip -1 \| ssh <platform worker> 'gunzip \| k3s ctr -n k8s.io images import -'` as `docker.io/library/kontra-gate:m4-3b890f3`, manifest `sha256:63ad7f84f24c43ad54c3fe5003ff74871e9d50d782ef167eb513badf53c0b2bd`; `kontra-gate version`: `kontra-gate m4-3b890f3 (go1.26.10, linux/amd64)` |
| Configuration | `spec/schema/examples/config.staging.json` at `3b890f3` with the Postgres password added: `public_url` `https://gateway.kontra.run`, `tenancy: multi`, `tls.acme` for `gateway.kontra.run`, `edge.client_ip` from PROXY v2 trusting `10.244.0.0/16`, `auth.api_keys`, `tunnel.anonymous` with §6.2's defaults (`ttl` 2h, 3 tenants per network, `streams` 8 from D308, `purge_after` 24h, `max_tenants` 200), no `services:` |
| Objects (13, all new) | Secrets `gate-v4-pg`, `gate-v4-signing`, `gate-v4-config`; Services `gate-v4`, `gate-v4-pg`, `gate-v4-temporal`; StatefulSets `gate-v4-pg` (`postgres:17-alpine`) and `gate-v4-temporal` (`temporalio/temporal:1.9.1` `start-dev` on a PVC); PVC `gate-v4-acme` (and the StatefulSets' `data-gate-v4-pg-0`, `data-gate-v4-temporal-0`, 10 Gi each, `hcloud-volumes`); Deployment `gate-v4` (1 replica, `Recreate`, an init container running `migrate up`, `imagePullPolicy: Never`, the platform worker's `nodeSelector`, pod security `restricted`); IngressRouteTCP `gate-v4` (`HostSNI(gateway.kontra.run)`, TLS passthrough, PROXY v2 to 8443); NetworkPolicies `gate-v4` and `gate-v4-backends`. All named `gate-v4*` and labelled `app.kubernetes.io/name=gate-v4`, `app.kubernetes.io/part-of=kontra-gateway-staging` |
| DNS (zone `kontra.run`) | A `ec974ffe8d5b6a4ae47547b766ebddbf` → `188.245.78.192` and AAAA `96a2ae7524198cb475ffbeedc2e2dfd5` → `2a01:4f8:c2c:a550::1`, proxied false, ttl 300, comment "Kontra v4 M4 staging gate (2026-10-31)"; a GET for the name found none before. Kept |
| Result | **up**: `migrate` applied `0001` to `0003` ("database at version 3"); the gate logged version `m4-3b890f3`, tenancy multi and its card-signing kid `Tzgy-taSXLZur0HZlbDFt8FSQ1OTQBPcxYNDcRT8cWw` |

**Pre-flight, read-only.** No pod labelled `app.kubernetes.io/name=gate-v4`; no IngressRoute, IngressRouteTCP or
Ingress in any namespace naming `gateway.kontra.run`; no ResourceQuota or LimitRange in `kontra-staging`; no object
named `gate-v4*`; `kubectl auth can-i create` yes for each kind applied; the namespace enforces pod security
`restricted`; `kubectl auth can-i list secrets -n kontra-staging --as system:serviceaccount:traefik:traefik` yes (and
for `kontra`). `kubectl apply --dry-run=server` created the 13 objects and `kubectl diff` showed only creations: no
existing object changed.

**Before the first apply**, WP-09 brought the private manifest WP-08 had written (29 Oct, never applied) up to this
commit: its `kontra.yaml` predated D308 and lacked `tunnel.anonymous.streams`, so it was rebuilt from
`config.staging.json` and checked equal to it but for `postgres:`; the Postgres password and the card-signing key were
generated afresh (the earlier password had been exposed in a working transcript); the `migrate` init container mounts
the signing secret, as production's does. The configuration was loaded by the image itself offline before the apply.

**Checked from the build host:**

```
$ curl -fsS https://gateway.kontra.run/healthz                    # IPv6 by default, then curl -4
ok    [200 2a01:4f8:c2c:a550::1]      ok    [200 188.245.78.192]
$ openssl s_client -connect gateway.kontra.run:443 -servername gateway.kontra.run </dev/null | openssl x509 -noout -issuer -subject -dates -ext subjectAltName
issuer=C = US, O = Let's Encrypt, CN = YE2
subject=CN = gateway.kontra.run
notBefore=Oct 31 18:12:29 2026 GMT
notAfter=Jan 29 18:12:28 2027 GMT
X509v3 Subject Alternative Name: DNS:gateway.kontra.run
(-verify_return_error: Verify return code: 0 (ok), TLSv1.3)
$ curl -s -o /dev/null -w '%{http_code}' https://gateway.kontra.run/agents/x
404
$ curl -si -X POST https://gateway.kontra.run/mcp …initialize…   # no credential
HTTP/2 401
www-authenticate: Bearer
{"jsonrpc":"2.0","id":null,"error":{"code":-32001,"message":"unauthenticated"}}
$ curl -si https://gateway.kontra.run/.well-known/jwks.json
HTTP/2 200 (application/jwk-set+json): one EC P-256 ES256 key, kid Tzgy-taSXLZur0HZlbDFt8FSQ1OTQBPcxYNDcRT8cWw
```

`/mcp` has no anonymous principal, and with no `auth.mcp_oauth` its challenge is a plain `Bearer`
([`relay.md`](../spec/relay.md) §12, §12.1). After the apply Traefik could still list secrets in `kontra-staging`, its
pod had not restarted (0 restarts, 5 days old), and the FastAPI staging Deployments `gate`, `console` and `temporal`
were unchanged. At 19:35:39Z, after the demo and the TCK, the three `gate-v4` pods were running with 0 restarts and
healthz answered 200.

**Left running on purpose** (staging until M5): the 13 objects and the StatefulSets' two PVCs above, the two DNS
records, the image in the platform worker's containerd, and `kontra-gate:m4-3b890f3` on the build host for redeploys.
Its teardown, when staging is retired, is label-scoped (`kubectl -n kontra-staging delete <kinds> -l
app.kubernetes.io/part-of=kontra-gateway-staging`, never `delete -f`), the two DNS records deleted by id, and
`k3s ctr -n k8s.io images rm docker.io/library/kontra-gate:m4-3b890f3` (and, since the redeploy below,
`docker.io/library/kontra-gate:m4-4831e40`).

### Redeployed from the merge commit

After the merge the gate was redeployed from `4831e40`, changing only the `gate-v4` Deployment's two images.

| | |
|---|---|
| When | checkout checked 22:01:01Z; image built 22:01:06Z (145 s) and imported 22:03:50Z; state read 22:04:07Z; server dry run and `set image` 22:04:30Z, rolled out in 7 s (22:04:37Z); checks 22:04:45Z–22:05:09Z |
| Checkout | a clean detached worktree of `origin/main`: `HEAD`, `origin/main` and `git ls-remote origin refs/heads/main` all `4831e40fc3931c4aadb2f5bce0dce6c8d7b8fb7f`, no changed file |
| Image | `BUILDX_BUILDER=default docker build -t kontra-gate:m4-4831e40 --build-arg VERSION=m4-4831e40 .`, exit 0: image id `sha256:c37f8b4bc63004fc53c7b810c5ffc499a71f18b37a347ab7157a9c116ec3bd8a` (37.8 MB); `docker save … \| gzip -1 \| ssh <platform worker> 'gunzip \| k3s ctr -n k8s.io images import -'` as `docker.io/library/kontra-gate:m4-4831e40` on `kontra-pool-platform-worker1`, manifest `sha256:1e05d8631c1b34195592f17ef9c9928c137ace3d87fecbed239218b8e2e5ca12`; `kontra-gate version`: `kontra-gate m4-4831e40 (go1.26.10, linux/amd64)` |
| Change | `kubectl -n kontra-staging set image deploy/gate-v4 gate=docker.io/library/kontra-gate:m4-4831e40 migrate=docker.io/library/kontra-gate:m4-4831e40`, after the same command with `--dry-run=server`: the Deployment from generation 1 to 2, nothing else changed (the configuration, the Secrets, the StatefulSets and the routing are `3b890f3`'s; `3b890f3` to `4831e40` changes no migration and not `config.staging.json`) |
| Result | **up**: both containers on image `sha256:c37f8b4b…`; `migrate` printed `no migrations to apply` and `database at version 3; this gate needs 3`; the gate logged `replica ready to serve`, version `m4-4831e40`, tenancy multi, services 0, the card-signing kid `Tzgy-taSXLZur0HZlbDFt8FSQ1OTQBPcxYNDcRT8cWw`; `kubectl exec … kontra-gate version`: `kontra-gate m4-4831e40 (go1.26.10, linux/amd64)`; 0 restarts, and at 22:09:15Z, after the published run, still 0 restarts and no `ERROR` log line |

**Checked from the build host** (22:04:59Z), as after the first apply: `/healthz` 200 `ok` over IPv4
(`188.245.78.192`) and IPv6 (`2a01:4f8:c2c:a550::1`); `/agents/x` 404; `/.well-known/jwks.json` 200
`application/jwk-set+json` with one ES256 P-256 key, kid `Tzgy-…`; `POST /mcp` `initialize` with no credential 401
with `www-authenticate: Bearer`; the same Let's Encrypt certificate (issuer `YE2`, `CN = gateway.kontra.run`, until
29 Jan 2027), verify return code 0, TLSv1.3.

**Nothing else changed.** Before and after (22:04:07Z, 22:05:09Z): `kubectl auth can-i list secrets --as
system:serviceaccount:traefik:traefik` yes in `kontra-staging` and in `kontra`, the Traefik pod at 0 restarts; the
FastAPI staging Deployments `console`, `gate` and `temporal` at the same generations (29, 31, 7) and images; namespace
`kontra` only read, and `gateway.kontra.sh/healthz` 200. Left: `kontra-gate:m4-4831e40` on the build host and in the
platform worker's containerd, and `m4-3b890f3` in both, for a rollback (`set image` back to it).

## `make test-kits`

| | At `3b890f3` | At `2eb8041` (D313; the code of `f9f5e61`) |
|---|---|---|
| When | 19:12:09Z, 856 s | 20:46Z–21:06Z, 1171 s |
| Command | `make test-kits` (`GOTOOLCHAIN=go1.26.10`) | the same, `KONTRA_TEST_PG` unset; `make check` (`GOFLAGS=-p=1`) had passed with the same change, exit 0 in 89 s |
| Result | **exit 0**, 20 top-level tests passed: `ok kontra.sh/kontra/test/vps 361.631s` (`TestTeamGateRehearsal` 229.6 s, `TestACMEVariantWritesItsCache` 115.9 s, the refusal, dry-run, pre-flight, teardown-only, reaping and run-teardown cases), `ok kontra.sh/kontra/test/demo 285.876s` (`TestDemoRehearsal` 280.7 s, `TestREADMEDemoBlock`, `TestDemoImage`, …) | **exit 0**, 21 top-level tests passed: `ok kontra.sh/kontra/test/vps 623.898s` (14, with D313's `TestTeamGateRecordsComposeFailure`; `TestTeamGateRehearsal` 485.8 s, in which the script's compose built the gate once from the shipped archive, `#16 naming to docker.io/library/kontra-gate:kitvps-3899213 done`, `gate-migrate` exited 0 on that image and the gate reported `kontra-gate m4 (go1.26.10, linux/amd64)`; `TestACMEVariantWritesItsCache` 119.1 s), `ok kontra.sh/kontra/test/demo 344.825s` (7; `TestDemoRehearsal` 338.4 s) |
| Teardown | the kits' own: the `kontra-gate:kit-<pid>` image and every container, volume and network they made were gone afterwards | the same: no image, container, volume or network beyond a snapshot taken before; the default builder's cache grew by about 2 GB and was left, as other projects share it |

The rehearsals run each kit end to end against a local gate (the VPS kit's steps 4 and 5 on `root@127.0.0.1` with a
local CA; the laptop against a local gate). They are not the gate runs. Since D313 the VPS rehearsal builds the gate as
the VPS does and counts the builds, but this host's Docker is not a fresh VPS's: only a droplet covers that
([below](#why-the-kits-did-not-see-it)).

## The clean laptop against staging

The plan's run B2, gate item "demo from a clean laptop against staging", in source mode (the commit is not on GitHub
yet). The published run, at the merge commit, is [below](#the-published-run-at-the-merge-commit).

| | |
|---|---|
| When | 19:26:38Z dry run; 19:26:42Z–19:29:09Z the run, 147 s (laptop ready in 136 s) |
| Command | `scripts/demo-clean-laptop.sh --transcript <file>` (after `--dry-run`, which listed the 10 commands, the two substitutions and the teardown, and built nothing); the host's `a2a-cli` on its `PATH` as `a2a`, so the script's optional host `a2a send` ran too. That binary is installed as `a2a-cli-v031` but reports `v0.3.2-0.20261016125318-609d0b5dd28f`, a build of a commit after v0.3.1; the laptop installs the pinned v0.3.1 itself |
| Laptop | `kontra-demo:3b890f3794e4`, target `laptop-src`, built from `git archive 3b890f3794e4`: `golang:1.26.10-bookworm` with Node v22.23.4, user `laptop` with an empty home, the default bridge, no mounts, no secrets |
| Result | **PASS, exit 0**: commands 1 to 10 of README.md's demo block all passed at the first attempt; the host checks passed with no credential |
| Slug | `anon-2d1d2dc15fff9177`, public URL `https://gateway.kontra.run/agents/anon-2d1d2dc15fff9177/hello` |
| Teardown | the script's trap: `docker rm -f kontra-demo-3b890f3794e4`, `docker image rm kontra-demo:3b890f3794e4`, its temporary directory; checked: none left |

Substitutions, as D298 allows in source mode: (a) `go install kontra.sh/kontra/cmd/kontra@latest` ran as `(cd /src &&
go install ./cmd/kontra)`, so the laptop's `kontra version` said `kontra dev (go1.26.10, linux/amd64)`; (b) `anon-<…>`
became the slug `kontra connect` printed. Every other command ran as written.

| # | Command (README.md) | Decisive output |
|---|---|---|
| 1–4 | install `kontra` and `a2a-cli` v0.3.1, `PATH`, `~/docs/notes.txt` | ok |
| 5–6 | `a2a server --exec 'echo Hello World' --port 9999 &`; `kontra connect --gate https://gateway.kontra.run --serve a2a:hello=… --serve 'mcp:fs=stdio:npx -y @modelcontextprotocol/server-filesystem@1.0.0 ~/docs' --public hello &` | the slug `anon-2d1d2dc15fff9177` |
| 7 | `a2a send -a http://localhost:7777/agents/anon-…/hello/.well-known/agent-card.json "hi"` | `Status: completed`, artifact `Hello World` |
| 8 | `npx -y @modelcontextprotocol/inspector@2.13.0 --cli http://localhost:7777/mcp/anon-…/fs --transport http --method tools/list` | lists `read_file` |
| 9 | the same client on `http://localhost:7777/mcp`, `tools/call` `hire` `service=anon-…/hello` `message=hi` | `"Hello World\n"`, `TASK_STATE_COMPLETED` |
| 10 | `curl -s https://gateway.kontra.run/agents/anon-…/hello/.well-known/agent-card.json` | the card naming the public URL, signed (kid `Tzgy-…`, `jku` the gate's JWKS), extension `https://kontra.sh/ext/anonymous/v1` with `expiresAt` `2026-10-31T21:28:21Z` |

From the build host, with no credential, while the laptop's node was connected: the public card (200, naming the
URL); a JSON-RPC `SendMessage` "hi" to `https://gateway.kontra.run/agents/anon-2d1d2dc15fff9177/hello` (a completed
task, artifact `Hello World\n`); and `a2a send -a <the public card> "hi"` (completed, `Hello World`).

The transcript, abridged (the full one, 790 lines with the `tools/list` result, is kept with the run's evidence):

```
demo-clean-laptop: 2026-10-31T19:26:42Z, commit 3b890f3794e4, mode source (target laptop-src), gate https://gateway.kontra.run
building kontra-demo:3b890f3794e4 (target laptop-src) from git archive 3b890f3794e4
  laptop | kontra-demo: laptop on x86_64, go version go1.26.10 linux/amd64, node v22.23.4, 10 commands, mode source
  laptop | kontra-demo: substitution (a): go install kontra.sh/kontra/cmd/kontra@latest  ->  (cd /src && go install ./cmd/kontra)
  laptop | kontra-demo: kontra version: kontra dev (go1.26.10, linux/amd64)
  laptop | kontra-demo: ok 1   … ok 2   … ok 3   … ok 4
  laptop | $ a2a server --exec 'echo Hello World' --port 9999 &          # or your own agent, in docker or not
  laptop | $ kontra connect --gate https://gateway.kontra.run --serve a2a:hello=http://127.0.0.1:9999   --serve 'mcp:fs=stdio:npx -y @modelcontextprotocol/server-filesystem@1.0.0 ~/docs' --public hello &
  laptop | kontra-demo: slug anon-2d1d2dc15fff9177
  laptop | $ a2a send -a http://localhost:7777/agents/anon-2d1d2dc15fff9177/hello/.well-known/agent-card.json "hi"
  laptop | Status:   completed (2026-10-31T19:28:23Z)
  laptop |   [01a1936f-3d54-72d9-96b1-2f8195638af1] Hello World
  laptop | kontra-demo: ok 7: "Hello World"
  laptop | $ npx -y @modelcontextprotocol/inspector@2.13.0 --cli http://localhost:7777/mcp/anon-2d1d2dc15fff9177/fs --transport http   --method tools/list
  laptop | kontra-demo: ok 8: "read_file"
  laptop | $ npx -y @modelcontextprotocol/inspector@2.13.0 --cli http://localhost:7777/mcp --transport http   --method tools/call --tool-name hire --tool-arg service=anon-2d1d2dc15fff9177/hello --tool-arg message=hi
  laptop |       "text": "Hello World\n"
  laptop |     "state": "TASK_STATE_COMPLETED"
  laptop | kontra-demo: ok 9: "Hello World"
  laptop | $ curl -s https://gateway.kontra.run/agents/anon-2d1d2dc15fff9177/hello/.well-known/agent-card.json
  laptop | kontra-demo: ok 10: "anon-2d1d2dc15fff9177"
  laptop | kontra-demo: ready
demo-clean-laptop: the laptop is ready (136 s), slug anon-2d1d2dc15fff9177; checking from this host
SendMessage to https://gateway.kontra.run/agents/anon-2d1d2dc15fff9177/hello: {… "parts":[{"text":"Hello World\n"}] …}
a2a send -a https://gateway.kontra.run/agents/anon-2d1d2dc15fff9177/hello/.well-known/agent-card.json "hi": Status: completed (2026-10-31T19:29:04Z) … Hello World
demo-clean-laptop: host checks passed; stopping the laptop
  laptop | kontra-demo: stopped
demo-clean-laptop: PASS (slug anon-2d1d2dc15fff9177, 145 s)
demo-clean-laptop: torn down (docker rm -f kontra-demo-3b890f3794e4; docker image rm kontra-demo:3b890f3794e4; rm -rf <the run's temporary directory>); exit 0
```

**The anonymous tenant**, read-only through the staging Postgres while the node was connected (19:28:59Z):

```
$ kubectl -n kontra-staging exec statefulset/gate-v4-pg -- psql -U kontra -d kontra -c "select slug, kind, created_from, created_at, expires_at, deleted_at from kontra.tenants where slug='anon-2d1d2dc15fff9177'"
 anon-2d1d2dc15fff9177 | anonymous | <the build host's /24> | 2026-10-31 19:28:21.884303+00 | 2026-10-31 21:28:21.884303+00 |
```

`created_from` is the /24 of the build host's public IPv4 (`'<host>'::inet <<= created_from` true) and does not
overlap `10.0.0.0/8` (false): Traefik's PROXY v2 header carried the client's address to the gate, not the cluster's.
It was the only tenant on the gate, and the run used one of the three anonymous sessions a /24 may open in two hours.
The tenant expired at 21:28:21Z; the gate purges it 24 h later (`purge_after`), leaving a tombstone, which is still
to be confirmed ([below](#the-anonymous-tenants-after-the-merge)).

### The published run, at the merge commit

D306's run after the merge: the README's block as a newcomer runs it, `kontra` from the module proxy, against the
staging gate [redeployed from the same commit](#redeployed-from-the-merge-commit).

| | |
|---|---|
| When | the module proxy's `@latest` checked 22:03Z; dry run 22:05:46Z; 22:05:54Z–22:08:42Z the run, 168 s (the script's own count 166 s; laptop ready in 157 s) |
| Command | `scripts/demo-clean-laptop.sh --published --expect 4831e40fc3931c4aadb2f5bce0dce6c8d7b8fb7f --transcript <file>`, after the same with `--dry-run` (the 10 commands, substitution (a) "none", the teardown; nothing built). `--expect` takes 12 to 40 hex digits: the short `--expect 4831e40` is refused with exit 2. The host's `a2a` on its `PATH` as in the source run |
| Module | `proxy.golang.org` already served `kontra.sh/kontra@latest` as `v0.0.0-20261031215600-4831e40fc393`, the merge commit's pseudo-version, so the run did not wait |
| Laptop | `kontra-demo:4831e40fc393`, target `laptop` (published mode), built from `git archive 4831e40fc393`: go1.26.10, Node v22.23.4, an empty home, the default bridge, no mounts, no secrets |
| Result | **PASS, exit 0**: commands 1 to 10 of README.md's demo block all passed at the first attempt, with no substitution but the slug; the host checks passed with no credential |
| `kontra version` | `kontra v0.0.0-20261031215600-4831e40fc393 (go1.26.10, linux/amd64)`: the merge commit, installed by `go install kontra.sh/kontra/cmd/kontra@latest` as written (`go: downloading kontra.sh/kontra v0.0.0-20261031215600-4831e40fc393`) |
| Slug | `anon-0d0d079ae3f6c7a5`, public URL `https://gateway.kontra.run/agents/anon-0d0d079ae3f6c7a5/hello` |
| Teardown | the script's trap: `docker rm -f kontra-demo-4831e40fc393`, `docker image rm kontra-demo:4831e40fc393`, its temporary directory; checked at 22:09:15Z: no `kontra-demo` container or image, no `/tmp/kontra-demo.*` |

| # | Command (README.md) | Decisive output |
|---|---|---|
| 1 | `go install kontra.sh/kontra/cmd/kontra@latest` | the module at `v0.0.0-20261031215600-4831e40fc393`; `kontra version` names it |
| 2–4 | install `a2a-cli` v0.3.1, `PATH`, `~/docs/notes.txt` | ok |
| 5–6 | `a2a server …`; `kontra connect --gate https://gateway.kontra.run … --public hello &` | the slug `anon-0d0d079ae3f6c7a5` |
| 7 | `a2a send -a http://localhost:7777/agents/anon-…/hello/.well-known/agent-card.json "hi"` | `Status: completed (2026-10-31T22:07:51Z)`, artifact `Hello World` |
| 8 | the inspector's `tools/list` on `http://localhost:7777/mcp/anon-…/fs` | lists `read_file` |
| 9 | the inspector's `tools/call` `hire` on `http://localhost:7777/mcp`, `service=anon-…/hello` `message=hi` | `"Hello World\n"`, `TASK_STATE_COMPLETED` |
| 10 | `curl -s https://gateway.kontra.run/agents/anon-…/hello/.well-known/agent-card.json` | the card naming the public URL, signed (ES256, kid `Tzgy-…`, `jku` `https://gateway.kontra.run/.well-known/jwks.json`), extension `https://kontra.sh/ext/anonymous/v1` with `expiresAt` `2026-11-01T00:07:49Z` |

From the build host, with no credential, while the laptop's node was connected: the public card (naming the URL); a
JSON-RPC `SendMessage` "hi" to `https://gateway.kontra.run/agents/anon-0d0d079ae3f6c7a5/hello` (a completed task,
artifact `Hello World\n`); and `a2a send -a <the public card> "hi"` (`Status: completed (2026-10-31T22:08:37Z)`,
`Hello World`).

The transcript, abridged (the full one, 786 lines, is kept with the run's evidence):

```
demo-clean-laptop: 2026-10-31T22:05:54Z, commit 4831e40fc393, mode published (target laptop), gate https://gateway.kontra.run
building kontra-demo:4831e40fc393 (target laptop) from git archive 4831e40fc393
  laptop | kontra-demo: laptop on x86_64, go version go1.26.10 linux/amd64, node v22.23.4, 10 commands, mode published
  laptop | $ go install kontra.sh/kontra/cmd/kontra@latest
  laptop | go: downloading kontra.sh/kontra v0.0.0-20261031215600-4831e40fc393
  …
  laptop | kontra-demo: kontra version: kontra v0.0.0-20261031215600-4831e40fc393 (go1.26.10, linux/amd64)
  laptop | kontra-demo: ok 1   … ok 2   … ok 3   … ok 4
  laptop | $ a2a server --exec 'echo Hello World' --port 9999 &          # or your own agent, in docker or not
  laptop | $ kontra connect --gate https://gateway.kontra.run --serve a2a:hello=http://127.0.0.1:9999   --serve 'mcp:fs=stdio:npx -y @modelcontextprotocol/server-filesystem@1.0.0 ~/docs' --public hello &
  laptop | kontra-demo: slug anon-0d0d079ae3f6c7a5
  laptop | kontra-demo: substitution (b): anon-<…>  ->  anon-0d0d079ae3f6c7a5
  laptop | $ a2a send -a http://localhost:7777/agents/anon-0d0d079ae3f6c7a5/hello/.well-known/agent-card.json "hi"
  laptop | Status:   completed (2026-10-31T22:07:51Z)
  laptop |   [01a19401-3afd-75ed-8292-5e3136c446b4] Hello World
  laptop | kontra-demo: ok 7: "Hello World"
  laptop | $ npx -y @modelcontextprotocol/inspector@2.13.0 --cli http://localhost:7777/mcp/anon-0d0d079ae3f6c7a5/fs --transport http   --method tools/list
  laptop | kontra-demo: ok 8: "read_file"
  laptop | $ npx -y @modelcontextprotocol/inspector@2.13.0 --cli http://localhost:7777/mcp --transport http   --method tools/call --tool-name hire --tool-arg service=anon-0d0d079ae3f6c7a5/hello --tool-arg message=hi
  laptop |       "text": "Hello World\n"
  laptop |     "state": "TASK_STATE_COMPLETED"
  laptop | kontra-demo: ok 9: "Hello World"
  laptop | $ curl -s https://gateway.kontra.run/agents/anon-0d0d079ae3f6c7a5/hello/.well-known/agent-card.json
  laptop | {"supportedInterfaces":[{"url":"https://gateway.kontra.run/agents/anon-0d0d079ae3f6c7a5/hello", …}kontra-demo: ok 10: "anon-0d0d079ae3f6c7a5"
  laptop | kontra-demo: ready
demo-clean-laptop: the laptop is ready (157 s), slug anon-0d0d079ae3f6c7a5; checking from this host
SendMessage to https://gateway.kontra.run/agents/anon-0d0d079ae3f6c7a5/hello: {… "parts":[{"text":"Hello World\n"}] …}
a2a send -a https://gateway.kontra.run/agents/anon-0d0d079ae3f6c7a5/hello/.well-known/agent-card.json "hi": Status: completed (2026-10-31T22:08:37Z) … Hello World
demo-clean-laptop: host checks passed; stopping the laptop
  laptop | kontra-demo: stopped
demo-clean-laptop: PASS (slug anon-0d0d079ae3f6c7a5, 166 s)
demo-clean-laptop: torn down (docker rm -f kontra-demo-4831e40fc393; docker image rm kontra-demo:4831e40fc393; rm -rf <the run's temporary directory>); exit 0
```

**Its anonymous tenant**, read-only (`default_transaction_read_only=on`) through the staging Postgres while the node
was connected (22:08:04Z, one live tunnel):

```
$ kubectl -n kontra-staging exec statefulset/gate-v4-pg -- env PGOPTIONS='-c default_transaction_read_only=on' psql -U kontra -d kontra -c "select now() as db_now, slug, kind, created_from, created_at, expires_at, deleted_at from kontra.tenants where slug='anon-0d0d079ae3f6c7a5'"
 2026-10-31 22:08:04.22815+00 | anon-0d0d079ae3f6c7a5 | anonymous | <the build host's /24> | 2026-10-31 22:07:49.608208+00 | 2026-11-01 00:07:49.608208+00 |
```

As in the source run, `created_from` is the /24 of the build host's public IPv4 (`'<host>'::inet <<= created_from`
true, overlap with `10.0.0.0/8` false); the tenant had its two services (`hello`, `fs`) and one tunnel. It was the
second anonymous session from that /24 on 31 Oct (19:26Z and 22:05Z); the first had expired, so one was live of the
three a network may hold in two hours.

### The anonymous tenants after the merge

The source run's tenant, read-only at 22:05:30Z, before the published run:

```
$ … psql … -c "select now() as db_now, slug, kind, created_from, created_at, expires_at, deleted_at, (expires_at < now()) as expired, expires_at + interval '24 hours' as purge_due from kontra.tenants where slug='anon-2d1d2dc15fff9177'"
 2026-10-31 22:05:30.658183+00 | anon-2d1d2dc15fff9177 | anonymous | <the build host's /24> | 2026-10-31 19:28:21.884303+00 | 2026-10-31 21:28:21.884303+00 |  | t | 2026-11-01 21:28:21.884303+00
```

**Expired, not purged, and not due**: `deleted_at` is null, as it should be until `purge_after` (24 h) has passed,
at 2026-11-01 21:28:21Z. It still had 2 services, 1 tunnel key, 0 tunnels and 0 rows in `anon_calls`. At 22:09:15Z,
after the published run, the gate held exactly these two tenants, both anonymous from the same /24, neither with a
live tunnel or `deleted_at` set; `anon-0d0d079ae3f6c7a5` expires at 2026-11-01 00:07:49Z and is due for purge at
2026-11-02 00:07:49Z. The purge of `anon-2d1d2dc15fff9177` is still to be confirmed ([below](#what-is-still-to-do)).

## The TCK (`tck_*`)

The plan's run A, gate item "`tck_*` green bar with expected failures": `tck_jsonrpc` and `tck_rest` are the direct
and tunnel variants, `tck_behind_caddy` the caddy variant (D279, [`relay.md`](../spec/relay.md) §16), each run once per
binding against the pinned a2a-tck `d4d6fae`, with `test/tck/expected-failures.txt` enforced both ways by
`test/tck/kontra_xfail.py`. On this host, one variant at a time, with `KONTRA_TEST_PG` a Postgres server (each run
creates and drops its own `tck_<pid>` database), the pinned Temporal CLI 1.9.1 and Caddy 2.11.7.

| Variant | When | `make tck` | jsonrpc | http_json |
|---|---|---|---|---|
| tunnel | 19:29:59Z | exit 0, 153 s | 82 passed, 173 skipped, **10 xfailed** (10 of 10 expected failures, 17 of 17 expected skips) | 76 passed, 180 skipped, **9 xfailed** (9 of 9, 16 of 16) |
| caddy | 19:32:47Z | exit 0, 152 s | 82 passed, 173 skipped, 10 xfailed (10 of 10, 17 of 17) | 76 passed, 180 skipped, 9 xfailed (9 of 9, 16 of 16) |
| direct | 20:27:50Z | exit 0, 114 s | 82 passed, 173 skipped, 10 xfailed (10 of 10, 17 of 17) | 76 passed, 180 skipped, 9 xfailed (9 of 9, 16 of 16) |

0 failed and 0 xpassed in every binding. Before each variant the kit's own tests of the xfail plugin passed (70). The
TCK's compatibility report names as failed requirements exactly the listed expected failures: in jsonrpc DM-MSG-001
(D5), PUSH-CREATE-001 and CORE-LIST-001 to 005 (D20, anonymous callers), CORE-HIST-001 and 002 (a2a-tck#242) and
CORE-HIST-003 (D7); in http_json the same without CORE-HIST-001 and 002, plus CORE-CANCEL-002 (a2a-tck#231). The JUnit,
HTML and compatibility reports and every process's log are kept with the run's evidence. Each run's trap dropped its
database, stopped its processes (gate, seller, node, Caddy, Temporal) and removed its directory: no `tck_` database
and no process was left.

The TCK checkout first named for the runs had a modified `uv.lock` (rewritten by a newer `uv`), which `scripts/tck.sh`
refuses by design; the runs used a clean clone at the same pin (`d4d6fae`, no modified file).

### In CI on the pull request

The `tck` workflow's first run: [run 41243927553](https://github.com/medmahmoudi26/kontra/actions/runs/41243927553),
on [pull request #8](https://github.com/medmahmoudi26/kontra/pull/8) (event `pull_request`) at its head `4a53750`
(`4a537505f8a7a53f52f4b3799e797666ff3ba766`, the merge commit's tree), created 21:30:47Z, **success**. One job per
variant on `ubuntu-24.04`, each fetching a2a-tck at the pin, running the kit's own tests of the xfail plugin (70
passed) and then `make tck VARIANT=<variant>` for both bindings:

| Job | Ran | jsonrpc | http_json |
|---|---|---|---|
| [`tck (direct)`](https://github.com/medmahmoudi26/kontra/actions/runs/41243927553/job/124663330818) | 21:30:49Z–21:33:05Z, success | 82 passed, 173 skipped, **10 xfailed** (10 of 10 expected failures, 17 of 17 expected skips) | 76 passed, 180 skipped, **9 xfailed** (9 of 9, 16 of 16) |
| [`tck (tunnel)`](https://github.com/medmahmoudi26/kontra/actions/runs/41243927553/job/124663330801) | 21:30:50Z–21:33:16Z, success | 82 passed, 173 skipped, 10 xfailed (10 of 10, 17 of 17) | 76 passed, 180 skipped, 9 xfailed (9 of 9, 16 of 16) |
| [`tck (caddy)`](https://github.com/medmahmoudi26/kontra/actions/runs/41243927553/job/124663330549) | 21:30:49Z–21:33:12Z, success | 82 passed, 173 skipped, 10 xfailed (10 of 10, 17 of 17) | 76 passed, 180 skipped, 9 xfailed (9 of 9, 16 of 16) |

0 failed and 0 xpassed in every binding, the same counts as on this host; each job's JUnit, HTML and compatibility
reports and its processes' logs are the run's artifacts `tck-direct`, `tck-tunnel` and `tck-caddy`. The workflow has no `push` trigger, so the merge commit has no `tck` run of
its own; its tree is `4a53750`'s. The same pull request's `check` workflow (`check` on Postgres 16 and 17, `go`,
`race`, `integration` on 16 and 17) and `e2e` workflow ([below](#anon_limits)) were green at `4a53750`.

### The nightly `auth` variant

The schedule (`41 3 * * *`) first ran at 2026-11-01 10:46:49Z (GitHub started the 03:41Z run late), on `main` at
`9f97492` (M4 plus the M5 gate side, #10): [`tck` run 41298966283](https://github.com/medmahmoudi26/kontra/actions/runs/41298966283),
all four variants green (direct, tunnel, caddy, auth). The `auth` job: jsonrpc 90 passed, 170 skipped, 5 xfailed
(`kontra_xfail: variant auth, bindings jsonrpc: 5 of 5 expected failures xfailed, 14 of 14 expected skips skipped`),
http_json 90 passed, 172 skipped, 3 xfailed (3 of 3, 8 of 8); 0 failed, 0 xpassed.

## `anon_limits`

`anon_limits` is `TestAnonLimits` in `test/e2e` ([`tunnel.md`](../spec/tunnel.md) §16, §6.2): on a multi-tenant gate
of two replicas behind the Front as a PROXY v2 forwarder, with `tunnel.anonymous` compressed (`ttl` 60 s, the caps
3/4/4/8/8, `parts_bytes` 1 MiB, `max_tenants` 12; D297), a 4th anonymous tenant from one /24 and from one IPv6 /48 is
`rate_limited`, one past `max_tenants` is `anonymous_unavailable`, a 5th service is `service_denied`, the 5th concurrent
task and the 9th concurrent call, stream and hiring stream are `429`, parts over the total are refused, the services
are absent from `search`, and at `expires_at` the session closes `revoked` and the URLs answer 404.

| | |
|---|---|
| When | 20:30:25Z, 78 s |
| Command | `scripts/test_e2e.sh -run '^TestAnonLimits$' -v` (`KONTRA_TEMPORAL_CLI` the pinned CLI, `KONTRA_TEST_PG` a Postgres server) |
| Result | **PASS**: `--- PASS: TestAnonLimits (71.50s)`, `ok kontra.sh/kontra/test/e2e 71.544s`, the script's leftover check clean (exit 0); the runtime limits took 7 s of the session's 60 s |

It also ran in the second review's full `make test-e2e` on the tree it committed as `3b890f3` (31 Oct, 18:43–19:02Z:
`ok kontra.sh/kontra/test/e2e 1155.406s`, the whole `test/e2e` package), and in CI's `e2e` workflow on pull request
#8 at `4a53750` ([run 41243927728](https://github.com/medmahmoudi26/kontra/actions/runs/41243927728), 21:30:49Z–21:49:22Z,
success): `--- PASS: TestAnonLimits (64.33s)`, `ok kontra.sh/kontra/test/e2e 1060.938s`, no test failed. The staging
runs showed the part of §6.2 a real deployment adds, the client's /24 carried through Traefik by PROXY v2.

## The team gate on a clean VPS

The plan's run C, gate item "team gate up on a clean VPS": `scripts/team-gate-vps.sh`, the ACME variant of
`compose.yaml` on a throwaway DigitalOcean droplet. **The first run, at `3b890f3`, failed at step 4 on a defect of
`compose.yaml`.** D313 fixed it (`2eb8041`), and **the rerun, at `f9f5e61`, passed**: compose built the gate's image
once on a fresh droplet's containerd image store, the gate got its Let's Encrypt certificate, and a node on another
machine enrolled; its service was hired through the gate with an API key and found through `/mcp`. Both droplets and
both DNS records were deleted by the script's trap, and each teardown was checked by hand.

| | First run | Rerun |
|---|---|---|
| Commit | `3b890f3` | `f9f5e61` |
| Result | **FAIL, exit 1**, at step 4 | **PASS, exit 0** |
| Droplet | id **614595251** | id **614610093** |
| DNS record | id **`dcd828bd791bd0ef06882021d9caa090`** | id **`2d7f37ac23e44fb9f1c5bc07bbbf8d02`** |
| Lifetime and cost | 5 min 34 s; at most $0.0357, about $0.0033 pro rata | 11 min 6 s; at most $0.0357, about $0.0066 pro rata |
| Teardown | the trap; checked at 19:44:36Z and 20:12:59Z | the trap, then `--teardown-only`; checked at 21:20:09Z |

### The first run, at `3b890f3` (failed)

| | |
|---|---|
| When | pre-flight 19:37:36Z; dry run 19:38:25Z; the run 19:38:44Z–19:44:21Z |
| Command | `scripts/team-gate-vps.sh --dry-run`, then `scripts/team-gate-vps.sh`, with `KONTRA_VPS_DO_KEY`, `KONTRA_VPS_SSH_KEY` and `KONTRA_VPS_PROTECTED` set (D306) and `CLOUDFLARE_API_TOKEN` in the environment |
| Droplet | `kontra-m4-teamgate-1793475527`, id **614595251**, `164.92.142.134`, fra1, `s-2vcpu-4gb` (`$0.035710/h`), `ubuntu-24-04-x64`, tags `kontra-m4-gaterun` and `ttl-1793489927`; cloud-init gave Docker 29.1.3 (`docker.io`), Compose 2.40.3, buildx 0.30.1 |
| DNS | A `m4-teamgate.kontra.run` → `164.92.142.134`, id **`dcd828bd791bd0ef06882021d9caa090`**, comment "M4 VPS gate run, delete after"; 1.1.1.1 resolved it before step 4 |
| Result | **FAIL, exit 1**: `docker compose up -d --build --wait` failed after about 3 minutes with `target gate: failed to solve: image "docker.io/library/kontra-gate:m4": already exists` |
| Lifetime and cost | 5 min 34 s; at most $0.0357 (one hour at $0.035710/h), about $0.0033 pro rata |
| Teardown | the script's EXIT trap: the DNS record deleted by id, then the droplet; "torn down; nothing left", checked twice by hand (below) |

**Pre-flight**, read-only: `doctl` context `default`; the account's SSH key named by `KONTRA_VPS_DO_KEY` had the MD5
fingerprint of the local key's `.pub` (`ssh-keygen -E md5 -lf`); `s-2vcpu-4gb` available in fra1; no droplet tagged
`kontra-m4-gaterun`; no Cloudflare record named `m4-teamgate.kontra.run` or with the run's comment. The dry run passed
its pre-flight, printed the teardown and created nothing (exit 0).

**The run's record**, as `team-gate-vps.sh` printed it:

```
team-gate-vps: 2026-10-31T19:38:47Z, commit 3b890f3794e4, kontra-m4-teamgate-1793475527 (fra1, s-2vcpu-4gb at $0.035710/h, ubuntu-24-04-x64), https://m4-teamgate.kontra.run
== 19:38:47 2. create kontra-m4-teamgate-1793475527
droplet 614595251 at 164.92.142.134
cloud-init: …status: done
  vps | Docker Compose version 2.40.3+ds1-0ubuntu1~24.04.1
  vps | github.com/docker/buildx 0.30.1 0.30.1-0ubuntu1~24.04.1
== 19:40:56 3. DNS: A m4-teamgate.kontra.run -> 164.92.142.134
record dcd828bd791bd0ef06882021d9caa090
1.1.1.1 resolves m4-teamgate.kontra.run to 164.92.142.134
== 19:41:06 4. ship git archive 3b890f3794e4 and bring compose up
configured: deploy/compose/kontra.run.yaml, .env (KONTRA_PORT=443), a new signing key
  vps | #18 [gate] exporting to image
  vps | #18 exporting manifest list sha256:85ecb26cb0f5b921b754391df7c8ecdd9ece4ce3ec6e675f1f1f5111fce31fb9 0.0s done
  vps | #18 naming to docker.io/library/kontra-gate:m4 done
  vps | #18 ERROR: image "docker.io/library/kontra-gate:m4": already exists
  vps | #17 [gate-migrate] exporting to image
  vps | #17 exporting manifest list sha256:041999d335b43a482175a3d7b7da037e88eefadd29853b92f6c279ba71f26996 0.0s done
  vps | #17 naming to docker.io/library/kontra-gate:m4 done
  vps | #17 CANCELED
  vps | target gate: failed to solve: image "docker.io/library/kontra-gate:m4": already exists
team-gate-vps: teardown (exit 1)
DELETE dns_records/dcd828bd791bd0ef06882021d9caa090 (m4-teamgate.kontra.run)
doctl compute droplet delete -f 614595251   # kontra-m4-teamgate-1793475527
droplet lifetime: 5 min 34 s; cost at most $0.0357 (1 h at $0.035710/h)
team-gate-vps: torn down; nothing left
```

**Not reached**: healthz and the Let's Encrypt certificate on `m4-teamgate.kontra.run`, the port check, `enroll`, the
team member's container, `a2a send` and the MCP `search` with the key, and the record of `docker compose ps` and the
image digests. Only one droplet was allowed for the run, so it was not repeated before the fix; the rerun is the
second droplet.

**Its teardown, checked.** At 19:44:36Z and again at 20:12:59Z: `doctl compute droplet list --tag-name
kontra-m4-gaterun --no-header` empty (exit 0, 0 lines), and `doctl compute droplet get 614595251` answered 404; the
only droplet of the account was the build host's, which `KONTRA_VPS_PROTECTED` named. Cloudflare: no record named
`m4-teamgate.kontra.run`, none with the comment "M4 VPS gate run, delete after", and a GET of record
`dcd828bd791bd0ef06882021d9caa090` answered 81044 ("Record does not exist"); the zone's 28 records were identical (id,
type, name, content) to the snapshot taken before the run, `gateway.kontra.run`'s A and AAAA among them. No
`kontra-demo`, `kontra-teammate` or repro container and no `kontra-demo` image were left; `164.92.142.134` was not in
`known_hosts`; the run's temporary directories and the repro's container and volume were removed.
`gateway.kontra.sh/healthz` and `gateway.kontra.run/healthz` answered 200.

### The defect

`compose.yaml`'s `x-gate` anchor carries both `image: kontra-gate:m4` and `build:`, and both `gate-migrate` and `gate`
merge it (`<<: *gate`), so `docker compose up --build` builds the same tag twice, in parallel. A fresh Ubuntu 24.04
now installs `docker.io` 29.1.3 (containerd 2.2.1), whose new installs use the containerd image store
(`io.containerd.snapshotter.v1`); there the second export of one tag can fail with "already exists". It is a race:
in an `ubuntu:24.04` container with the three packages cloud-init installs (`docker.io` 29.1.3, `docker-compose-v2`
2.40.3, `docker-buildx` 0.30.1, a nested `dockerd`), a two-service stack shaped like `3b890f3`'s failed 5 of 15
fresh tries (build cache pruned and images removed between tries). The README's own `docker compose up -d --build
--wait` meets the same race for any operator on a current Ubuntu.

### Why the kits did not see it

The build host runs Docker 24.0.5 with Compose 2.24.6 on the classic `overlay2` store, where naming one image twice
is harmless, and `test/vps` and `test/demo` override `gate-migrate`'s image (`build: !reset null`, or `--no-build`), so
`make test-kits` never builds the tag twice on a containerd image store. A green `make test-kits` says nothing about a
fresh VPS's Docker. The script's record printed Compose's and buildx's versions but not the engine's or its storage
driver, and kept the last 20 lines of Compose's output; `docker version` and `docker info --format '{{.Driver}}
{{.DriverStatus}}'` in the record would have named the cause at once. D313 changed both: the rehearsal builds the gate
as the VPS does and counts the builds, and the record names the engine, containerd, runc and the storage driver, and
keeps compose's whole output when it fails.

### The fix, D313 (`2eb8041`)

Only `gate` builds the image, and `gate-migrate` runs the image `gate` builds; compose builds every image a command
needs before it creates any container, so the image exists when `gate-migrate` starts:

```diff
 x-gate: &gate
   image: kontra-gate:m4
-  build:
-    context: .
-    args: { VERSION: m4 }
   read_only: true
@@
   gate:
     <<: *gate
+    build:
+      context: .
+      args: { VERSION: m4 }
     command: ["serve", "-f", "/etc/kontra/kontra.yaml"]
```

Verified before it was committed, in the same `ubuntu:24.04` container with Docker 29.1.3 and Compose 2.40.3: the
two-service shape with the fix failed 0 of 15 fresh tries, and its upgrade path (`up --build` over a running stack)
and `build` then `up --no-build` passed; on the build host's Compose 2.24.6 it passes too. The whole patched
`compose.yaml`, with a real gate build, capped at 1 CPU: `docker compose up -d --build --wait` exit 0 in 366 s, the tag
named once, every service healthy or exited 0, `kontra-gate version` `kontra-gate m4 (go1.26.10, linux/amd64)`,
healthz 200, and an API key and an enrollment token minted. A new named volume over the image's `/var/lib/kontra-gate`
(owned by 65532) stays writable for 65532 on the containerd store, so the ACME cache works there.

Committed as `2eb8041` with D313. `TestComposeTeamGate` (`test/pentest`, in `make check`) and
`TestComposeConfigResolves` (`docker compose config`) check that no service but `gate` builds and that `gate-migrate`
runs the gate's image; both fail on `7033e0a`'s `compose.yaml`. `scripts/team-gate-vps.sh`'s step 4 records the host's
Docker (engine, containerd, runc, the storage driver and its status, compose, buildx) and the images compose built,
and on a failure compose's whole output, `docker compose ps -a` and each service's last 40 log lines
(`TestTeamGateHostMode`, `TestTeamGateRecordsComposeFailure`). Compose 2.24.6 first tries to pull `gate-migrate`'s
image, prints `gate-migrate Pulling` and `Error`, and goes on; `docker compose run --rm gate-migrate` alone now needs
`docker compose build gate` first, which the README's upgrade paragraph says.

### The rerun, at `f9f5e61` (passed)

The same procedure on a second droplet, at the head that has D313.

| | |
|---|---|
| When | pre-flight 21:07:50Z; dry run 21:08:18Z; the run 21:08:27Z–21:19:37Z (droplet 21:08:31Z, DNS 21:10:24Z, compose 21:10:35Z, checks from this host 21:14:40Z, record 21:19:16Z); explicit teardown 21:19:55Z; teardown checked 21:20:09Z |
| Command | as the first run's: `scripts/team-gate-vps.sh --dry-run`, then `scripts/team-gate-vps.sh`, then `scripts/team-gate-vps.sh --teardown-only` |
| Droplet | `kontra-m4-teamgate-1793480911`, id **614610093**, `157.230.110.237`, fra1, `s-2vcpu-4gb` (`$0.035710/h`), `ubuntu-24-04-x64`, tags `kontra-m4-gaterun` and `ttl-1793495311`; cloud-init done |
| The droplet's Docker | Engine 29.1.3, containerd 2.2.1, runc 1.3.4; storage driver `overlayfs`, `driver-type io.containerd.snapshotter.v1`: the containerd image store, where `3b890f3`'s two builds failed. Compose 2.40.3, buildx 0.30.1 |
| DNS | A `m4-teamgate.kontra.run` → `157.230.110.237`, id **`2d7f37ac23e44fb9f1c5bc07bbbf8d02`**, comment "M4 VPS gate run, delete after"; 1.1.1.1 resolved it before step 4 |
| `docker compose up -d --build --wait` | **exit 0 in 232 s**, one build (`#17 naming to docker.io/library/kontra-gate:m4 done`); `gate-migrate`, `temporal-schema` and `temporal-namespace` exited 0 (`gate` and `temporal` start only on their successful completion); `postgres` and `gate` healthy, `temporal` up |
| From the build host | `/healthz` 200; the certificate's issuer `C = US, O = Let's Encrypt, CN = YE2`, subject `CN = m4-teamgate.kontra.run`; ports 22 and 443 open, 80, 5432, 7233, 8443 and 9090 closed |
| Minted on the VPS | an API key `m4run` (scope `hire`, tenant `default`) and a single-use enrollment token for `hello`; neither recorded |
| The team member | `kontra-demo:f9f5e61b3ae6`, built on this host for this head, as container `kontra-teammate-1793480911` on the default bridge: `kontra` installed from the image's source (`git archive f9f5e61b3ae6`, `go install ./cmd/kontra`), `a2a-cli` v0.3.1 (`go install …@v0.3.1`), `a2a server --exec 'echo Hello World'`, `kontra connect --enroll`: "connected to https://m4-teamgate.kontra.run as k1_vjqsoszafrmkmzlyco6wrfpihp (tenant default, …)", `hello` at `/agents/hello` |
| Hired with the key | `SendMessage` "hi" with `curl` to `https://m4-teamgate.kontra.run/agents/hello`: a task with the artifact `Hello World\n`; `a2a send` with the host's `a2a` (the clean laptop's host check used the same binary, which reports `v0.3.2-0.20261016125318-609d0b5dd28f`): `Status: completed`, artifact `Hello World`; MCP `initialize` and `tools/call` `search` on `/mcp`: `services` `[{ref: hello, kind: a2a, available: true}]` |
| Recorded | `docker compose ps`: `gate` healthy on `0.0.0.0:443->8443`, `[::]:443->8443` and `127.0.0.1:9090`, `postgres` healthy, `temporal` up; `docker compose images`: `gate` and `gate-migrate` both on `kontra-gate:m4`, image `0652b5e9f172`; `kontra-gate version`: `kontra-gate m4 (go1.26.10, linux/amd64)`; the digests below |
| Lifetime and cost | 11 min 6 s; at most $0.0357 (billed as one hour at $0.035710/h), about $0.0066 pro rata |
| Teardown | the script's EXIT trap: the team member's container and the `kontra-demo` image removed, the DNS record deleted by id, then the droplet, `ssh-keygen -R` for its address, its temporary directory removed: "torn down; nothing left"; then `--teardown-only` (exit 0, "nothing of a run is left"); checked (below) |

| Image on the droplet | Digest |
|---|---|
| `kontra-gate:m4` (`gate` and `gate-migrate`) | `sha256:0652b5e9f17259bdc473381f7a463fb20f7b7bc08ddd35570cfb9faee2070eea` |
| `postgres:16-alpine` | `sha256:0cefaabd882e42bcb2a2f3dc56d3ce4770560ba62fd7f197f1a2e008fd5731ab` |
| `temporalio/admin-tools:1.32.1` | `sha256:9ef0dc5b063bf4ffd5c1512de72e0e73e225bd2c7006a18ce69bd592a8862a83` |
| `temporalio/server:1.32.1` | `sha256:acecbab5b0b8ae9cf4012b508866ca97feda3709577bf157b537239f40029049` |

**Pre-flight**, read-only, as the first run's: `doctl` context `default`, active; the account's SSH key named by
`KONTRA_VPS_DO_KEY` had the MD5 fingerprint of the local key's `.pub`; `s-2vcpu-4gb` at $0.035710/h; no droplet tagged
`kontra-m4-gaterun`; no Cloudflare record named `m4-teamgate.kontra.run` or with the run's comment. The dry run passed
its pre-flight, printed the teardown and created nothing (exit 0).

**The run's record**, as `team-gate-vps.sh` printed it, abridged (`…`):

```
team-gate-vps: 2026-10-31T21:08:31Z, commit f9f5e61b3ae6, kontra-m4-teamgate-1793480911 (fra1, s-2vcpu-4gb at $0.035710/h, ubuntu-24-04-x64), https://m4-teamgate.kontra.run
== 21:08:31 2. create kontra-m4-teamgate-1793480911
droplet 614610093 at 157.230.110.237
cloud-init: …status: done
== 21:10:24 3. DNS: A m4-teamgate.kontra.run -> 157.230.110.237
record 2d7f37ac23e44fb9f1c5bc07bbbf8d02
1.1.1.1 resolves m4-teamgate.kontra.run to 157.230.110.237
== 21:10:35 4. ship git archive f9f5e61b3ae6 and bring compose up
configured: deploy/compose/kontra.run.yaml, .env (KONTRA_PORT=443), a new signing key
  vps | docker Engine 29.1.3; containerd 2.2.1; runc 1.3.4-0ubuntu1~24.04.1; docker-init 0.19.0; client 29.1.3
  vps | storage driver overlayfs [["driver-type","io.containerd.snapshotter.v1"]]
  vps | Docker Compose version 2.40.3+ds1-0ubuntu1~24.04.1
  vps | github.com/docker/buildx 0.30.1 0.30.1-0ubuntu1~24.04.1
compose up --build --wait: 232 s; the images it built:
  vps | #17 naming to docker.io/library/kontra-gate:m4 done
  …
  vps |  Container kontra-temporal-schema-1  Exited
  vps |  Container kontra-temporal-1  Healthy
  vps |  Container kontra-gate-migrate-1  Exited
  vps |  Container kontra-postgres-1  Healthy
  vps |  Container kontra-temporal-namespace-1  Exited
  vps |  Container kontra-gate-1  Healthy
== 21:14:40 5. verify from this host
GET https://m4-teamgate.kontra.run/healthz: 200
issuer=C = US, O = Let's Encrypt, CN = YE2
subject=CN = m4-teamgate.kontra.run
port 22: open
port 80: closed
port 443: open
port 5432: closed
port 7233: closed
port 8443: closed
port 9090: closed
minted API key "m4run": principal key:kk_m4run_…, tenant default, …, scopes hire; the key is shown once
minted an enrollment token for service hello (tenant default): single use, valid until 2026-10-31T21:29:47Z; it is shown once
an API key kk_… and a token for hello (neither recorded)
the team member's clean machine: kontra-teammate-1793480911
  …
  teammate | kontra connect: connected to https://m4-teamgate.kontra.run as k1_vjqsoszafrmkmzlyco6wrfpihp (tenant default, session 01a193d4-9a42-7b0e-8768-9c7818133040)
  teammate |   a2a hello  https://m4-teamgate.kontra.run/agents/hello
SendMessage "hi" to https://m4-teamgate.kontra.run/agents/hello with the key: {"jsonrpc":"2.0","id":1,"result":{"task":{"id":"01a193d4-ab26-7bcd-be1c-f93db9e150cc","artifacts":[{…,"parts":[{"text":"Hello World\n"}]}],…
a2a send: Task:     01a193d4-b488-75fc-950f-94c25b4ed0a7
…
Status:   completed (2026-10-31T21:19:13Z)
  the service ended the task: completed
Artifacts:
  [01a193d4-b5e0-751b-a1b7-51ba86f7fdd8] Hello World
…
MCP tools/call search on https://m4-teamgate.kontra.run/mcp with the key: {"jsonrpc":"2.0","id":2,"result":{…,"structuredContent":{"services":[{"ref":"hello","kind":"a2a","name":"hello","description":"Wraps command: echo Hello World","available":true}]}}}
== 21:19:16 record
  vps | NAME                IMAGE                      … SERVICE    … STATUS                   PORTS
  vps | kontra-gate-1       kontra-gate:m4             … gate       … Up 4 minutes (healthy)   127.0.0.1:9090->9090/tcp, 0.0.0.0:443->8443/tcp, [::]:443->8443/tcp
  vps | kontra-postgres-1   postgres:16-alpine         … postgres   … Up 4 minutes (healthy)   5432/tcp
  vps | kontra-temporal-1   temporalio/server:1.32.1   … temporal   … Up 4 minutes
  vps | CONTAINER                     REPOSITORY               TAG                 PLATFORM            IMAGE ID            …
  vps | kontra-gate-1                 kontra-gate              m4                  linux/amd64         0652b5e9f172        …
  vps | kontra-gate-migrate-1         kontra-gate              m4                  linux/amd64         0652b5e9f172        …
  …
  vps | kontra-gate m4 (go1.26.10, linux/amd64)
  vps | kontra-gate:m4 sha256:0652b5e9f17259bdc473381f7a463fb20f7b7bc08ddd35570cfb9faee2070eea 0652b5e9f172
  …
team-gate-vps: PASS (kontra-m4-teamgate-1793480911, https://m4-teamgate.kontra.run, 10 min)
team-gate-vps: teardown (exit 0)
DELETE dns_records/2d7f37ac23e44fb9f1c5bc07bbbf8d02 (m4-teamgate.kontra.run)
doctl compute droplet delete -f 614610093   # kontra-m4-teamgate-1793480911
droplet lifetime: 11 min 6 s; cost at most $0.0357 (1 h at $0.035710/h)
team-gate-vps: torn down; nothing left
```

On success the record keeps compose's `naming to` lines and its last 20 lines, which do not reach back to compose's
attempt to pull `gate-migrate`'s image: whether Compose 2.40.3 prints the `gate-migrate Pulling` and `Error` that 2.24.6
prints is not recorded. Either way compose built once and `gate-migrate` ran the gate's image.

**Its teardown, checked.** At 21:19:55Z `scripts/team-gate-vps.sh --teardown-only` reaped again: exit 0, "nothing of a
run is left". At 21:20:09Z, read-only: `doctl compute droplet list --tag-name kontra-m4-gaterun --no-header` empty
(exit 0, 0 lines), `doctl compute droplet get 614610093` answered 404, and the account's only droplet was the build
host's, which `KONTRA_VPS_PROTECTED` named. Cloudflare: no record named `m4-teamgate.kontra.run`, none with the comment
"M4 VPS gate run, delete after", and a GET of record `2d7f37ac23e44fb9f1c5bc07bbbf8d02` answered 81044 ("Record does
not exist"); the zone's 28 records were identical (id, type, name, content) to the snapshot taken before the run,
`gateway.kontra.run`'s A and AAAA among them; 1.1.1.1 answered NXDOMAIN for `m4-teamgate.kontra.run`. On the build
host: no `kontra-demo` or `kontra-teammate` container and no `kontra-demo` image; `157.230.110.237` was not in
`known_hosts`; the run's temporary directory was empty and was removed; containers, images, volumes and networks were
identical to the snapshot taken before the run. `gateway.kontra.sh/healthz` and `gateway.kontra.run/healthz` answered
200.

## Production, `gateway.kontra.sh`, at `4831e40`

Not a gate item: production runs the gate single-tenant, with no anonymous sessions (they need `tenancy: multi`, which
is Cloud's at M5; D268, D280). It was deployed from the merge commit by the orchestrator right after the merge, before
the staging redeploy, the same way as at M3: the image built from the merge commit, imported on the platform worker,
and `kubectl -n kontra set image deploy/gateway` for the `gate` and `migrate` containers, the configuration unchanged.
Its state was read back read-only at 22:12Z for this record.

| | |
|---|---|
| When | merged 21:56:00Z; the new ReplicaSet created and its pod started 21:58:48Z (Deployment `gateway` revision 5, generation 6); `replica ready to serve` 21:58:59Z; checked 21:59:16Z–21:59:43Z |
| Image | `docker.io/library/kontra-gate:4831e40`, in both containers, image id `sha256:a6cc1a36e5daf7b4c3d94d3000c41f9e448913f0279160ca1d44cee606bfc0ab` |
| Migration | `migrate`: `OK up 0003_m4.sql (37ms)`, `database at version 3; this gate needs 3` |
| The gate | `replica ready to serve`, version `4831e40`, tenancy single, `services` 4; 0 restarts and no `ERROR` log line at 22:12Z |
| `/mcp` | with the owner's API key: `initialize`'s `serverInfo` names version `4831e40`; `tools/call` `search` lists the four services, `hello`, `hello-tunnel`, `everything` and `fs`; `tools/call` `hire` of `hello-tunnel` completes through the node (the gate's log: a `gate-mcp` session on `/mcp` at 21:59:40Z, `hire created a task` for `hello-tunnel` at 21:59:43Z, its tunnel attempt placed on the node's session) |
| Unchanged | both nodes reconnected on their own: `hello-tunnel`'s at 21:59:02Z, the MCP node's (`everything`, `fs`) at 21:59:16Z, and both MCP services opened sessions through it (21:59:20Z, 21:59:24Z); the MCP services and the A2A services `hello` and `hello-tunnel` answer as at M3; `/healthz` 200 at 22:04Z and 22:09Z |
| Rollback | M3's image `kontra-gate:98a9f25` (revision 4) stays imported |

## What is still to do

Done since the first runs: `compose.yaml`'s fix with its decision entry and tests, and the record of the host's Docker
in `team-gate-vps.sh` (D313, `2eb8041`); the VPS run on a head that has them (`f9f5e61`, passed). Done after the
merge, on 31 Oct: the `tck` workflow's first CI run on pull request #8, green for the direct, tunnel and caddy
variants ([run 41243927553](https://github.com/medmahmoudi26/kontra/actions/runs/41243927553)), with `anon_limits` in
CI's `e2e` on the same pull request; production deployed from the merge commit; staging redeployed from it (only
`gate-v4`'s images changed; healthz, the certificate and Traefik's secrets checked); and the published clean-laptop run
at it, which passed with `kontra version` naming the merge commit.

Still to do:

1. **An anonymous tenant's purge** (D306's last item), on staging's current gate: `anon-835027ae5bdf23d3` must show
   `deleted_at` set after 2026-11-02 23:43:01Z ([the purge](#the-purge-after-gate-v4s-retirement)). M4 is gated when
   this is confirmed.

### The purge after `gate-v4`'s retirement

M5's PR-7 (kontra-cloud #75, #76; 1 Nov) moved staging's data plane under helm: the Go gate at `gateway.kontra.run`
is now kontra-cloud's chart-managed `gate` (image `kontra-dataplane:9f97492`, tenancy multi, `tunnel.anonymous` with
`purge_after: 24h`), and the hand-applied `gate-v4` was deleted by label around 15:30Z, before
`anon-2d1d2dc15fff9177`'s purge fell due (21:28:21Z). With no gate running on its database, that purge can never
happen, so it is not evidence either way; `gate-v4`'s volumes and secrets are left as they were, for the release's
clean-up. The purge itself is covered by `internal/anon`'s `TestPurgeLeavesTheTombstone`,
`TestPurgeDeletesTheObjectsOfEveryTask` and `TestPurgeClaimsATenant`, and `internal/gate`'s
`TestAnonymousPurgeDeletesObjectsWithoutRows`, which pass in CI on `main`.

To observe it on a running gate, an anonymous session was opened on staging's current gate at 2026-11-01 21:43Z from
this host (`kontra connect --gate https://gateway.kontra.run --serve a2a:hello=… --public hello --listen off`, the CLI
at `9f97492`): tenant `anon-835027ae5bdf23d3`, its public card served at
`https://gateway.kontra.run/agents/anon-835027ae5bdf23d3/hello`, the session ending at 23:43:01Z. Its purge falls due at
2026-11-02 23:43:01Z and is checked after that.
