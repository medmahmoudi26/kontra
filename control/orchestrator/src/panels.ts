/**
 * `panels` — the Dashboard streamer, a forked child of `orchestrator-infra` (ADR 0020).
 *
 *   node dist/src/panels.js        (forked and supervised by dist/src/infra.js)
 *
 * SAME CONTAINER, SAME KEY, DIFFERENT PID, and the reason is measured. Pulumi's Node language host
 * installs process-global `unhandledRejection` / `uncaughtException` handlers for the duration of
 * every inline run; an unhandled rejection injected two seconds into an inline `up()` made `up()`
 * throw, reporting the foreign error as `error: [runtime] Unhandled exception`, while **the process
 * survived**. Co-location therefore does not crash a provisioner — it fails `kontra fleet deploy`,
 * blames streaming code in the provisioner's error, and leaves the provisioner reporting healthy.
 * The handlers are process-global, not container-global, so a fork is the whole fix.
 *
 * It is NOT `orchestrator-api`: that process binds `0.0.0.0`, is host-published, and is
 * unauthenticated on most routes. It must not hold the key that reaches every Machine in the
 * run.
 *
 * This file is only assembly. Every seam it wires — discovery, the health probe, the snapshot exec,
 * the converge — is an interface implemented elsewhere and faked in tests, because no fleet exists
 * in this environment and no Machine can be SSHed to.
 */

import { realAttacher } from './panels/attach';
import { discoverMachines, pulumiStackReader, sshAddress, type MachineTarget } from './panels/discovery';
import { discoverWorkerContainers } from './panels/docker';
import { discoverLocalSessions, localRunner } from './panels/local';
import { enabledModes, panelRunner } from './panels/modes';
import { probeMachine, probeTarget } from './panels/probe';
import { realFleetHealthProbe } from './panels/health';
import type { ExecutionMode } from './panels/ids';
import { modeOf } from './panels/transport';
import { batchSnapshotCommand, parseBatchSnapshot } from './panels/tmux';
import { temporalConverger, type Converger } from './panels/converger';
import { WardenReports } from './panels/warden';
import {
  DEFAULT_DISCOVER_MS,
  DEFAULT_ORIGIN,
  DEFAULT_PORT,
  DEFAULT_SNAPSHOT_MS,
  PANEL_TOKEN_VARS,
  PanelServer,
  type PanelConfig,
  type PanelDeps,
} from './panels/server';

function log(line: string, extra?: Record<string, unknown>): void {
  // eslint-disable-next-line no-console
  console.log(`[panels] ${line}${extra ? ` ${JSON.stringify(extra)}` : ''}`);
}

function intEnv(name: string, fallback: number): number {
  const raw = process.env[name];
  if (!raw) return fallback;
  const n = Number(raw);
  return Number.isFinite(n) && n > 0 ? Math.floor(n) : fallback;
}

/** The CORS allow-list, comma-separated. Narrow by default: the SPA's own origin and nothing
 * else. Two origins in a browser is the cost of keeping panel bytes off the event loop that serves
 * the CLI's DuckDB queries. */
export function configuredOrigins(): string[] {
  return (process.env.KONTRA_PANEL_ORIGIN ?? DEFAULT_ORIGIN)
    .split(',')
    .map((o) => o.trim())
    .filter(Boolean);
}

export function panelConfig(): PanelConfig {
  return {
    snapshotMs: intEnv('KONTRA_PANEL_SNAPSHOT_MS', DEFAULT_SNAPSHOT_MS),
    discoverMs: intEnv('KONTRA_PANEL_DISCOVER_MS', DEFAULT_DISCOVER_MS),
    origins: configuredOrigins(),
  };
}

/**
 * Every node the streamer can see, across every mode that is switched on.
 *
 * ONE discovery per mode, each INDEPENDENTLY FALLIBLE. A missing state directory, a missing `docker`
 * binary and a host with no tmux server are all normal, and none of them may cost the other modes
 * their tiles — so each is caught, logged with the mode that failed, and contributes nothing. That is
 * also what makes `KONTRA_PANEL_MODES` default to all three: a mode with nothing to find costs a
 * streamer one failed exec per discovery interval and says so once in the log.
 */
export async function discoverAllModes(
  modes: readonly ExecutionMode[],
  reader: ReturnType<typeof pulumiStackReader>
): Promise<MachineTarget[]> {
  const out: MachineTarget[] = [];
  for (const mode of modes) {
    try {
      switch (mode) {
        case 'fleet':
          out.push(...(await discoverMachines(reader)));
          break;
        case 'local':
          out.push(...(await discoverLocalSessions(localRunner)));
          break;
        case 'docker': {
          const { machines, error } = await discoverWorkerContainers();
          if (error) log('docker discovery found nothing', { err: error });
          out.push(...machines);
          break;
        }
      }
    } catch (err) {
      // A mode's discovery THROWING is a bug in that discovery, not a reason for the wall to empty.
      log('discovery failed for one mode', { mode, err: String(err) });
    }
  }
  return out;
}

/** The real seams. */
export function realDeps(converger?: Converger): PanelDeps {
  const reader = pulumiStackReader();
  const { modes, unknown } = enabledModes();
  if (unknown.length) {
    log('KONTRA_PANEL_MODES named modes that do not exist; they are ignored', {
      unknown: unknown.join(','),
    });
  }
  const deps: PanelDeps = {
    discover: () => discoverAllModes(modes, reader),
    // ONE probe and ONE snapshot for all three modes: `panelRunner` picks the transport from the
    // target's own mode, so neither of these functions knows how many there are.
    probe: (m: MachineTarget) => probeMachine(panelRunner, m),
    async snapshot(m: MachineTarget, windows: string[]) {
      const res = await panelRunner.run(probeTarget(m), batchSnapshotCommand(m.session, windows), {
        timeoutMs: 15_000,
      });
      // A non-zero exit is not an empty wall: the per-window `|| true` means partial output is
      // normal, and what is missing shows up as that window reporting absent on the next probe.
      return parseBatchSnapshot(res.stdout);
    },
    // The live-on-focus attach. On the fleet it shares `ssh.ts`'s ControlMaster with the two execs
    // above, so a focused tile costs a channel on an existing connection rather than a second one;
    // in the other two modes it is a `docker exec` or a local `sh`, chosen from the target.
    attach: realAttacher(log),
    // The `poller` and `loads` signals. Wired UNCONDITIONALLY on purpose: both halves are bounded
    // and both fail to `unknown` with a sentence, so a streamer with no Temporal and no metrics
    // backend shows chips that say what it does not know rather than chips that say `ok`.
    fleetHealth: realFleetHealthProbe({ log }),
    // Panes and telemetry from the Machines themselves (ADR 0037). Also unconditional, and it costs
    // nothing until a Machine reports: an empty store changes no behaviour, because every rule in
    // `server.ts` that reads it is keyed on a Machine having ALREADY spoken. A streamer serving a
    // Fleet placed before the Warden behaves exactly as it did.
    reports: new WardenReports(),
    log,
  };
  if (converger) {
    deps.converge = (m) => {
      // Belt to `server.ts`'s braces, which refuses a non-fleet converge before it ever gets here.
      // The workflow SSHes to a Machine as root and its script apt-installs tmux; pointing it at a
      // container name or a local hostname would be the Fleet's authority aimed somewhere it does not
      // belong (`CONTEXT-MAP.md`), so this is a throw and not a best-effort translation.
      if (modeOf(m) !== 'fleet') {
        return Promise.reject(
          new Error(
            `panels: a ${modeOf(m)} session is not converged by the Fleet workflow — ` +
              'session existence there belongs to `kontra serve --tmux` and `kontra scale`'
          )
        );
      }
      return converger.converge(m);
    };
  }
  return deps;
}

export async function main(): Promise<void> {
  const port = intEnv('KONTRA_PANEL_PORT', DEFAULT_PORT);
  const converger = temporalConverger();
  const server = new PanelServer(realDeps(converger), panelConfig());
  const bound = await server.listen(port);

  const enabled = Boolean(process.env[PANEL_TOKEN_VARS[0]]);
  log(
    `listening on :${bound} snapshot=${panelConfig().snapshotMs}ms ` +
      `modes=${enabledModes().modes.join(',')} origins=${configuredOrigins().join(',')}`
  );
  if (!enabled) {
    log(
      `NO ${PANEL_TOKEN_VARS[0]} CONFIGURED — every panel route answers 503 and serves nothing. ` +
        'This is the fail-closed default, not a fault.'
    );
  }

  // Draining on SIGTERM matters because the supervisor restarts what exits: a child that ignored
  // the signal would be killed, and its sockets would be reset rather than closed.
  const shutdown = (signal: string): void => {
    log(`${signal} — closing`);
    void (async () => {
      try {
        await server.close();
        await converger.close();
      } catch (err) {
        log('shutdown error', { err: String(err) });
      } finally {
        process.exit(0);
      }
    })();
  };
  process.on('SIGTERM', () => shutdown('SIGTERM'));
  process.on('SIGINT', () => shutdown('SIGINT'));
}

if (require.main === module) {
  // Explicit rather than inherited: Node's default for an unhandled rejection is a non-zero exit
  // with a stack, which is right — but a LOG LINE naming this process is what stops the next
  // operator from reading the supervisor's restart as the Pulumi engine dying. In this PID a crash
  // costs the view and nothing else; the supervisor brings it back.
  process.on('unhandledRejection', (reason) => {
    log('fatal: unhandled rejection', { reason: String(reason) });
    process.exit(1);
  });
  process.on('uncaughtException', (err) => {
    log('fatal: uncaught exception', { err: String(err) });
    process.exit(1);
  });
  main().catch((err) => {
    log('fatal:', { err: String(err) });
    process.exit(1);
  });
}
