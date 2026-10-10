import { afterEach, describe, expect, it } from 'vitest';

import { parseFleets } from '../fleets';
import { CONTROL_PLANE_VAR, DigitalOceanProvider } from './digitalOcean';
import { PROVIDERS, providerFor } from './registry';

describe('fleet providers', () => {
  it('has one provider per provider name a profile may use', () => {
    expect(Object.keys(PROVIDERS).sort()).toEqual(['byo_kubeconfig', 'digital_ocean', 'local']);
  });

  it('adopts a bring-your-own cluster and never destroys it', async () => {
    const fleets = parseFleets('fleets:\n  default: eks\n  eks:\n    provider: byo_kubeconfig\n    kubeconfig: "apiVersion: v1"\n');
    const p = fleets.profiles.eks!;
    const got = await providerFor(p).converge('eks', p);
    expect(got).toEqual({ kubeconfig: 'apiVersion: v1', flavor: 'byo', nodes: [] });
    await expect(providerFor(p).destroy('eks', p)).resolves.toBeUndefined();
  });

  describe('digital_ocean', () => {
    const saved = { cp: process.env[CONTROL_PLANE_VAR], key: process.env.KONTRA_SSH_KEY };
    afterEach(() => {
      for (const [k, v] of [[CONTROL_PLANE_VAR, saved.cp], ['KONTRA_SSH_KEY', saved.key]] as const) {
        if (v === undefined) delete process.env[k];
        else process.env[k] = v;
      }
    });
    const d = parseFleets('fleets:\n  default: d\n  d:\n    provider: digital_ocean\n    region: fra1\n    token: t\n    ssh_key_fingerprint: f\n    sizes: { m: s-1vcpu-1gb }\n').profiles.d!;

    it('is the droplet provider', () => {
      expect(providerFor(d)).toBeInstanceOf(DigitalOceanProvider);
    });

    // WIRED, AND FAIL-CLOSED: the registry's instance reads the install's environment at converge,
    // and refuses before the engine — so this test can reach no cloud, whatever the machine has.
    it('reads the environment at converge, and refuses before any engine runs when the firewall has no source', async () => {
      delete process.env[CONTROL_PLANE_VAR];
      await expect(providerFor(d).converge('d', d)).rejects.toThrow(new RegExp(CONTROL_PLANE_VAR));
    });

    it('opens no key file but the one KONTRA_SSH_KEY names, and refuses when it is missing', async () => {
      process.env[CONTROL_PLANE_VAR] = '203.0.113.7';
      process.env.KONTRA_SSH_KEY = '/nonexistent/kontra-test/fleet_key';
      await expect(providerFor(d).converge('d', d)).rejects.toThrow(/cannot read the fleet SSH key at \/nonexistent\/kontra-test\/fleet_key/);
    });
  });
});
