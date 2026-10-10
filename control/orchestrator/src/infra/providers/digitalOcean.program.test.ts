/**
 * THE `digital_ocean` FLEET'S RESOURCE GRAPH, UNDER PULUMI MOCKS — what the engine would be asked to
 * create, with nothing created, nothing dialled and nothing billed (PRD §13: no cloud spend).
 *
 * ═══ THE WHOLE PROVIDER RUNS, NOT ONLY THE PROGRAM ═══
 *
 * The secret properties are about the path from `kontra.yaml` to a resource input, so the test drives
 * `DigitalOceanProvider.converge` with a real profile and swaps only the engine: the stand-in below
 * runs the program the provider built under `pulumi.runtime.setMocks` and returns its outputs the way
 * `stack.up()` does. Every resource the program registers is captured with its inputs, and that
 * capture is the haystack.
 *
 * ═══ THE CONTROL FOR THE SWEEP ═══
 *
 * "The token is in no input" passes vacuously if the sweep cannot see into a SECRET input — those
 * reach a mock wrapped in Pulumi's sentinel object, not as the value. So the join token, which the
 * program DOES put in an input (the agents' stdin, as a secret), is swept for with the same function
 * and must be found exactly there. Same sweep, same wrapping, opposite answer.
 *
 * The settle loop is `packing.test.ts`'s, for its reason: resources that `dependsOn` another register
 * on a later turn of the event loop than the program's own promise.
 */
import * as pulumi from '@pulumi/pulumi';
import type { OutputMap } from '@pulumi/pulumi/automation';
import { beforeAll, describe, expect, it } from 'vitest';

import { parseFleets, type DigitalOceanProfile } from '../fleets';
import {
  CONTROL_PLANE_VAR,
  DO_FLEET_PROJECT,
  DO_TOKEN_VAR,
  DROPLET_IMAGE,
  DigitalOceanProvider,
  SHARED_TAG,
  fleetTag,
  type DoStacks,
} from './digitalOcean';
import type { ProvisionedCluster } from './registry';

const TOKEN = 'dop_v1_PROGRAM_SENTINEL_not_in_any_resource_input_51a7';
const JOIN = 'K10fffff::server:JOIN_SENTINEL_only_on_agent_stdin_0c3e';
const KEY = '-----BEGIN OPENSSH PRIVATE KEY-----\nKEY_SENTINEL_only_in_connections\n-----END OPENSSH PRIVATE KEY-----\n';
const CONTROL_PLANE = ['203.0.113.7', '2001:db8::7'];
const RAW_KUBECONFIG = 'apiVersion: v1\nclusters:\n- cluster:\n    certificate-authority-data: Zm9v\n    server: https://127.0.0.1:6443\n  name: default\n';

/** One resource the program asked the engine for. */
interface Seen {
  type: string;
  name: string;
  inputs: Record<string, unknown>;
}
const captured: Seen[] = [];

/** Pulumi's secret sentinel: a secret input reaches a mock as `{ <SIG>: <SECRET_SIG>, value }`. */
const SIG = '4dabf18193072939515e22adb298388d';
const isWrappedSecret = (v: unknown): boolean => !!v && typeof v === 'object' && SIG in (v as Record<string, unknown>);
const unsecret = (v: unknown): unknown => (isWrappedSecret(v) ? (v as { value?: unknown }).value : v);

/** Every string in a value, secret-wrapped or not, as one haystack. */
const haystack = (v: unknown): string => JSON.stringify(v) ?? '';

const DROPLET = 'digitalocean:index/droplet:Droplet';
const COMMAND = 'command:remote:Command';

beforeAll(() => {
  pulumi.runtime.setMocks(
    {
      newResource: (args: pulumi.runtime.MockResourceArgs) => {
        captured.push({ type: args.type, name: args.name, inputs: args.inputs });
        const state: Record<string, unknown> = { ...args.inputs };
        if (args.type === DROPLET) {
          // Node i: public 203.0.113.(10+i), VPC 10.200.0.(2+i) — distinct, so a mix-up shows.
          const i = Number(args.name.split('-').at(-1));
          Object.assign(state, { ipv4Address: `203.0.113.${10 + i}`, ipv4AddressPrivate: `10.200.0.${2 + i}`, priceHourly: 0.07143 });
        }
        if (args.type === COMMAND && args.name.endsWith('-join-token')) state.stdout = `${JOIN}\n`;
        if (args.type === COMMAND && args.name.endsWith('-kubeconfig')) state.stdout = RAW_KUBECONFIG;
        return { id: `${args.name}-id`, state };
      },
      call: (args: pulumi.runtime.MockCallArgs) => args.inputs,
    },
    DO_FLEET_PROJECT,
    'mocks',
    false
  );
});

async function resolve(v: unknown): Promise<{ value: unknown; secret: boolean }> {
  if (!pulumi.Output.isInstance(v)) return { value: v, secret: false };
  const o = v as pulumi.Output<unknown>;
  const value = await new Promise((r) => o.apply((x) => (r(x), x)));
  return { value, secret: await pulumi.isSecret(o) };
}

const profile = (nodes: number): DigitalOceanProfile =>
  parseFleets(
    `fleets:\n  default: do\n  do:\n    provider: digital_ocean\n    region: fra1\n    token: ${TOKEN}\n` +
      `    ssh_key_fingerprint: "aa:bb:cc"\n    vpc_cidr: 10.200.0.0/20\n` +
      `    sizes: { s: s-2vcpu-4gb, m: s-4vcpu-8gb, l: s-8vcpu-16gb }\n    nodes: ${nodes}\n`
  ).profiles.do as DigitalOceanProfile;

interface Converged {
  seen: Seen[];
  cluster: ProvisionedCluster;
  outputs: OutputMap;
  providerEnv: Record<string, string>;
}

/**
 * The real provider, with an engine that runs its program under the mocks. EACH SCENARIO GETS ITS OWN
 * FLEET NAME, so resource names cannot collide between scenarios inside one Pulumi runtime.
 */
async function converge(fleet: string, nodes: number): Promise<Converged> {
  const before = captured.length;
  let providerEnv: Record<string, string> = {};
  let outputs: OutputMap = {};
  const stacks: DoStacks = {
    async up(_ref, program, env) {
      providerEnv = env;
      const raw = await program();
      for (let quiet = 0, seen = -1; quiet < 5; quiet += captured.length === seen ? 1 : 0) {
        seen = captured.length;
        await new Promise((r) => setTimeout(r, 25));
      }
      for (const [k, v] of Object.entries(raw)) outputs[k] = await resolve(v);
      return outputs;
    },
    destroy: async () => undefined,
  };
  const provider = new DigitalOceanProvider({
    stacks,
    env: () => ({ [CONTROL_PLANE_VAR]: CONTROL_PLANE.join(','), KONTRA_SSH_KEY: '/keys/fleet' }),
    readFile: () => KEY,
  });
  const cluster = await provider.converge(fleet, profile(nodes));
  return { seen: captured.slice(before), cluster, outputs, providerEnv };
}

const ofType = (seen: Seen[], type: string) => seen.filter((r) => r.type === type);

describe('a three-node digital_ocean fleet', () => {
  let c: Converged;
  beforeAll(async () => {
    c = await converge('three', 3);
  });

  it('makes one VPC, on the profile\'s range, in its region', () => {
    const vpcs = ofType(c.seen, 'digitalocean:index/vpc:Vpc');
    expect(vpcs).toHaveLength(1);
    expect(vpcs[0]!.inputs).toMatchObject({ name: 'kontra-three', region: 'fra1', ipRange: '10.200.0.0/20' });
  });

  it('makes one droplet per node, of the m size, in that VPC, tagged, with NO user_data', () => {
    const droplets = ofType(c.seen, DROPLET);
    expect(droplets.map((d) => d.name)).toEqual(['kontra-three-0', 'kontra-three-1', 'kontra-three-2']);
    for (const d of droplets) {
      expect(d.inputs).toMatchObject({
        name: d.name,
        region: 'fra1',
        size: 's-4vcpu-8gb',
        image: DROPLET_IMAGE,
        vpcUuid: 'three-vpc-id',
        sshKeys: ['aa:bb:cc'],
        tags: [SHARED_TAG, fleetTag('three')],
      });
      expect(d.inputs.userData).toBeUndefined();
    }
  });

  it('admits 6443 and 22 from the control plane ONLY, the fleet\'s own VPC for node-to-node, and nothing else', () => {
    const firewalls = ofType(c.seen, 'digitalocean:index/firewall:Firewall');
    expect(firewalls).toHaveLength(1);
    const fw = firewalls[0]!.inputs as {
      tags: string[];
      inboundRules: Array<{ protocol: string; portRange?: string; sourceAddresses: string[] }>;
      outboundRules: Array<{ protocol: string; destinationAddresses: string[] }>;
    };
    expect(fw.tags).toEqual([fleetTag('three')]);
    expect(fw.inboundRules).toEqual([
      { protocol: 'tcp', portRange: '6443', sourceAddresses: CONTROL_PLANE },
      { protocol: 'tcp', portRange: '22', sourceAddresses: CONTROL_PLANE },
      { protocol: 'tcp', portRange: '1-65535', sourceAddresses: ['10.200.0.0/20'] },
      { protocol: 'udp', portRange: '1-65535', sourceAddresses: ['10.200.0.0/20'] },
      { protocol: 'icmp', sourceAddresses: ['10.200.0.0/20'] },
    ]);
    // Said the other way round, so a rule added later is checked too: from outside the VPC, only
    // the control plane, and only on 6443 and 22.
    for (const rule of fw.inboundRules) {
      expect(rule.sourceAddresses).not.toContain('0.0.0.0/0');
      expect(rule.sourceAddresses).not.toContain('::/0');
      if (rule.sourceAddresses.some((s) => CONTROL_PLANE.includes(s))) {
        expect(rule.sourceAddresses).toEqual(CONTROL_PLANE);
        expect(['6443', '22']).toContain(rule.portRange);
      } else {
        expect(rule.sourceAddresses).toEqual(['10.200.0.0/20']);
      }
    }
    expect(fw.outboundRules.every((r) => r.destinationAddresses.includes('0.0.0.0/0'))).toBe(true);
  });

  it('installs the server on node 0 and joins the others to its VPC address', () => {
    const installs = ofType(c.seen, COMMAND).filter((r) => r.name.endsWith('-k3s'));
    expect(installs.map((r) => r.name).sort()).toEqual(['kontra-three-0-k3s', 'kontra-three-1-k3s', 'kontra-three-2-k3s']);
    const create = (name: string) => String(installs.find((r) => r.name === name)!.inputs.create);
    expect(create('kontra-three-0-k3s')).toMatch(/INSTALL_K3S_EXEC='server .*--tls-san 203\.0\.113\.10 --node-name kontra-three-0'/);
    expect(create('kontra-three-2-k3s')).toContain("K3S_URL='https://10.200.0.2:6443'");
    expect(create('kontra-three-2-k3s')).toContain('node-ip: 10.200.0.4');
    // Every SSH command goes to the droplet it is for, as root, with the key — secret-wrapped.
    for (const r of ofType(c.seen, COMMAND)) {
      expect(isWrappedSecret(r.inputs.connection), r.name).toBe(true);
      expect(unsecret(r.inputs.connection)).toMatchObject({ user: 'root', privateKey: KEY });
    }
    expect((unsecret(installs.find((r) => r.name === 'kontra-three-1-k3s')!.inputs.connection) as { host: string }).host).toBe('203.0.113.11');
  });

  it('reads the join token and the kubeconfig back with logging OFF', () => {
    const reads = ofType(c.seen, COMMAND).filter((r) => !r.name.endsWith('-k3s'));
    expect(reads.map((r) => [r.name, r.inputs.create]).sort()).toEqual([
      ['three-join-token', 'cat /var/lib/rancher/k3s/server/node-token'],
      ['three-kubeconfig', 'cat /etc/rancher/k3s/k3s.yaml'],
    ]);
    for (const r of reads) expect(r.inputs).toMatchObject({ logging: 'none', addPreviousOutputInEnv: false });
  });

  it('delivers the join token ONLY on the agents\' stdin, as a secret — never in user_data or a script', () => {
    const holding = c.seen.filter((r) => haystack(r.inputs).includes(JOIN));
    // THE CONTROL: the sweep finds the token where it is, through the secret wrapper.
    expect(holding.map((r) => r.name).sort()).toEqual(['kontra-three-1-k3s', 'kontra-three-2-k3s']);
    for (const r of holding) {
      expect(isWrappedSecret(r.inputs.stdin)).toBe(true);
      expect(unsecret(r.inputs.stdin)).toBe(JOIN);
      expect(String(r.inputs.create)).not.toContain(JOIN);
    }
  });

  it("puts the profile's token in NO resource input — not even a provider's — and hands it to the engine env alone", () => {
    expect(c.seen.length).toBeGreaterThan(8);
    expect(c.seen.filter((r) => haystack(r.inputs).includes(TOKEN)).map((r) => r.name)).toEqual([]);
    expect(c.seen.filter((r) => r.type.startsWith('pulumi:providers:'))).toEqual([]);
    expect(haystack(c.outputs)).not.toContain(TOKEN);
    expect(c.providerEnv).toEqual({ [DO_TOKEN_VAR]: TOKEN });
  });

  it('keeps the SSH key inside the secret connections and nowhere else', () => {
    for (const r of c.seen) {
      const { connection, ...rest } = r.inputs;
      expect(haystack(rest), r.name).not.toContain('KEY_SENTINEL');
      if (haystack(connection).includes('KEY_SENTINEL')) expect(isWrappedSecret(connection)).toBe(true);
    }
  });

  it('returns the kubeconfig as a SECRET output, pointed at the server\'s public address', () => {
    expect(c.outputs.kubeconfig!.secret).toBe(true);
    expect(c.cluster.kubeconfig).toContain('server: https://203.0.113.10:6443');
    expect(c.cluster.kubeconfig).not.toContain('127.0.0.1');
    expect(c.cluster.flavor).toBe('k3s');
  });

  it('returns the nodes with their size and hourly price', () => {
    expect(c.cluster.nodes).toEqual([
      { name: 'kontra-three-0', address: '203.0.113.10', size: 's-4vcpu-8gb', priceHourly: 0.07143 },
      { name: 'kontra-three-1', address: '203.0.113.11', size: 's-4vcpu-8gb', priceHourly: 0.07143 },
      { name: 'kontra-three-2', address: '203.0.113.12', size: 's-4vcpu-8gb', priceHourly: 0.07143 },
    ]);
  });
});

describe('a one-node digital_ocean fleet', () => {
  it('is a server alone: one droplet, no agent, no token on any stdin', async () => {
    const c = await converge('solo', 1);
    expect(ofType(c.seen, DROPLET).map((d) => d.name)).toEqual(['kontra-solo-0']);
    expect(ofType(c.seen, COMMAND).map((r) => r.name).sort()).toEqual(['kontra-solo-0-k3s', 'solo-join-token', 'solo-kubeconfig']);
    expect(c.seen.filter((r) => haystack(r.inputs).includes(JOIN))).toEqual([]);
    expect(c.cluster.nodes).toHaveLength(1);
  });
});
