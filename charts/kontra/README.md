# charts/kontra

One Kontra gate (`kontra-gate`, built from the repository's `Dockerfile`) on Kubernetes, with an optional
bundled Postgres 17 and Temporal development server for small deployments. `values.yaml` documents every
value and the `kontra.yaml` keys that depend on the chart.

```sh
helm install kontra charts/kontra -n kontra --create-namespace -f my-values.yaml
```

What it runs:

- **The gate**: a Deployment of `gate.replicas` (one by default), replaced with `Recreate` while it keeps the ACME
  cache volume (built-in ACME is for one replica, D70, and its cache volume is `ReadWriteOnce`). An init container runs `kontra-gate migrate -f
  /etc/kontra/kontra.yaml up`; the gate runs `serve -f /etc/kontra/kontra.yaml` on 8443 (public) and 9090
  (metrics), with `tcpSocket` probes on 8443, the ACME cache on a volume at `/var/lib/kontra-gate`, an
  `emptyDir` at `/tmp` and a read-only root. `kontra.yaml` comes from a Secret: yours (`config.existingSecret`)
  or one the chart makes from `config.kontraYaml`.
- **Postgres** (`postgres.enabled`): `postgres:17-alpine` as uid 70, one 10Gi volume. Its password reaches
  the gate as `PGPASSWORD`, so `kontra.yaml`'s URL names none.
- **Temporal** (`temporal.enabled`): `temporalio/temporal:1.9.1`, `server start-dev --headless` with SQLite in
  a 10Gi volume, as uid 1000, with the two dynamic-config settings spec/relay.md §14.2 asks for. Its frontend
  has no authorizer, so only the gate may reach it (§14.4).
- **Traefik** (`ingressRouteTCP.enabled`): an `IngressRouteTCP` passing TLS through by SNI to the gate, with
  PROXY protocol v2; set `kontra.yaml`'s `edge.client_ip` to `proxy_protocol` and trust the Traefik pods (D69).
- **NetworkPolicies** (`networkPolicy.enabled`, on by default): the gate takes connections on 8443 only from
  the front (Traefik's pods by default) and reaches DNS, its Postgres and Temporal, the sellers you list, and
  public addresses on 443, never the private ranges; the bundled Postgres and Temporal take connections from
  the gate alone.

**The card-signing key** (D166, D183). A gate that serves A2A services at a public `public_url` (not a loopback
origin) signs every served card and refuses to start without `signing_key`. Put a PEM file of ECDSA P-256 private
keys in a Secret and name it in `signingKey.existingSecret` (and `signingKey.key`, default `kontra.key`): the chart
mounts that one key read-only at `/etc/kontra/signing/kontra.key` with mode `0440`, readable through the pod's
`fsGroup`, in the gate container only (the migrate init container does not see it). `kontra.yaml` then says
`signing_key: file:/etc/kontra/signing/kontra.key`. Every replica reads the same file; the first key signs and
every key is published at `/.well-known/jwks.json`, so a rotation prepends the new key and drops the old one later.

```sh
openssl ecparam -name prime256v1 -genkey -noout | openssl pkcs8 -topk8 -nocrypt > kontra.key
kubectl -n kontra create secret generic kontra-signing-key --from-file=kontra.key
helm upgrade kontra charts/kontra -n kontra -f my-values.yaml --set signingKey.existingSecret=kontra-signing-key
```

**More than one replica: the peer hop** (spec/relay.md §11.2, D163, D179, D183). Only the replica holding a node's
tunnel session can open a stream on it, so a replica that receives an MCP request for a session pinned to another
replica's tunnel forwards it to that replica's peer listener. `gate.replicas` above 1 needs `gate.peer.enabled`
(without it about half of the pinned MCP requests behind a balancer end with a `404` and a new `initialize`) and
`persistence.enabled: false` (use a provided certificate, or a front that terminates TLS); the chart refuses either
combination. `gate.peer` renders the container port (`gate.peer.port`, 8444), the shared secret from your Secret
(`gate.peer.secret.existingSecret`, key `gate.peer.secret.key`, at least 32 bytes) in the variable
`gate.peer.secretEnv` (`KONTRA_PEER_SECRET`) of the gate container only, and NetworkPolicy ingress to the port from
the release's gate pods with the matching egress: the HMAC authenticates a forwarded request and does not hide it,
so the port must stay within the release. `kontra.yaml` then says
`peer: {listen: ":8444", secret_env: KONTRA_PEER_SECRET}`; each replica advertises its pod address.

```sh
kubectl -n kontra create secret generic kontra-peer --from-literal=secret="$(openssl rand -base64 48)"
helm upgrade kontra charts/kontra -n kontra -f my-values.yaml --set gate.replicas=2 --set persistence.enabled=false \
  --set gate.peer.enabled=true --set gate.peer.secret.existingSecret=kontra-peer
```

Every pod and container meets the `restricted` Pod Security Standard (non-root, seccomp `RuntimeDefault`, no
privilege escalation, all capabilities dropped, read-only root), so the namespace can enforce it:
`kubectl label namespace kontra pod-security.kubernetes.io/enforce=restricted`.

`ci/production-values.yaml` is the shape gateway.kontra.sh runs (its Secret names and sellers rule are
placeholders); `ci/team-values.yaml` is a small team gate. Both are what `helm lint` and `helm template` are
run with.
