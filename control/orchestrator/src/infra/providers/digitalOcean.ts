/**
 * THE `digital_ocean` FLEET PROVIDER — k3s on droplets, in a VPC of the fleet's own (ADR 0066 slice 9).
 *
 * The same cluster `local` makes, on rented machines: node 0 is the k3s server, the others join it
 * as agents, gVisor is a containerd runtime on every node, and both are installed from the SAME
 * pinned scripts `local.ts` uses (k3s by version with its installer checked by SHA-256, gVisor by
 * SHA-512). What differs is only who makes the machines — PRD goal 3 — and the three things a
 * public cloud adds: a network boundary, a credential, and a bill.
 *
 * A PULUMI PROGRAM, RUN THROUGH THE AUTOMATION API EXACTLY AS THE LEGACY FLEET IS: `selectStack`
 * (infra/workspace.ts) owns the `file://` backend, the passphrase and the refusal to run against
 * Pulumi's SaaS, and this file adds a project to it — `kontra-do-fleet`, one stack per profile.
 *
 * ═══ WHAT A DROPLET IS GIVEN, AND HOW ═══
 *
 *   - NOTHING IN `user_data`. ADR 0064 measured user_data as readable from the droplet's metadata
 *     service, which every process on the node can reach. Everything a node is told travels over
 *     SSH from the control plane, after the firewall below exists.
 *   - THE JOIN TOKEN IS THE SERVER'S OWN. The k3s server generates it on first start; a command
 *     reads it back over SSH (as a Pulumi secret, with logging off), and each agent receives it on
 *     the STDIN of its install command, which writes it to a 0600 file k3s reads through
 *     `K3S_TOKEN_FILE`. So it is never in a command line (`/proc/<pid>/cmdline` on the node), never
 *     in a Pulumi diagnostic, and never in an error message that quotes the script. kontra never
 *     invents one, so there is no second copy on the control plane to drift from the server's — a
 *     regenerated token would be refused by a server whose datastore was sealed with the first.
 *   - THE KUBECONFIG IS READ BACK THE SAME WAY, as a secret output, with its `server:` rewritten
 *     from the droplet's loopback to its public address (which the server's certificate names, via
 *     `--tls-san`).
 *
 * ═══ THE FIREWALL ═══
 *
 * From outside the fleet: TCP 6443 (the Kubernetes API) and TCP 22 (the install and read-back
 * commands above) from the control plane's address ONLY, and nothing else. SSH is in the list because
 * the token and the kubeconfig travel over it; leaving it to the world would make the one channel
 * that carries them the widest door on the node. Inside the fleet: everything from the fleet's own
 * VPC range, because k3s needs node-to-node traffic (the agents' API connection, flannel's VXLAN,
 * the kubelet) and the VPC is this fleet's alone. Outbound is open: a node pulls images and its pods
 * dial the control plane, and what a POD may reach is the tenant NetworkPolicy's job (decision 4),
 * not a cloud firewall's.
 *
 * The control plane's address is configuration, `KONTRA_FLEET_CONTROL_PLANE_IPS`, read at converge.
 * UNSET REFUSES: a firewall that opened 6443 "for now" is the thing this provider exists to not make.
 *
 * ═══ SECRETS: WHERE EACH ONE GOES, AND ONLY THERE ═══
 *
 *   - `token` (the profile's DigitalOcean token): read here, from the profile, and handed to the
 *     workspace as `DIGITALOCEAN_TOKEN` in its `envVars` — the one place workspace.ts point 3 MEASURED
 *     to leave no trace in config or state. Not even an explicit `digitalocean.Provider` resource
 *     gets it: a provider's inputs are persisted, encrypted, in the very state directory ADR 0034
 *     keeps cloud tokens out of. {@link DoFleetArgs} has no field for it, so the program cannot
 *     interpolate it anywhere.
 *   - the SSH private key: read from `KONTRA_SSH_KEY` BEFORE anything is created — a missing key used
 *     to surface only once droplets were up and billing (.env.quickstart, "the expensive way") — and
 *     passed to the commands' `connection`, which @pulumi/command marks secret.
 *   - `ssh_key_fingerprint` is not a secret, but it is read from the profile here and nowhere else.
 *
 * NOTHING HERE HAS BEEN RUN AGAINST DIGITALOCEAN. The owner has not cleared cloud spend (PRD §13):
 * the resource graph is proved under Pulumi mocks (`digitalOcean.program.test.ts`) and the wrapper
 * with a fake engine (`digitalOcean.test.ts`). A live run is a decision for the owner.
 */
import { readFileSync } from 'node:fs';
import { isIPv4, isIPv6 } from 'node:net';

import * as command from '@pulumi/command';
import * as digitalocean from '@pulumi/digitalocean';
import * as pulumi from '@pulumi/pulumi';
import type { OutputMap } from '@pulumi/pulumi/automation';

import type { DigitalOceanProfile, FleetProfile } from '../fleets';
import { selectStack, type StackRef } from '../workspace';
import { gvisorInstall, k3sInstall, vmName } from './local';
import type { FleetNode, FleetProvider, ProviderRun, ProvisionedCluster } from './registry';

/** The Pulumi project for droplet fleets. One stack per profile: the pool is the profile's one writer. */
export const DO_FLEET_PROJECT = 'kontra-do-fleet';

/** The variable the DigitalOcean provider reads its token from (the same one `stacks.ts` names). */
export const DO_TOKEN_VAR = 'DIGITALOCEAN_TOKEN';

/** Where the firewall admits the API and SSH from. */
export const CONTROL_PLANE_VAR = 'KONTRA_FLEET_CONTROL_PLANE_IPS';

/** The droplet image. An LTS slug, the one the k3s and gVisor pins in `local.ts` are run against. */
export const DROPLET_IMAGE = 'ubuntu-24-04-x64';

/** k3s's own pod and service ranges. A VPC that overlaps either routes pod traffic into the VPC. */
export const K3S_CLUSTER_CIDRS = ['10.42.0.0/16', '10.43.0.0/16'] as const;

/** Every droplet kontra makes carries this tag, as the legacy fleet's droplets do; nothing selects on it. */
export const SHARED_TAG = 'kontra-fleet';

/**
 * The tag that binds a fleet's droplets to its firewall.
 *
 * A COLON, BECAUSE A TAG-BOUND FIREWALL APPLIES TO EVERY DROPLET IN THE ACCOUNT CARRYING THE TAG. The
 * legacy fleet tags its droplets `kontra-<tag>` with a tag of `[a-z][a-z0-9-]`, so a dash-joined
 * name here (`kontra-fleet-dev`, `kontra-k3s-dev`) is one a legacy fleet can also produce — and this
 * firewall would then close a legacy machine's ports under it. A legacy tag cannot hold a colon.
 */
export function fleetTag(fleet: string): string {
  return `kontra-fleet:${fleet}`;
}

/** The size letter a fleet uses: `m` when the profile has one, else the first it lists. */
export function sizeLetter(sizes: Record<string, string>): string {
  const letters = Object.keys(sizes);
  if (letters.length === 0) throw new Error('the profile has no sizes — a map of size letter to droplet slug');
  return letters.includes('m') ? 'm' : letters[0]!;
}

// ── addresses ─────────────────────────────────────────────────────────────────────────────────────

function ipv4ToInt(ip: string): number {
  return ip.split('.').reduce((n, part) => n * 256 + Number(part), 0);
}

function parseCidr4(cidr: string): { net: number; prefix: number } | undefined {
  const m = /^(\d{1,3}(?:\.\d{1,3}){3})\/(\d{1,2})$/.exec(cidr.trim());
  if (!m || !isIPv4(m[1]!)) return undefined;
  const prefix = Number(m[2]);
  if (prefix > 32) return undefined;
  return { net: ipv4ToInt(m[1]!), prefix };
}

function within(a: { net: number; prefix: number }, b: { net: number; prefix: number }): boolean {
  // Does `a` lie inside `b`? Compare under b's mask. Division, not bit shifts: JS shifts are 32-bit signed.
  const block = 2 ** (32 - b.prefix);
  return a.prefix >= b.prefix && Math.floor(a.net / block) === Math.floor(b.net / block);
}

function overlaps(a: { net: number; prefix: number }, b: { net: number; prefix: number }): boolean {
  return within(a, b) || within(b, a);
}

const RFC1918 = ['10.0.0.0/8', '172.16.0.0/12', '192.168.0.0/16'].map((c) => parseCidr4(c)!);

/**
 * The fleet's VPC range, checked before anything is created.
 *
 * RFC 1918 and between /16 and /24 are DigitalOcean's own rules (the provider's `ipRange` docs); they
 * are checked here so a typo refuses with the profile's key in the message rather than after a
 * plugin download. The k3s overlap is kontra's: k3s puts pods in 10.42.0.0/16 and services in
 * 10.43.0.0/16, and a node whose VPC overlaps either sends pod traffic out of the node instead of
 * into flannel — a cluster that installs, reports Ready, and cannot reach its own DNS.
 *
 * NOT CHECKED, AND SAID SO: DigitalOcean also refuses a range that overlaps ANY other VPC in the
 * account, so two profiles in one account need two `vpc_cidr`s. That needs the account's VPC list,
 * which is a cloud call; DigitalOcean's own refusal names the clash.
 */
export function checkVpcCidr(cidr: string): void {
  const where = `vpc_cidr ${JSON.stringify(cidr)}`;
  const c = parseCidr4(cidr);
  if (!c) throw new Error(`${where} is not an IPv4 CIDR like 10.200.0.0/20`);
  if (!RFC1918.some((r) => within(c, r))) throw new Error(`${where} is not a private (RFC 1918) range, which a DigitalOcean VPC must be`);
  if (c.prefix < 16 || c.prefix > 24) throw new Error(`${where}: a DigitalOcean VPC is between a /16 and a /24`);
  for (const k of K3S_CLUSTER_CIDRS) {
    if (overlaps(c, parseCidr4(k)!)) {
      throw new Error(`${where} overlaps ${k}, which k3s uses for its pods or services — pick a range outside 10.42.0.0/15`);
    }
  }
}

/**
 * Where the fleet's firewall admits the Kubernetes API and SSH from: the control plane's public
 * address(es) as a droplet sees them, comma-separated, each an address or a CIDR.
 *
 * A CIDR IS ALLOWED, AND A WIDE ONE IS NOT. A control plane behind a cloud NAT can leave from a small
 * range, so a /24 (IPv4) or a /64 (IPv6) is accepted. Anything wider is a neighbourhood rather than
 * an address, and `0.0.0.0/0` is the open API port this variable exists to prevent.
 */
export function controlPlaneSources(env: NodeJS.ProcessEnv = process.env): string[] {
  const raw = env[CONTROL_PLANE_VAR]?.trim();
  if (!raw) {
    throw new Error(
      `${CONTROL_PLANE_VAR} is not set: a digital_ocean fleet admits its Kubernetes API and SSH from the ` +
        `control plane's public address only, and this install has not said what that is`
    );
  }
  const out: string[] = [];
  for (const part of raw.split(',').map((p) => p.trim()).filter(Boolean)) {
    const [addr, bits, extra] = part.split('/');
    const v4 = isIPv4(addr ?? '');
    const v6 = isIPv6(addr ?? '');
    const prefix = bits === undefined ? (v4 ? 32 : 128) : /^\d{1,3}$/.test(bits) ? Number(bits) : NaN;
    const narrowest = v4 ? 24 : 64;
    const widest = v4 ? 32 : 128;
    if (extra !== undefined || (!v4 && !v6) || !(prefix >= narrowest && prefix <= widest)) {
      throw new Error(
        `${CONTROL_PLANE_VAR}: ${JSON.stringify(part)} is not an address or a CIDR of /${narrowest} or narrower — ` +
          'the fleet firewall admits the control plane, not a range around it'
      );
    }
    out.push(part);
  }
  if (out.length === 0) throw new Error(`${CONTROL_PLANE_VAR} names no address`);
  return out;
}

/** An address from the cloud, checked before it is written into a shell script. */
function ipv4(what: string, v: unknown): string {
  if (typeof v !== 'string' || !isIPv4(v)) throw new Error(`${what} is not an IPv4 address: ${JSON.stringify(v)}`);
  return v;
}

// ── what runs on a node ───────────────────────────────────────────────────────────────────────────

/** Where k3s reads the join token on an agent. 0600, written from the install command's stdin. */
export const JOIN_TOKEN_FILE = '/etc/rancher/k3s/join-token';
const SERVER_TOKEN_FILE = '/var/lib/rancher/k3s/server/node-token';
const KUBECONFIG_FILE = '/etc/rancher/k3s/k3s.yaml';

/**
 * What every node runs before it may touch apt.
 *
 * BOTH HALVES WERE MEASURED ON DROPLETS (programs/machine.ts, step 1): cloud-init is still running
 * when DigitalOcean reports the droplet up, and `apt-get update` then dies on
 * `/var/lib/apt/lists/lock`, which `DPkg::Lock::Timeout` does not cover. So: wait for cloud-init,
 * then retry. The retrying wrapper is what `gvisorInstall` is handed in place of `apt-get`.
 */
const PRELUDE = `export DEBIAN_FRONTEND=noninteractive
command -v cloud-init >/dev/null 2>&1 && cloud-init status --wait >/dev/null 2>&1 || true
apt_retry() {
  n=0
  while [ "$n" -lt 30 ]; do
    if apt-get -o DPkg::Lock::Timeout=60 "$@"; then return 0; fi
    n=$((n + 1))
    echo "[kontra] apt busy (attempt $n/30), waiting 10s" >&2
    sleep 10
  done
  return 1
}`;

/**
 * The node's k3s config: its VPC address for the cluster, its public one for display, and flannel
 * on the interface that carries the VPC address.
 *
 * THE INTERFACE IS FOUND ON THE NODE, BY ADDRESS, because k3s's default is the interface with the
 * default route — the PUBLIC one on a droplet — and flannel's VXLAN over public addresses is traffic
 * the firewall (rightly) drops. Naming `eth1` would work on today's image and break silently on one
 * that names its NICs differently; asking which interface holds the address cannot.
 */
function nodeConfig(privateIp: string, publicIp: string): string {
  return `IFACE=$(ip -o -4 addr show | awk -v want='${privateIp}' '{ split($4, a, "/"); if (a[1] == want) { print $2; exit } }')
if [ -z "$IFACE" ]; then
  echo "no interface on this droplet carries its VPC address ${privateIp}" >&2
  exit 1
fi
install -d -m 0755 /etc/rancher/k3s
cat > /etc/rancher/k3s/config.yaml <<YAML
node-ip: ${privateIp}
node-external-ip: ${publicIp}
flannel-iface: $IFACE
YAML`;
}

/** The server's install, run over SSH. It carries no token: the server makes its own. */
export function serverScript(opts: { name: string; privateIp: string; publicIp: string }): string {
  const privateIp = ipv4('the server droplet\'s VPC address', opts.privateIp);
  const publicIp = ipv4('the server droplet\'s public address', opts.publicIp);
  const exec = `server --disable traefik --disable servicelb --write-kubeconfig-mode 0600 --tls-san ${publicIp} --node-name ${opts.name}`;
  return `set -eu
${PRELUDE}
${gvisorInstall('apt_retry')}
${nodeConfig(privateIp, publicIp)}
${k3sInstall(exec, '')}
if ! timeout 300s sh -c 'until test -s ${KUBECONFIG_FILE} && test -s ${SERVER_TOKEN_FILE}; do sleep 3; done'; then
  echo "k3s did not come up" >&2
  exit 1
fi
`;
}

/**
 * An agent's install, run over SSH. THE TOKEN IS NOT A PARAMETER: it arrives on stdin and goes
 * straight to a 0600 file before anything else runs, so no line of this script — which Pulumi and
 * the command provider may quote in an error — can ever contain it.
 */
export function agentScript(opts: { name: string; privateIp: string; publicIp: string; serverPrivateIp: string }): string {
  const privateIp = ipv4('the agent droplet\'s VPC address', opts.privateIp);
  const publicIp = ipv4('the agent droplet\'s public address', opts.publicIp);
  const server = ipv4('the server droplet\'s VPC address', opts.serverPrivateIp);
  return `set -eu
install -d -m 0755 /etc/rancher/k3s
(umask 077 && cat > ${JOIN_TOKEN_FILE})
if [ ! -s ${JOIN_TOKEN_FILE} ]; then
  echo "no join token arrived on stdin" >&2
  exit 1
fi
${PRELUDE}
${gvisorInstall('apt_retry')}
${nodeConfig(privateIp, publicIp)}
${k3sInstall(`agent --node-name ${opts.name}`, `K3S_URL='https://${server}:6443' K3S_TOKEN_FILE='${JOIN_TOKEN_FILE}'`)}
`;
}

/** The kubeconfig k3s wrote, pointed at the server's public address instead of its loopback. */
export function kubeconfigFor(raw: string, publicIp: string): string {
  const address = ipv4('the server droplet\'s public address', publicIp);
  const loopback = /server: https:\/\/127\.0\.0\.1:6443/;
  if (!loopback.test(raw)) {
    // Handing back a kubeconfig that still names 127.0.0.1 would make the bootstrap dial the
    // CONTROL PLANE's loopback, and fail there with an error about the wrong machine.
    throw new Error("the kubeconfig k3s wrote names no https://127.0.0.1:6443 server to point at the droplet's public address");
  }
  return raw.replace(loopback, `server: https://${address}:6443`);
}

// ── the program ───────────────────────────────────────────────────────────────────────────────────

/**
 * Everything the program is given. THERE IS NO TOKEN FIELD, and that is the type doing the work
 * `stacks.ts` describes for the legacy fleet: what reaches the program cannot carry the cloud token,
 * so no careless interpolation can put it in a resource input.
 */
export interface DoFleetArgs {
  fleet: string;
  region: string;
  /** The droplet slug, already chosen from the profile's size letters. */
  size: string;
  nodes: number;
  vpcCidr: string;
  /** The DigitalOcean key the droplets are created with; {@link privateKey} is its private half. */
  sshKeyFingerprint: string;
  /** Addresses the firewall admits 6443 and 22 from — {@link controlPlaneSources}. */
  controlPlane: string[];
  /** SECRET: the control plane's SSH key. Only ever an input of a command's (secret) `connection`. */
  privateKey: string;
}

/**
 * A profile, checked, as the program's arguments. Every refusal here happens before a stack is
 * selected, so a mistake costs nothing. `privateKey` is a thunk, read last: a profile that is wrong
 * says so without the key file having been opened.
 */
export function doFleetArgs(
  fleet: string,
  profile: DigitalOceanProfile,
  ctx: { controlPlane: string[]; privateKey: () => string }
): DoFleetArgs {
  if (!/^[a-z]([a-z0-9-]{0,30}[a-z0-9])?$/.test(fleet)) throw new Error(`fleet name ${JSON.stringify(fleet)} is not a profile name`);
  if (!Number.isInteger(profile.nodes) || profile.nodes < 1) throw new Error(`fleets.${fleet}: nodes must be at least 1`);
  checkVpcCidr(profile.vpc_cidr);
  const size = profile.sizes[sizeLetter(profile.sizes)]!;
  return {
    fleet,
    region: profile.region,
    size,
    nodes: profile.nodes,
    vpcCidr: profile.vpc_cidr,
    sshKeyFingerprint: profile.ssh_key_fingerprint,
    controlPlane: ctx.controlPlane,
    privateKey: ctx.privateKey(),
  };
}

const EVERYWHERE = ['0.0.0.0/0', '::/0'];

/** The firewall's inbound rules: see the header. Exported so the test reads the same list the program uses. */
export function inboundRules(args: Pick<DoFleetArgs, 'controlPlane' | 'vpcCidr'>): digitalocean.types.input.FirewallInboundRule[] {
  return [
    { protocol: 'tcp', portRange: '6443', sourceAddresses: [...args.controlPlane] },
    { protocol: 'tcp', portRange: '22', sourceAddresses: [...args.controlPlane] },
    { protocol: 'tcp', portRange: '1-65535', sourceAddresses: [args.vpcCidr] },
    { protocol: 'udp', portRange: '1-65535', sourceAddresses: [args.vpcCidr] },
    { protocol: 'icmp', sourceAddresses: [args.vpcCidr] },
  ];
}

const OUTBOUND: digitalocean.types.input.FirewallOutboundRule[] = [
  { protocol: 'tcp', portRange: '1-65535', destinationAddresses: EVERYWHERE },
  { protocol: 'udp', portRange: '1-65535', destinationAddresses: EVERYWHERE },
  { protocol: 'icmp', destinationAddresses: EVERYWHERE },
];

/** The fleet's resources. Returns its stack outputs: the kubeconfig (secret) and the node inventory. */
export function doFleetProgram(args: DoFleetArgs) {
  return async (): Promise<Record<string, unknown>> => {
    const { fleet } = args;

    const tag = new digitalocean.Tag(`${fleet}-tag`, { name: fleetTag(fleet) });
    const vpc = new digitalocean.Vpc(`${fleet}-vpc`, {
      name: `kontra-${fleet}`,
      region: args.region,
      ipRange: args.vpcCidr,
      description: `kontra fleet ${fleet}: made by kontra, destroyed with the fleet`,
    });
    // BOUND BY TAG, AND MADE BEFORE ANY DROPLET: every droplet depends on it, so no node of this fleet
    // exists before its firewall does, and k3s — installed by commands that come after the droplets —
    // never listens on a node that has none. (How quickly DigitalOcean applies a tag's firewall to a
    // new droplet is DigitalOcean's; this ordering is what kontra controls.)
    const firewall = new digitalocean.Firewall(`${fleet}-firewall`, {
      name: `kontra-${fleet}`,
      tags: [tag.name],
      inboundRules: inboundRules(args),
      outboundRules: OUTBOUND,
    });

    const droplets = Array.from({ length: args.nodes }, (_, i) => {
      const name = vmName(fleet, i);
      const droplet = new digitalocean.Droplet(
        name,
        {
          name,
          region: args.region,
          size: args.size,
          image: DROPLET_IMAGE,
          vpcUuid: vpc.id,
          sshKeys: [args.sshKeyFingerprint],
          tags: [SHARED_TAG, tag.name],
          // NO userData. See the header: the metadata service would serve it to every process on the node.
        },
        // An image slug moving, or a key rotated in the profile, must not REPLACE a live node — the
        // legacy fleet's measured lesson (programs/fleet.ts header), kept.
        { ignoreChanges: ['image', 'sshKeys'], dependsOn: [firewall] }
      );
      return { name, droplet };
    });

    const key = pulumi.secret(args.privateKey);
    const connection = (d: digitalocean.Droplet) => ({
      host: d.ipv4Address,
      user: 'root',
      privateKey: key,
      // SSH comes up somewhere in the minutes after the API calls the droplet active.
      dialErrorLimit: 60,
      perDialTimeout: 15,
    });

    const server = droplets[0]!;
    const serverInstall = new command.remote.Command(
      `${server.name}-k3s`,
      {
        connection: connection(server.droplet),
        create: pulumi
          .all([server.droplet.ipv4AddressPrivate, server.droplet.ipv4Address])
          .apply(([privateIp, publicIp]) => serverScript({ name: server.name, privateIp, publicIp })),
        // A replaced droplet is a new machine: install on it, rather than diff a script that did not change.
        triggers: [server.droplet.id],
      },
      { dependsOn: [firewall] }
    );

    /**
     * A file read back from the server, as a SECRET output. `logging: none` because the command
     * provider otherwise streams stdout into the engine's diagnostics — the kubeconfig and the join
     * token would be printed in the clear, in the activity's logs and any error that quotes them.
     * `addPreviousOutputInEnv: false` because the provider otherwise hands the previous stdout to the
     * next run as an environment variable.
     */
    const readBack = (name: string, file: string) =>
      new command.remote.Command(
        name,
        {
          connection: connection(server.droplet),
          create: `cat ${file}`,
          logging: 'none',
          addPreviousOutputInEnv: false,
          triggers: [serverInstall.id],
        },
        { dependsOn: [serverInstall], additionalSecretOutputs: ['stdout'] }
      );

    const joinToken = pulumi.secret(readBack(`${fleet}-join-token`, SERVER_TOKEN_FILE).stdout.apply((s) => s.trim()));
    const rawKubeconfig = readBack(`${fleet}-kubeconfig`, KUBECONFIG_FILE).stdout;

    for (const { name, droplet } of droplets.slice(1)) {
      new command.remote.Command(
        `${name}-k3s`,
        {
          connection: connection(droplet),
          create: pulumi
            .all([droplet.ipv4AddressPrivate, droplet.ipv4Address, server.droplet.ipv4AddressPrivate])
            .apply(([privateIp, publicIp, serverPrivateIp]) => agentScript({ name, privateIp, publicIp, serverPrivateIp })),
          // THE TOKEN, ON STDIN. See agentScript.
          stdin: joinToken,
          triggers: [droplet.id],
        },
        { dependsOn: [firewall, serverInstall] }
      );
    }

    const kubeconfig = pulumi.secret(
      pulumi.all([rawKubeconfig, server.droplet.ipv4Address]).apply(([raw, publicIp]) => kubeconfigFor(raw, publicIp))
    );

    // `priceHourly` is the droplet's own output, filled in by the provider from DigitalOcean's size
    // list. 0 when it is not reported: "unknown", never "free" (see FleetNode).
    const nodes = pulumi
      .all(droplets.map(({ droplet }) => pulumi.all([droplet.name, droplet.ipv4Address, droplet.priceHourly])))
      .apply((rows) =>
        rows.map(([name, address, price]): FleetNode => ({
          name,
          address,
          size: args.size,
          priceHourly: Number.isFinite(Number(price)) ? Number(price) : 0,
        }))
      );

    return { kubeconfig, nodes, server: server.droplet.ipv4Address, region: args.region, size: args.size };
  };
}

// ── the provider ──────────────────────────────────────────────────────────────────────────────────

/** The engine, narrowed to the two operations a provider runs. Injectable, so the wrapper is testable
 *  without the Pulumi CLI, a backend or a cloud. */
export interface DoStacks {
  up(
    ref: StackRef,
    program: () => Promise<Record<string, unknown>>,
    providerEnv: Record<string, string>,
    run?: ProviderRun
  ): Promise<OutputMap>;
  destroy(ref: StackRef, providerEnv: Record<string, string>, run?: ProviderRun): Promise<void>;
}

/** What one engine event says about progress — a resource URN and its operation, never a value. */
function progressOf(e: { resourcePreEvent?: { metadata?: { op?: string; urn?: string } }; summaryEvent?: { resourceChanges?: unknown } }) {
  const m = e.resourcePreEvent?.metadata;
  if (m) return { op: m.op, urn: m.urn };
  if (e.summaryEvent) return { changes: e.summaryEvent.resourceChanges };
  return undefined;
}

/**
 * The real engine, through `selectStack`.
 *
 * A RETRY CLEARS THE STACK'S LOCK FIRST, for the reason and under the condition `activities/infra.ts`
 * states: a killed `up` leaves a lock with no expiry, and every later operation then fails forever.
 * It is safe because the pool workflow (`kontra-pool/<profile>`) is this stack's only writer and runs
 * one converge at a time, and because the activity's keepalive means a heartbeat timeout is a dead
 * process, not a slow one.
 */
export const pulumiStacks: DoStacks = {
  async up(ref, program, providerEnv, run) {
    const stack = await selectStack(ref, program, providerEnv);
    if (run?.retrying) await stack.cancel().catch(() => undefined);
    const res = await stack.up({
      color: 'never',
      signal: run?.signal,
      onEvent: (e) => {
        const detail = progressOf(e);
        if (detail) run?.progress?.(detail);
      },
    });
    return res.outputs;
  },
  async destroy(ref, providerEnv, run) {
    // A destroy reads the state, not the program; the program here is never run.
    const stack = await selectStack(ref, async () => ({}), providerEnv);
    if (run?.retrying) await stack.cancel().catch(() => undefined);
    await stack.destroy({
      color: 'never',
      signal: run?.signal,
      onEvent: (e) => {
        const detail = progressOf(e);
        if (detail) run?.progress?.(detail);
      },
    });
  },
};

/**
 * An engine failure, as an error that may be logged and may land in workflow history.
 *
 * THE SECRETS ARE SCRUBBED EVEN THOUGH NOTHING ABOVE PUTS THEM IN AN ERROR. The engine's message
 * quotes provider diagnostics and command output this file does not author, and an activity's failure
 * message is written into history in the clear; a literal replace of two known values is cheap
 * against a token surviving a namespace's retention. The TAIL is kept, because that is where the
 * engine says what failed.
 */
function engineFailure(what: string, err: unknown, secrets: string[]): Error {
  let msg = err instanceof Error ? err.message : String(err);
  for (const s of secrets) if (s) msg = msg.split(s).join('[redacted]');
  if (msg.length > 4000) msg = `…${msg.slice(-4000)}`;
  return new Error(`${what}: ${msg}`);
}

function asDigitalOcean(profile: FleetProfile): DigitalOceanProfile {
  if (profile.provider !== 'digital_ocean') throw new Error(`the digital_ocean provider was handed a ${profile.provider} profile`);
  return profile;
}

/** Where the SSH key is: `KONTRA_SSH_KEY`, as compose sets it for the infra container. */
export function sshKeyPath(env: NodeJS.ProcessEnv = process.env): string {
  return env.KONTRA_SSH_KEY?.trim() || '/var/lib/kontra-ssh/fleet_key';
}

export class DigitalOceanProvider implements FleetProvider {
  private readonly stacks: DoStacks;
  private readonly env: () => NodeJS.ProcessEnv;
  private readonly readFile: (path: string) => string;

  constructor(
    opts: {
      stacks?: DoStacks;
      /** Read at each converge, never cached: see the registry. */
      env?: () => NodeJS.ProcessEnv;
      readFile?: (path: string) => string;
    } = {}
  ) {
    this.stacks = opts.stacks ?? pulumiStacks;
    this.env = opts.env ?? (() => process.env);
    this.readFile = opts.readFile ?? ((p) => readFileSync(p, 'utf8'));
  }

  private privateKey(env: NodeJS.ProcessEnv): string {
    const file = sshKeyPath(env);
    let key: string;
    try {
      key = this.readFile(file);
    } catch (err) {
      throw new Error(`cannot read the fleet SSH key at ${file} (set KONTRA_SSH_KEY): ${(err as Error).message}`);
    }
    // The public half mounted by mistake would fail only at the first SSH dial — after the droplets exist.
    if (!/-----BEGIN [A-Z ]*PRIVATE KEY-----/.test(key)) {
      throw new Error(`${file} is not a private key (no PRIVATE KEY header) — KONTRA_SSH_KEY must name the private half`);
    }
    return key;
  }

  async converge(fleet: string, profile: FleetProfile, run?: ProviderRun): Promise<ProvisionedCluster> {
    const p = asDigitalOcean(profile);
    const env = this.env();
    // EVERY REFUSAL BEFORE THE ENGINE: nothing below this block can be a droplet that bills.
    const args = doFleetArgs(fleet, p, { controlPlane: controlPlaneSources(env), privateKey: () => this.privateKey(env) });
    let outputs: OutputMap;
    try {
      outputs = await this.stacks.up({ project: DO_FLEET_PROJECT, stack: fleet }, doFleetProgram(args), { [DO_TOKEN_VAR]: p.token }, run);
    } catch (err) {
      throw engineFailure(`digital_ocean fleet ${fleet}: pulumi up failed`, err, [p.token, args.privateKey]);
    }
    const kubeconfig = outputs.kubeconfig?.value;
    if (typeof kubeconfig !== 'string' || kubeconfig === '') {
      throw new Error(`digital_ocean fleet ${fleet}: the converge finished without a kubeconfig output`);
    }
    const nodes = Array.isArray(outputs.nodes?.value) ? (outputs.nodes.value as FleetNode[]) : [];
    return { kubeconfig, flavor: 'k3s', nodes };
  }

  /** Destroys the stack. Needs the token and nothing else: no command has a delete step, so no SSH. */
  async destroy(fleet: string, profile: FleetProfile, run?: ProviderRun): Promise<void> {
    const p = asDigitalOcean(profile);
    try {
      await this.stacks.destroy({ project: DO_FLEET_PROJECT, stack: fleet }, { [DO_TOKEN_VAR]: p.token }, run);
    } catch (err) {
      throw engineFailure(`digital_ocean fleet ${fleet}: pulumi destroy failed`, err, [p.token]);
    }
  }
}
