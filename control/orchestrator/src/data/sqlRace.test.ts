import { randomBytes } from 'node:crypto';

import { afterAll, describe, expect, it } from 'vitest';

import { createDriver, isCreationRace, type SqlDriver } from './sql';

/**
 * A FRESH INSTALL LOST A RACE AT BOOT, AT RANDOM, AND NEVER CAME UP.
 *
 * Five stores run `CREATE SCHEMA IF NOT EXISTS kontra` when they first touch Postgres, in two
 * processes (api and infra) that start together. Two sessions running that at once both pass the
 * existence check, one inserts the schema, and the other gets
 * `duplicate key value violates unique constraint "pg_namespace_nspname_index"` instead of a no-op.
 * The orchestrator treated that as fatal, so the API never became healthy. It happened on two CI
 * runs in a row once the report store's init stopped failing earlier and joined the race.
 *
 * The concurrent case needs a real Postgres (`KONTRA_TEST_PG`, which CI's orchestrator job sets).
 * Without it, only the classification is tested.
 */

describe('isCreationRace', () => {
  it('is the lost-race codes on IF NOT EXISTS DDL, and nothing else', () => {
    const dupIndex = Object.assign(new Error('duplicate key'), { code: '23505' });
    expect(isCreationRace('CREATE SCHEMA IF NOT EXISTS kontra', dupIndex)).toBe(true);
    expect(isCreationRace('create table if not exists x (a int)', { code: '42P07' })).toBe(true);
    // A unique violation on an INSERT is a real conflict, not a race to retry.
    expect(isCreationRace('INSERT INTO t VALUES (1)', dupIndex)).toBe(false);
    // And a different error on IF NOT EXISTS DDL propagates.
    expect(isCreationRace('CREATE SCHEMA IF NOT EXISTS kontra', { code: '42501' })).toBe(false);
    expect(isCreationRace('CREATE SCHEMA IF NOT EXISTS kontra', new Error('no code'))).toBe(false);
  });
});

const PG = process.env.KONTRA_TEST_PG;

describe.skipIf(!PG)('concurrent IF NOT EXISTS DDL on a real Postgres', () => {
  const schema = `race_${randomBytes(4).toString('hex')}`;
  const drivers: SqlDriver[] = [];

  afterAll(async () => {
    await drivers[0]?.exec(`DROP SCHEMA IF EXISTS ${schema} CASCADE`);
    await Promise.all(drivers.map((d) => d.close()));
  });

  it('lets every one of a dozen simultaneous creators succeed: schema, then table', async () => {
    for (let i = 0; i < 12; i++) drivers.push(createDriver({ url: PG!, schema }).driver);
    // Several rounds, because one round can be won without a collision by timing alone.
    for (let round = 0; round < 5; round++) {
      await drivers[0]!.exec(`DROP SCHEMA IF EXISTS ${schema} CASCADE`);
      await expect(
        Promise.all(drivers.map((d) => d.exec(`CREATE SCHEMA IF NOT EXISTS ${schema}`)))
      ).resolves.toBeDefined();
      await expect(
        Promise.all(drivers.map((d) => d.exec(`CREATE TABLE IF NOT EXISTS ${schema}.t (a INT)`)))
      ).resolves.toBeDefined();
    }
  });
});
