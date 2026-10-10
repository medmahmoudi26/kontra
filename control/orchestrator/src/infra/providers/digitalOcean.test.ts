/**
 * THE `digital_ocean` PROVIDER'S PURE PARTS AND ITS WRAPPER — with no engine, no cloud and no spend.
 *
 * The resource graph is `digitalOcean.program.test.ts`'s (Pulumi mocks). This file pins what runs on
 * a node (the scripts), what is refused before anything is created, and what the provider hands the
 * engine: the token in `envVars` and nowhere else, and a failure with no secret in it.
 */
import { describe, expect, it } from 'vitest';

import { parseFleets, type DigitalOceanProfile } from '../fleets';
import {
  CONTROL_PLANE_VAR,
  DO_FLEET_PROJECT,
  DO_TOKEN_VAR,
  DigitalOceanProvider,
  JOIN_TOKEN_FILE,
  agentScript,
  checkVpcCidr,
  controlPlaneSources,
  doFleetArgs,
  kubeconfigFor,
  serverScript,
  sizeLetter,
  type DoStacks,
} from './digitalOcean';
import { GVISOR, K3S } from './local';
import type { ProviderRun } from './registry';

const TOKEN = 'dop_v1_TEST_SENTINEL_never_leaves_the_provider_9f2c';
const KEY = '-----BEGIN OPENSSH PRIVATE KEY-----\nnot-a-real-key\n-----END OPENSSH PRIVATE KEY-----\n';

const profile = (extra = ''): DigitalOceanProfile =>
  parseFleets(
    `fleets:\n  default: do\n  do:\n    provider: digital_ocean\n    region: fra1\n    token: ${TOKEN}\n` +
      `    ssh_key_fingerprint: "aa:bb:cc"\n    sizes: { s: s-2vcpu-4gb, m: s-4vcpu-8gb, l: s-8vcpu-16gb }\n${extra}`
  ).profiles.do as DigitalOceanProfile;

describe('the size a fleet uses', () => {
  it("is 'm' when the profile has it", () => {
    expect(sizeLetter({ s: 'a', m: 'b', l: 'c' })).toBe('m');
  });
  it('is the first one listed otherwise', () => {
    expect(sizeLetter({ l: 'c', s: 'a' })).toBe('l');
  });
});

describe('the VPC range', () => {
  it('accepts the example range', () => {
    expect(() => checkVpcCidr('10.200.0.0/20')).not.toThrow();
    expect(() => checkVpcCidr('192.168.10.0/24')).not.toThrow();
  });
  it('refuses a public range, a malformed one, and sizes DigitalOcean refuses', () => {
    expect(() => checkVpcCidr('203.0.113.0/24')).toThrow(/RFC 1918/);
    expect(() => checkVpcCidr('10.200.0.0')).toThrow(/not an IPv4 CIDR/);
    expect(() => checkVpcCidr('10.0.0.0/8')).toThrow(/between a \/16 and a \/24/);
    expect(() => checkVpcCidr('10.200.0.0/28')).toThrow(/between a \/16 and a \/24/);
  });
  it("refuses a range overlapping k3s's pod or service network", () => {
    expect(() => checkVpcCidr('10.42.16.0/20')).toThrow(/10\.42\.0\.0\/16/);
    expect(() => checkVpcCidr('10.43.0.0/16')).toThrow(/10\.43\.0\.0\/16/);
    expect(() => checkVpcCidr('10.44.0.0/16')).not.toThrow();
  });
});

describe('where the firewall admits the API and SSH from', () => {
  it('refuses to guess: unset names the variable', () => {
    expect(() => controlPlaneSources({})).toThrow(new RegExp(CONTROL_PLANE_VAR));
    expect(() => controlPlaneSources({ [CONTROL_PLANE_VAR]: ' , ' })).toThrow(/names no address/);
  });
  it('takes addresses and narrow ranges, IPv4 and IPv6', () => {
    expect(controlPlaneSources({ [CONTROL_PLANE_VAR]: ' 203.0.113.7, 198.51.100.0/24 ,2001:db8::1, 2001:db8:1::/64' })).toEqual([
      '203.0.113.7',
      '198.51.100.0/24',
      '2001:db8::1',
      '2001:db8:1::/64',
    ]);
  });
  it('refuses the world, and anything wide enough to be a neighbourhood', () => {
    for (const bad of ['0.0.0.0/0', '::/0', '203.0.0.0/16', '2001:db8::/32', 'example.com', '203.0.113.7/33', '1.2.3.4/24/1']) {
      expect(() => controlPlaneSources({ [CONTROL_PLANE_VAR]: bad }), bad).toThrow(new RegExp(CONTROL_PLANE_VAR));
    }
  });
});

describe('the server install', () => {
  const script = serverScript({ name: 'kontra-do-0', privateIp: '10.200.0.2', publicIp: '203.0.113.10' });

  it('installs the SAME pinned k3s and gVisor as the local provider', () => {
    expect(script).toContain(`INSTALL_K3S_VERSION='${K3S.version}'`);
    expect(script).toContain(`${K3S.installerSha256}  /tmp/k3s-install.sh" | sha256sum -c -`);
    expect(script).toContain(GVISOR.sha512.x86_64);
    expect(script).toContain('sha512sum -c -');
    expect(script).toContain('runtime_type = "io.containerd.runsc.v1"');
  });

  it("waits for cloud-init and retries apt, the two things a fresh droplet was measured to need", () => {
    expect(script).toContain('cloud-init status --wait');
    expect(script).toContain('apt_retry update -q && apt_retry install -y -q zstd');
    expect(script).not.toMatch(/^\s*apt-get update/m);
  });

  it("names the public address in the API certificate, and runs flannel on the VPC interface", () => {
    expect(script).toMatch(/INSTALL_K3S_EXEC='server .*--tls-san 203\.0\.113\.10 --node-name kontra-do-0'/);
    expect(script).toContain('node-ip: 10.200.0.2');
    expect(script).toContain("awk -v want='10.200.0.2'");
    expect(script).toContain('flannel-iface: $IFACE');
  });

  it('carries no token: the server makes its own', () => {
    expect(script).not.toMatch(/K3S_TOKEN/);
  });

  it('refuses an address that is not one, rather than write it into a shell script', () => {
    expect(() => serverScript({ name: 'n', privateIp: "10.0.0.1'; reboot; '", publicIp: '203.0.113.10' })).toThrow(/not an IPv4 address/);
  });
});

describe('an agent install', () => {
  const script = agentScript({ name: 'kontra-do-2', privateIp: '10.200.0.4', publicIp: '203.0.113.12', serverPrivateIp: '10.200.0.2' });

  it('joins the server over the VPC, reading the token from a file', () => {
    expect(script).toContain(`K3S_URL='https://10.200.0.2:6443' K3S_TOKEN_FILE='${JOIN_TOKEN_FILE}'`);
    expect(script).toMatch(/INSTALL_K3S_EXEC='agent --node-name kontra-do-2'/);
    expect(script).not.toMatch(/K3S_TOKEN=/);
  });

  it('writes the token from stdin to a 0600 file BEFORE anything else runs', () => {
    const lines = script.split('\n');
    expect(lines[0]).toBe('set -eu');
    expect(lines.findIndex((l) => l === `(umask 077 && cat > ${JOIN_TOKEN_FILE})`)).toBe(2);
    expect(script.indexOf(JOIN_TOKEN_FILE)).toBeLessThan(script.indexOf('cloud-init'));
  });
});

describe('the kubeconfig handed back', () => {
  const raw = 'apiVersion: v1\nclusters:\n- cluster:\n    certificate-authority-data: Zm9v\n    server: https://127.0.0.1:6443\n';
  it("points at the server's public address", () => {
    expect(kubeconfigFor(raw, '203.0.113.10')).toContain('server: https://203.0.113.10:6443');
    expect(kubeconfigFor(raw, '203.0.113.10')).not.toContain('127.0.0.1');
  });
  it('refuses one that does not name the loopback server, rather than return it unchanged', () => {
    expect(() => kubeconfigFor('apiVersion: v1\n', '203.0.113.10')).toThrow(/127\.0\.0\.1/);
  });
});

describe('the program arguments', () => {
  it('carry the chosen slug, the VPC range and the firewall sources — and have no room for the token', () => {
    const args = doFleetArgs('do', profile('    nodes: 3\n'), { controlPlane: ['203.0.113.7'], privateKey: () => KEY });
    expect(args).toMatchObject({ fleet: 'do', region: 'fra1', size: 's-4vcpu-8gb', nodes: 3, vpcCidr: '10.200.0.0/20' });
    expect(JSON.stringify(args)).not.toContain(TOKEN);
    expect(Object.keys(args)).not.toContain('token');
  });

  it('checks the profile before the key file is opened', () => {
    let opened = false;
    expect(() =>
      doFleetArgs('do', profile('    vpc_cidr: 10.42.0.0/20\n'), {
        controlPlane: ['203.0.113.7'],
        privateKey: () => {
          opened = true;
          return KEY;
        },
      })
    ).toThrow(/10\.42/);
    expect(opened).toBe(false);
  });
});

describe('DigitalOceanProvider', () => {
  const ENV = { [CONTROL_PLANE_VAR]: '203.0.113.7', KONTRA_SSH_KEY: '/keys/fleet' };
  const KUBECONFIG = 'apiVersion: v1\nclusters:\n- cluster:\n    server: https://203.0.113.10:6443\n';

  /** An engine that records what it was handed and answers like a converge that worked. */
  function engine(fail?: Error) {
    const calls: Array<{ op: string; ref: unknown; providerEnv: Record<string, string>; run?: ProviderRun; program?: unknown }> = [];
    const stacks: DoStacks = {
      async up(ref, program, providerEnv, run) {
        calls.push({ op: 'up', ref, providerEnv, run, program });
        if (fail) throw fail;
        return {
          kubeconfig: { value: KUBECONFIG, secret: true },
          nodes: { value: [{ name: 'kontra-do-0', address: '203.0.113.10', size: 's-4vcpu-8gb', priceHourly: 0.07143 }], secret: false },
        };
      },
      async destroy(ref, providerEnv, run) {
        calls.push({ op: 'destroy', ref, providerEnv, run });
        if (fail) throw fail;
      },
    };
    return { calls, stacks };
  }

  const reads: string[] = [];
  const files = (content: Record<string, string>) => (p: string) => {
    reads.push(p);
    if (!(p in content)) throw Object.assign(new Error(`ENOENT: no such file, open '${p}'`), { code: 'ENOENT' });
    return content[p]!;
  };

  it('converges one stack per profile, with the token in the engine env and nowhere else', async () => {
    const e = engine();
    const run: ProviderRun = { retrying: true };
    const got = await new DigitalOceanProvider({ stacks: e.stacks, env: () => ENV, readFile: files({ '/keys/fleet': KEY }) }).converge('do', profile(), run);
    expect(got).toEqual({
      kubeconfig: KUBECONFIG,
      flavor: 'k3s',
      nodes: [{ name: 'kontra-do-0', address: '203.0.113.10', size: 's-4vcpu-8gb', priceHourly: 0.07143 }],
    });
    expect(e.calls).toHaveLength(1);
    expect(e.calls[0]!.ref).toEqual({ project: DO_FLEET_PROJECT, stack: 'do' });
    expect(e.calls[0]!.providerEnv).toEqual({ [DO_TOKEN_VAR]: TOKEN });
    expect(e.calls[0]!.run).toBe(run);
  });

  it('refuses before the engine when the firewall has no source', async () => {
    const e = engine();
    const p = new DigitalOceanProvider({ stacks: e.stacks, env: () => ({ KONTRA_SSH_KEY: '/keys/fleet' }), readFile: files({ '/keys/fleet': KEY }) });
    await expect(p.converge('do', profile())).rejects.toThrow(new RegExp(CONTROL_PLANE_VAR));
    expect(e.calls).toEqual([]);
  });

  it('refuses before the engine when the SSH key is missing — not once droplets are billing', async () => {
    const e = engine();
    const p = new DigitalOceanProvider({ stacks: e.stacks, env: () => ENV, readFile: files({}) });
    await expect(p.converge('do', profile())).rejects.toThrow(/cannot read the fleet SSH key at \/keys\/fleet/);
    expect(e.calls).toEqual([]);
  });

  it('refuses a public key mounted where the private one belongs', async () => {
    const e = engine();
    const p = new DigitalOceanProvider({ stacks: e.stacks, env: () => ENV, readFile: files({ '/keys/fleet': 'ssh-ed25519 AAAA… ops@host\n' }) });
    await expect(p.converge('do', profile())).rejects.toThrow(/not a private key/);
    expect(e.calls).toEqual([]);
  });

  it('scrubs the token and the key out of an engine failure, keeping what failed', async () => {
    const e = engine(new Error(`stderr: provider config token=${TOKEN} rejected\n${KEY}\nerror: 401 Unable to authenticate you`));
    const p = new DigitalOceanProvider({ stacks: e.stacks, env: () => ENV, readFile: files({ '/keys/fleet': KEY }) });
    const err = (await p.converge('do', profile()).catch((x: Error) => x)) as Error;
    expect(err.message).toMatch(/^digital_ocean fleet do: pulumi up failed: /);
    expect(err.message).toContain('401 Unable to authenticate you');
    expect(err.message).not.toContain(TOKEN);
    expect(err.message).not.toContain('not-a-real-key');
  });

  it('refuses a converge whose outputs carry no kubeconfig', async () => {
    const stacks: DoStacks = { up: async () => ({}), destroy: async () => undefined };
    const p = new DigitalOceanProvider({ stacks, env: () => ENV, readFile: files({ '/keys/fleet': KEY }) });
    await expect(p.converge('do', profile())).rejects.toThrow(/without a kubeconfig/);
  });

  it('destroys the stack with the token alone: no SSH key is read', async () => {
    const e = engine();
    reads.length = 0;
    await new DigitalOceanProvider({ stacks: e.stacks, env: () => ENV, readFile: files({}) }).destroy('do', profile());
    expect(e.calls.map((c) => [c.op, c.ref, c.providerEnv])).toEqual([['destroy', { project: DO_FLEET_PROJECT, stack: 'do' }, { [DO_TOKEN_VAR]: TOKEN }]]);
    expect(reads).toEqual([]);
  });

  it('scrubs the token out of a destroy failure', async () => {
    const e = engine(new Error(`token ${TOKEN} is not valid`));
    const err = (await new DigitalOceanProvider({ stacks: e.stacks, env: () => ENV }).destroy('do', profile()).catch((x: Error) => x)) as Error;
    expect(err.message).toMatch(/pulumi destroy failed: token \[redacted\] is not valid/);
  });

  it('refuses a profile for another provider', async () => {
    const local = parseFleets('fleets:\n  local:\n    provider: local\n').profiles.local!;
    await expect(new DigitalOceanProvider({ stacks: engine().stacks }).converge('local', local)).rejects.toThrow(/handed a local profile/);
  });
});
