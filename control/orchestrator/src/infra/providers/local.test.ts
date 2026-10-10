import { mkdtempSync, readFileSync, rmSync, statSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';

import { load as loadYaml } from 'js-yaml';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { parseFleets, type LocalProfile } from '../fleets';
import { GVISOR, K3S, LocalProvider, apiPort, limaConfig, vmName, type Runner } from './local';

const local = (nodes = 1): LocalProfile =>
  parseFleets(`fleets:\n  local:\n    provider: local\n    nodes: ${nodes}\n`).profiles.local as LocalProfile;

// eslint-disable-next-line @typescript-eslint/no-explicit-any
type Any = any;

describe('limaConfig', () => {
  const server: Any = loadYaml(limaConfig({ fleet: 'dev', index: 0, profile: local(3), token: 'tok', registry: { name: 'zot.kontra:5000', endpoint: 'http://host.lima.internal:5000' } }));
  const agent: Any = loadYaml(limaConfig({ fleet: 'dev', index: 2, profile: local(3), token: 'tok' }));
  const script = (doc: Any): string => doc.provision[0].script;

  it('sizes the VM from the profile, on the user-v2 network so nodes reach each other', () => {
    expect(server).toMatchObject({ cpus: 2, memory: '4GiB', networks: [{ lima: 'user-v2' }] });
    expect(server.containerd).toEqual({ system: false, user: false });
  });

  it('installs a PINNED k3s, verifying the installer by digest', () => {
    expect(script(server)).toContain(`INSTALL_K3S_VERSION='${K3S.version}'`);
    expect(script(server)).toContain(`${K3S.installerSha256}  /tmp/k3s-install.sh" | sha256sum -c -`);
  });

  it('installs a PINNED gVisor, verifying it by SHA-512, and registers runsc with containerd', () => {
    expect(script(agent)).toContain(GVISOR.sha512.x86_64);
    expect(script(agent)).toContain('sha512sum -c -');
    expect(script(agent)).toContain(`runtimes.runsc]`);
    expect(script(agent)).toContain('runtime_type = "io.containerd.runsc.v1"');
  });

  it('makes node 0 the server and the others agents joining it by name', () => {
    expect(script(server)).toMatch(/INSTALL_K3S_EXEC='server .*--tls-san lima-kontra-dev-0\.internal/);
    expect(script(agent)).toContain("K3S_URL='https://lima-kontra-dev-0.internal:6443'");
    expect(script(agent)).toMatch(/INSTALL_K3S_EXEC='agent --node-name kontra-dev-2'/);
  });

  it('forwards only the server\'s API port, to a port derived from the fleet', () => {
    expect(server.portForwards[0]).toEqual({ guestPort: 6443, hostPort: apiPort('dev'), hostIP: '127.0.0.1' });
    expect(server.portForwards.at(-1)).toEqual({ guestPortRange: [1, 65535], ignore: true });
    expect(agent.portForwards).toEqual([{ guestPortRange: [1, 65535], ignore: true }]);
    expect(apiPort('dev')).not.toBe(apiPort('ci'));
  });

  it('points the registry name at where a node reaches it', () => {
    expect(script(server)).toContain('"zot.kontra:5000":');
    expect(script(server)).toContain('endpoint: ["http://host.lima.internal:5000"]');
    expect(script(agent)).not.toContain('registries.yaml');
  });
});

describe('LocalProvider', () => {
  let state: string;
  beforeEach(() => {
    state = mkdtempSync(path.join(tmpdir(), 'kontra-local-'));
  });
  afterEach(() => rmSync(state, { recursive: true, force: true }));

  /** A fake limactl over a map of instances. */
  function lima(initial: Record<string, string> = {}) {
    const instances = new Map(Object.entries(initial));
    const calls: string[] = [];
    const run: Runner = async (cmd, args) => {
      calls.push(`${cmd} ${args.join(' ')}`);
      const [verb] = args;
      if (verb === 'list') {
        return { code: 0, stdout: [...instances].map(([name, status]) => JSON.stringify({ name, status })).join('\n'), stderr: '' };
      }
      if (verb === 'create') {
        instances.set(args.find((a) => a.startsWith('--name='))!.slice(7), 'Stopped');
      }
      if (verb === 'start') instances.set(args.at(-1)!, 'Running');
      if (verb === 'delete') instances.delete(args.at(-1)!);
      if (verb === 'shell') {
        return { code: 0, stdout: 'apiVersion: v1\nclusters:\n- cluster:\n    server: https://127.0.0.1:6443\n', stderr: '' };
      }
      return { code: 0, stdout: '', stderr: '' };
    };
    return { instances, calls, run };
  }

  it('creates and starts the server first, then the agents, and returns a kubeconfig on the forwarded port', async () => {
    const l = lima();
    const got = await new LocalProvider(l.run, state).converge('dev', local(3));
    const starts = l.calls.filter((c) => c.startsWith('limactl start')).map((c) => c.split(' ').at(-1));
    expect(starts).toEqual([vmName('dev', 0), vmName('dev', 1), vmName('dev', 2)]);
    expect(got.flavor).toBe('k3s');
    expect(got.kubeconfig).toContain(`server: https://127.0.0.1:${apiPort('dev')}`);
    expect(got.nodes.map((n) => n.address)).toEqual(['lima-kontra-dev-0.internal', 'lima-kontra-dev-1.internal', 'lima-kontra-dev-2.internal']);
  });

  it('is idempotent: running nodes are left alone', async () => {
    const l = lima({ 'kontra-dev-0': 'Running' });
    await new LocalProvider(l.run, state).converge('dev', local(1));
    expect(l.calls.filter((c) => /limactl (create|start)/.test(c))).toEqual([]);
  });

  it('restarts a stopped node rather than recreating it', async () => {
    const l = lima({ 'kontra-dev-0': 'Stopped' });
    await new LocalProvider(l.run, state).converge('dev', local(1));
    expect(l.calls.filter((c) => /limactl (create|start)/.test(c))).toEqual(['limactl start --tty=false kontra-dev-0']);
  });

  it('shrinks by deleting the highest agents, never the server', async () => {
    const l = lima({ 'kontra-dev-0': 'Running', 'kontra-dev-1': 'Running', 'kontra-dev-2': 'Running' });
    await new LocalProvider(l.run, state).converge('dev', local(1));
    expect([...l.instances.keys()]).toEqual(['kontra-dev-0']);
  });

  it('keeps one join token per fleet, private to the host', async () => {
    const l = lima();
    const p = new LocalProvider(l.run, state);
    await p.converge('dev', local(1));
    const file = path.join(state, 'dev', 'token');
    const first = readFileSync(file, 'utf8');
    expect(statSync(file).mode & 0o777).toBe(0o600);
    await p.destroy('dev');
    await p.converge('dev', local(1));
    expect(readFileSync(file, 'utf8')).toBe(first);
  });

  it('destroys only its own fleet\'s VMs — not a fleet whose name it is a prefix of', async () => {
    const l = lima({ 'kontra-dev-0': 'Running', 'kontra-other-0': 'Running', 'kontra-dev-x-0': 'Running' });
    await new LocalProvider(l.run, state).destroy('dev');
    expect([...l.instances.keys()].sort()).toEqual(['kontra-dev-x-0', 'kontra-other-0']);
  });

  it('reports a failed limactl with its exit code and the tail of its stderr', async () => {
    const run: Runner = async (_c, args) =>
      args[0] === 'list' ? { code: 0, stdout: '', stderr: '' } : { code: 1, stdout: '', stderr: 'qemu: KVM not available' };
    await expect(new LocalProvider(run, state).converge('dev', local(1))).rejects.toThrow(/limactl create: exit 1: qemu: KVM not available/);
  });
});
