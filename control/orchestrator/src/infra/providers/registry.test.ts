import { describe, expect, it } from 'vitest';

import { parseFleets } from '../fleets';
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

  it('refuses the providers that are not built yet, naming the slice', async () => {
    const d = parseFleets('fleets:\n  default: d\n  d:\n    provider: digital_ocean\n    region: fra1\n    token: t\n    ssh_key_fingerprint: f\n    sizes: { m: s-1vcpu-1gb }\n').profiles.d!;
    await expect(providerFor(d).converge('d', d)).rejects.toThrow(/slice 9/);
  });
});
