# charts/kontra

One Kontra gate (`kontra-gate`, built from the repository's `Dockerfile`) on Kubernetes, with an optional
bundled Postgres 17 and Temporal development server for small deployments. `values.yaml` documents every
value and the `kontra.yaml` keys that depend on the chart.

```sh
helm install kontra charts/kontra -n kontra --create-namespace -f my-values.yaml
```

What it runs:

- **The gate**: a Deployment of one replica, replaced with `Recreate` (built-in ACME is for one replica, D70,
  and its cache volume is `ReadWriteOnce`). An init container runs `kontra-gate migrate -f
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

Every pod and container meets the `restricted` Pod Security Standard (non-root, seccomp `RuntimeDefault`, no
privilege escalation, all capabilities dropped, read-only root), so the namespace can enforce it:
`kubectl label namespace kontra pod-security.kubernetes.io/enforce=restricted`.

`ci/production-values.yaml` is the shape gateway.kontra.sh runs (its Secret names and sellers rule are
placeholders); `ci/team-values.yaml` is a small team gate. Both are what `helm lint` and `helm template` are
run with.
