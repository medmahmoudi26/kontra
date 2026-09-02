import { describe, expect, it } from 'vitest';
import type { MachineTarget } from './discovery';
import {
  describeFleetQueues,
  describeQueue,
  hostIsMachine,
  identityHost,
  parseDescribeResponse,
  pollerFor,
  queueForMachine,
  timestampToMs,
  TASK_QUEUE_KIND_NORMAL,
  TASK_QUEUE_TYPE,
  type PollerInfo,
  type QueueDescriber,
  type TaskQueueType,
} from './pollers';

const MACHINE: MachineTarget = {
  machine: 'kf-crawl-01',
  host: '10.124.0.9',
  publicIp: '203.0.113.9',
  tag: 'crawl',
  fleet: 'run-apex-119',
  actor: 'webcrawl',
  version: '0.2.0',
  session: 'kontra-webcrawl',
  windows: ['actor', 'handler'],
};

/** A `queueDescriber` fake — the whole reason `cli/workers.go` has that interface. Records what it
 * was asked, so the fold's ORDER and its stop-on-first-error are both observable. */
function fakeDescriber(
  answers: Partial<Record<TaskQueueType, PollerInfo[] | Error>>,
  perQueue?: Record<string, Partial<Record<TaskQueueType, PollerInfo[] | Error>>>
): QueueDescriber & { asked: Array<{ queue: string; type: TaskQueueType }> } {
  const asked: Array<{ queue: string; type: TaskQueueType }> = [];
  return {
    asked,
    async pollers(queue, type) {
      asked.push({ queue, type });
      const answer = perQueue?.[queue]?.[type] ?? answers[type] ?? [];
      if (answer instanceof Error) throw answer;
      return answer;
    },
    async close() {},
  };
}

const goPoller = (host: string, pid = 42, queue = 'webcrawl-0.2.0'): PollerInfo => ({
  // The Go SDK default, verbatim: `<pid>@<hostname>@<taskqueue>`. runtime/handler/main.go passes no
  // Identity to client.Dial, so this is what Temporal sees.
  identity: `${pid}@${host}@${queue}`,
  lastAccess: 1_700_000_000_000,
});

describe('which queue a Machine is asked about', () => {
  // THE NAME ITSELF is pinned by shared/conformance/queues.json (queues.conformance.test.ts), because it
  // is one string eight derivations in four languages have to spell the same way. What is this
  // file's own is the question above it: which Machines HAVE a queue to describe.
  it('has no queue for a Machine with no placement', () => {
    // `sharedQueue(actor, '')` is `<actor>-shared`, a real but DIFFERENT queue: describing it for
    // an unplaced Machine would report pollers on somebody else's queue as this tile's.
    expect(queueForMachine(MACHINE)).toBe('webcrawl-0.2.0');
    expect(queueForMachine({ ...MACHINE, actor: '', version: '' })).toBeUndefined();
  });
});

describe('describeQueue mirrors cli/workers.go', () => {
  it('folds WORKFLOW + ACTIVITY into DISTINCT identities and the freshest poll', async () => {
    const d = fakeDescriber({
      workflow: [
        { identity: 'a', lastAccess: 100 },
        { identity: 'b', lastAccess: 300 },
      ],
      // A live worker polls both types with ONE identity — that is why the fold is by identity and
      // not a sum of two counts.
      activity: [
        { identity: 'a', lastAccess: 500 },
        { identity: 'b', lastAccess: 200 },
      ],
    });

    const q = await describeQueue(d, 'webcrawl-0.2.0');
    expect(q.identities).toEqual(['a', 'b']);
    expect(q.lastPoll).toBe(500);
    expect(q.error).toBeUndefined();
    expect(d.asked.map((a) => a.type)).toEqual(['workflow', 'activity']);
  });

  it('asks WORKFLOW and ACTIVITY and never NEXUS', async () => {
    const d = fakeDescriber({});
    await describeQueue(d, 'webcrawl-0.2.0');
    expect(d.asked.map((a) => a.type)).toEqual(['workflow', 'activity']);
    expect(TASK_QUEUE_TYPE.workflow).toBe(1);
    expect(TASK_QUEUE_TYPE.activity).toBe(2);
    expect(TASK_QUEUE_KIND_NORMAL).toBe(1);
  });

  it('reports NO identities and no error when the queue is registered and idle', async () => {
    const q = await describeQueue(fakeDescriber({ workflow: [], activity: [] }), 'echo-shared');
    expect(q.identities).toEqual([]);
    expect(q.error).toBeUndefined();
  });

  it('STOPS at the first type that errors, and drops the identities it already had', async () => {
    // The half-fold rule: a count from one of two types is not a smaller count, it is an unknown
    // one. Go breaks out of the loop for the same reason.
    const d = fakeDescriber({
      workflow: [{ identity: 'a', lastAccess: 100 }],
      activity: new Error('connection refused'),
    });
    const q = await describeQueue(d, 'webcrawl-0.2.0');
    expect(q.error).toBe('connection refused');
    expect(q.identities).toEqual([]);
    expect(q.lastPoll).toBe(0);
  });

  it('does not ask the second type once the first has failed', async () => {
    const d = fakeDescriber({ workflow: new Error('deadline exceeded'), activity: [] });
    await describeQueue(d, 'webcrawl-0.2.0');
    expect(d.asked.map((a) => a.type)).toEqual(['workflow']);
  });
});

describe('poller attribution', () => {
  it('reads the host out of both SDKs default identity shapes', () => {
    expect(identityHost('42@kf-crawl-01@webcrawl-0.2.0')).toBe('kf-crawl-01'); // Go worker
    expect(identityHost('42@kf-crawl-01@')).toBe('kf-crawl-01'); // Go client
    expect(identityHost('4711@kf-crawl-02')).toBe('kf-crawl-02'); // Python
  });

  it('returns undefined for an identity it does not understand', () => {
    // A custom Identity is legal. Undefined must reach the caller as "cannot attribute", never as
    // "not this Machine".
    expect(identityHost('some-custom-identity')).toBeUndefined();
    expect(identityHost('42@')).toBeUndefined();
    expect(identityHost('')).toBeUndefined();
  });

  it('matches an FQDN against the Machine name by its first label', () => {
    expect(hostIsMachine('kf-crawl-01', 'kf-crawl-01')).toBe(true);
    expect(hostIsMachine('kf-crawl-01.internal', 'kf-crawl-01')).toBe(true);
    expect(hostIsMachine('KF-CRAWL-01', 'kf-crawl-01')).toBe(true);
    expect(hostIsMachine('kf-crawl-02', 'kf-crawl-01')).toBe(false);
  });
});

describe('pollerFor: the three values, and which facts produce them', () => {
  const queue = 'webcrawl-0.2.0';

  it('live when a poller identity names this Machine', () => {
    const v = pollerFor('kf-crawl-01', {
      queue,
      identities: [goPoller('kf-crawl-01').identity],
      lastPoll: 1,
    });
    expect(v.poller).toBe('live');
    // A healthy signal contributes no sentence: `detail` is for failing signals only.
    expect(v.detail).toBeUndefined();
  });

  it('none when the queue exists and NOTHING polls it — the trap the CLI was written for', () => {
    const v = pollerFor('kf-crawl-01', { queue, identities: [], lastPoll: 0 });
    expect(v.poller).toBe('none');
    expect(v.detail).toContain('registered but nothing is polling');
  });

  it('unknown, NEVER none, when the describe failed', () => {
    // The rule heartbeat.ts documents: "we could not ask" and "nothing is polling" are different
    // facts, and a dial failure in cli/workers.go is a loud error rather than `(none)`.
    const v = pollerFor('kf-crawl-01', { queue, identities: [], lastPoll: 0, error: 'no such host' });
    expect(v.poller).toBe('unknown');
    expect(v.poller).not.toBe('none');
    expect(v.detail).toContain('could not ask Temporal');
    expect(v.detail).toContain('no such host');
  });

  it('unknown when the Machine carries no placement', () => {
    expect(pollerFor('kf-crawl-01', undefined).poller).toBe('unknown');
    expect(pollerFor('kf-crawl-01', undefined).detail).toContain('no actor placement');
  });

  it('none for the ONE dead Machine among healthy peers — the failure a queue-level answer hides', () => {
    // Nine handlers polling one shared queue would make a queue-level `live` paint the tenth green.
    // That is the round-3 shape, one layer up, and it is why attribution exists.
    const identities = ['kf-crawl-01', 'kf-crawl-02', 'kf-crawl-03'].map((h) => goPoller(h).identity);
    const state = { queue, identities: identities.sort(), lastPoll: 9 };

    expect(pollerFor('kf-crawl-02', state).poller).toBe('live');
    const dead = pollerFor('kf-crawl-09', state);
    expect(dead.poller).toBe('none');
    expect(dead.detail).toContain('none from kf-crawl-09');
    expect(dead.detail).toContain('kontra-handler.service');
  });

  it('unknown when identities exist but none can be attributed at all', () => {
    // Someone set a custom Identity. We do not know whether this Machine is among them, and
    // guessing `none` would red-flag a healthy Worker while guessing `live` would hide a dead one.
    const v = pollerFor('kf-crawl-01', {
      queue,
      identities: ['custom-worker-a', 'custom-worker-b'],
      lastPoll: 3,
    });
    expect(v.poller).toBe('unknown');
    expect(v.detail).toContain('no identity names a host');
  });
});

describe('describeFleetQueues', () => {
  it('describes each distinct queue ONCE, however many Machines share it', async () => {
    const d = fakeDescriber({ workflow: [], activity: [] });
    const machines: MachineTarget[] = [
      MACHINE,
      { ...MACHINE, machine: 'kf-crawl-02' },
      { ...MACHINE, machine: 'kf-crawl-03' },
      // A different actor is a different queue.
      { ...MACHINE, machine: 'kf-scan-01', actor: 'portscan', version: '1.0.0' },
    ];

    const queues = await describeFleetQueues(d, machines);
    expect([...queues.keys()].sort()).toEqual(['portscan-1.0.0', 'webcrawl-0.2.0']);
    // Two queues x two task-queue types = four calls for four Machines, not eight.
    expect(d.asked.length).toBe(4);
  });

  it('contributes no queue for a Machine with no placement', async () => {
    const d = fakeDescriber({ workflow: [], activity: [] });
    const queues = await describeFleetQueues(d, [{ ...MACHINE, actor: '', version: '' }]);
    expect(queues.size).toBe(0);
    expect(d.asked).toEqual([]);
  });

  it('keeps one failing queue from making the other unknown', async () => {
    const d = fakeDescriber(
      { workflow: [], activity: [] },
      { 'portscan-1.0.0': { workflow: new Error('queue not found') } }
    );
    const queues = await describeFleetQueues(d, [
      MACHINE,
      { ...MACHINE, machine: 'kf-scan-01', actor: 'portscan', version: '1.0.0' },
    ]);
    expect(queues.get('webcrawl-0.2.0')?.error).toBeUndefined();
    expect(queues.get('portscan-1.0.0')?.error).toBe('queue not found');
  });
});

describe('the DescribeTaskQueue wire shape', () => {
  it('parses pollers, including a Long-encoded lastAccessTime', () => {
    // protobufjs hands an int64 back as {low, high} unless the client was built otherwise, which is
    // why this is a function and not a `new Date(ts.seconds)`.
    const pollers = parseDescribeResponse({
      pollers: [
        { identity: '42@kf-crawl-01@q', lastAccessTime: { seconds: { low: 1700000000, high: 0 }, nanos: 500_000_000 } },
        { identity: '43@kf-crawl-02@q', lastAccessTime: { seconds: '1700000001', nanos: 0 } },
        { identity: '44@kf-crawl-03@q', lastAccessTime: { seconds: 1700000002 } },
      ],
    });
    expect(pollers.map((p) => p.identity)).toEqual([
      '42@kf-crawl-01@q',
      '43@kf-crawl-02@q',
      '44@kf-crawl-03@q',
    ]);
    expect(pollers[0]?.lastAccess).toBe(1_700_000_000_500);
    expect(pollers[1]?.lastAccess).toBe(1_700_000_001_000);
    expect(pollers[2]?.lastAccess).toBe(1_700_000_002_000);
  });

  it('reads an empty or absent poller list as no pollers, not as an error', () => {
    expect(parseDescribeResponse({})).toEqual([]);
    expect(parseDescribeResponse({ pollers: null })).toEqual([]);
    expect(parseDescribeResponse({ pollers: [null] })).toEqual([]);
  });

  it('reads an absent timestamp as 0 rather than inventing a poll time', () => {
    expect(timestampToMs(null)).toBe(0);
    expect(timestampToMs({})).toBe(0);
    expect(timestampToMs({ seconds: 'not-a-number' })).toBe(0);
  });
});
