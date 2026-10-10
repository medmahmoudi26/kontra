/**
 * THE `local` FLEET PROVIDER — Lima VMs on the control-plane host, k3s inside (ADR 0066).
 *
 * The same mechanics as a cloud fleet, on a laptop or a CI runner: real VMs (QEMU with KVM, or
 * Apple's Virtualization.framework), a real k3s cluster, gVisor as a containerd runtime, the same
 * manifests. What differs from `digital_ocean` is only who makes the machines — PRD goal 3.
 *
 * ONE VM PER NODE, NAMED `kontra-<fleet>-<n>`. Node 0 is the k3s server; the others join it as
 * agents over Lima's `user-v2` network, where every VM reaches every other as
 * `lima-<name>.internal` with no root on the host. The server's API port is forwarded to a host
 * port derived from the fleet's name, so two local fleets do not fight over 6443.
 *
 * EVERYTHING DOWNLOADED IS PINNED: the k3s release and the installer script by digest, the gVisor
 * release by its SHA-512. A converge that would install whatever is newest today is a fleet that
 * differs from yesterday's for no reason anybody chose.
 *
 * The cluster join token is generated once per fleet and kept in the provider's state directory
 * (0600); it is the one secret this provider makes, and it never leaves the host.
 */
import { createHash, randomBytes } from 'node:crypto';
import { chmodSync, existsSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import path from 'node:path';

import { dump as dumpYaml } from 'js-yaml';

import type { FleetProfile, LocalProfile } from '../fleets';
import type { FleetProvider, ProvisionedCluster } from './registry';

export const K3S = {
  version: 'v1.36.5+k3s1',
  /** The installer script at that tag, by digest (raw.githubusercontent.com/k3s-io/k3s/<tag>/install.sh). */
  installer: 'https://raw.githubusercontent.com/k3s-io/k3s/v1.36.5%2Bk3s1/install.sh',
  installerSha256: '46177d4c99440b4c0311b67233823a8e8a2fc09693f6c89af1a7161e152fbfad',
} as const;

export const GVISOR = {
  release: '20261005',
  sha512: {
    x86_64:
      '27fed731b432d6a39d908956623fd273979fda66b3ccf4b9c12413ea417c516e18a9dc3cced403fb28efe495d64dce4251f51cf8c4d9f4a72f9833bd96b6bb35',
    aarch64:
      'cb1c1ac7616f1f0e2287d39660aa9a65f1d81b866c7fa29457f78da62373baadda2d55aa96a200743dfe56193e43d2b6256af23a23d41d0b49a49b4971982435',
  },
} as const;

/** A registry name pods pull from, and where a node reaches it. */
export interface RegistryMirror {
  /** The host part of image references, e.g. `zot.kontra:5000`. */
  name: string;
  /** Where the node fetches it, e.g. `http://host.lima.internal:5000`. */
  endpoint: string;
}

export function vmName(fleet: string, index: number): string {
  return `kontra-${fleet}-${index}`;
}

/** The node index a VM name has in `fleet`, or undefined when it is not one of the fleet's nodes. */
export function nodeIndex(fleet: string, name: string): number | undefined {
  const prefix = `kontra-${fleet}-`;
  if (!name.startsWith(prefix)) return undefined;
  const rest = name.slice(prefix.length);
  return /^(0|[1-9][0-9]*)$/.test(rest) ? Number(rest) : undefined;
}

/** The host port the server's API is forwarded to: stable per fleet, inside 16443–17442. */
export function apiPort(fleet: string): number {
  const n = createHash('sha256').update(fleet).digest().readUInt32BE(0);
  return 16443 + (n % 1000);
}

/**
 * The gVisor install, verified by SHA-512, and its registration as a containerd runtime. SHARED WITH
 * THE `digital_ocean` PROVIDER, so the two fleets cannot drift onto different pins.
 *
 * `apt` IS THE ONE SEAM, and it exists for a measured reason that applies to a droplet and not to a
 * Lima VM here: a fresh DigitalOcean image is still running unattended-upgrades when SSH comes up,
 * and `apt-get update` then dies on `/var/lib/apt/lists/lock` (programs/machine.ts, step 1). The
 * droplet script passes a retrying wrapper; this provider passes nothing and its script is the same
 * bytes it always was.
 */
export const gvisorInstall = (apt = 'apt-get'): string => `ARCH=$(uname -m)
case "$ARCH" in
  x86_64) WANT=${GVISOR.sha512.x86_64} ;;
  aarch64) WANT=${GVISOR.sha512.aarch64} ;;
  *) echo "no pinned gVisor for $ARCH" >&2; exit 1 ;;
esac
if ! command -v runsc >/dev/null; then
  ${apt} update -q && ${apt} install -y -q zstd
  curl -fsSL -o /tmp/gvisor.tar.zstd https://storage.googleapis.com/gvisor/releases/release/${GVISOR.release}/$ARCH/gvisor.tar.zstd
  echo "$WANT  /tmp/gvisor.tar.zstd" | sha512sum -c -
  tar --zstd -xf /tmp/gvisor.tar.zstd -C /usr/local/bin
  rm -f /tmp/gvisor.tar.zstd
fi
# gVisor as a containerd runtime, in k3s's containerd 2 template (config-v3).
mkdir -p /var/lib/rancher/k3s/agent/etc/containerd
cat > /var/lib/rancher/k3s/agent/etc/containerd/config-v3.toml.tmpl <<'TOML'
{{ template "base" . }}

[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.runsc]
  runtime_type = "io.containerd.runsc.v1"
TOML`;

/** The pinned k3s install: the installer verified by digest, then run at the pinned version. Shared
 *  with the `digital_ocean` provider for the same reason as {@link gvisorInstall}. */
export const k3sInstall = (exec: string, env: string): string => `curl -fsSL -o /tmp/k3s-install.sh ${K3S.installer}
echo "${K3S.installerSha256}  /tmp/k3s-install.sh" | sha256sum -c -
${env} INSTALL_K3S_VERSION='${K3S.version}' INSTALL_K3S_EXEC='${exec}' sh /tmp/k3s-install.sh`;

/** The Lima instance file for node `index` of `fleet`. */
export function limaConfig(opts: {
  fleet: string;
  index: number;
  profile: LocalProfile;
  token: string;
  registry?: RegistryMirror;
}): string {
  const { fleet, index, profile, token, registry } = opts;
  const server = index === 0;
  const serverHost = `lima-${vmName(fleet, 0)}.internal`;
  const registries = registry
    ? `mkdir -p /etc/rancher/k3s
cat > /etc/rancher/k3s/registries.yaml <<'YAML'
mirrors:
  "${registry.name}":
    endpoint: ["${registry.endpoint}"]
YAML
`
    : '';
  const exec = server
    ? `server --disable traefik --disable servicelb --write-kubeconfig-mode 0600 --tls-san ${serverHost} --tls-san 127.0.0.1 --node-name ${vmName(fleet, index)}`
    : `agent --node-name ${vmName(fleet, index)}`;
  const env = server ? `K3S_TOKEN='${token}'` : `K3S_URL='https://${serverHost}:6443' K3S_TOKEN='${token}'`;
  const doc: Record<string, unknown> = {
    minimumLimaVersion: '2.0.0',
    base: 'template:_images/ubuntu-lts',
    cpus: profile.size.cpus,
    memory: profile.size.memory.replace(/i$/, 'iB'),
    // k3s owns containerd on the node; Lima's own would be a second one.
    containerd: { system: false, user: false },
    networks: [{ lima: 'user-v2' }],
    mounts: [],
    provision: [
      {
        mode: 'system',
        script: `#!/bin/sh\nset -eu\n${gvisorInstall()}\n${registries}${k3sInstall(exec, env)}\n`,
      },
    ],
    probes: server
      ? [
          {
            description: 'the k3s server has written its kubeconfig',
            script:
              '#!/bin/sh\nset -eu\nif ! timeout 300s sh -c "until test -f /etc/rancher/k3s/k3s.yaml; do sleep 3; done"; then\n  echo "k3s did not come up" >&2\n  exit 1\nfi\n',
          },
        ]
      : [],
    // THE API PORT ONLY, and nothing else from any node: Lima forwards every listening guest port
    // by default, and two nodes' kubelets (10250) would race for one host port.
    portForwards: [
      ...(server ? [{ guestPort: 6443, hostPort: apiPort(fleet), hostIP: '127.0.0.1' }] : []),
      { guestPortRange: [1, 65535], ignore: true },
    ],
  };
  return dumpYaml(doc, { lineWidth: -1, noRefs: true });
}

/** One command, as the provider runs it. Injectable so the provider is testable without Lima. */
export type Runner = (cmd: string, args: string[]) => Promise<{ code: number; stdout: string; stderr: string }>;

interface LimaInstance {
  name: string;
  status: string;
}

export class LocalProvider implements FleetProvider {
  constructor(
    private readonly run: Runner,
    /** Where instance files and each fleet's join token live. */
    private readonly stateDir: string,
    private readonly registry?: RegistryMirror
  ) {}

  private async limactl(args: string[]): Promise<string> {
    const r = await this.run('limactl', args);
    if (r.code !== 0) throw new Error(`limactl ${args[0]}: exit ${r.code}: ${r.stderr.trim().slice(-500)}`);
    return r.stdout;
  }

  private async instances(fleet: string): Promise<LimaInstance[]> {
    const out = await this.limactl(['list', '--json']);
    return out
      .split('\n')
      .filter((l) => l.trim().startsWith('{'))
      .map((l) => JSON.parse(l) as LimaInstance)
      // EXACTLY this fleet's nodes: a prefix match would make `dev`'s destroy delete `dev-x`'s VMs.
      .filter((i) => nodeIndex(fleet, i.name) !== undefined);
  }

  private token(fleet: string): string {
    const dir = path.join(this.stateDir, fleet);
    const file = path.join(dir, 'token');
    if (existsSync(file)) return readFileSync(file, 'utf8').trim();
    mkdirSync(dir, { recursive: true, mode: 0o700 });
    const token = randomBytes(32).toString('hex');
    writeFileSync(file, `${token}\n`, { mode: 0o600 });
    chmodSync(file, 0o600);
    return token;
  }

  async converge(fleet: string, profile: FleetProfile): Promise<ProvisionedCluster> {
    if (profile.provider !== 'local') throw new Error(`the local provider was handed a ${profile.provider} profile`);
    const token = this.token(fleet);
    const have = new Map((await this.instances(fleet)).map((i) => [i.name, i.status]));
    // SERVER FIRST: an agent that boots before the server it joins retries for minutes.
    for (let i = 0; i < profile.nodes; i += 1) {
      const name = vmName(fleet, i);
      const status = have.get(name);
      if (status === 'Running') continue;
      if (status === undefined) {
        const file = path.join(this.stateDir, fleet, `${name}.yaml`);
        writeFileSync(file, limaConfig({ fleet, index: i, profile, token, registry: this.registry }), { mode: 0o600 });
        await this.limactl(['create', `--name=${name}`, '--tty=false', file]);
      }
      await this.limactl(['start', '--tty=false', name]);
    }
    // FEWER NODES THAN BEFORE: the highest-numbered agents go. Never the server.
    for (const [name] of have) {
      const index = nodeIndex(fleet, name)!;
      if (index >= profile.nodes && index > 0) {
        await this.limactl(['delete', '--force', name]);
      }
    }
    const raw = await this.limactl(['shell', vmName(fleet, 0), 'sudo', 'cat', '/etc/rancher/k3s/k3s.yaml']);
    const kubeconfig = raw.replace(/server: https:\/\/127\.0\.0\.1:6443/, `server: https://127.0.0.1:${apiPort(fleet)}`);
    const nodes = Array.from({ length: profile.nodes }, (_, i) => ({
      name: vmName(fleet, i),
      address: `lima-${vmName(fleet, i)}.internal`,
    }));
    return { kubeconfig, flavor: 'k3s', nodes };
  }

  async destroy(fleet: string): Promise<void> {
    for (const i of await this.instances(fleet)) await this.limactl(['delete', '--force', i.name]);
  }
}
