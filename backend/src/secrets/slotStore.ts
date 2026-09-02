/**
 * WHERE DECLARATIONS AND BINDINGS REST, and the one path that turns a slot into bytes.
 *
 * `slots.ts` is the vocabulary and the pure state; this is the half with a disk, a clock and a
 * {@link SecretStore} behind it. The split is the same one `secrets.ts` and `SecretsSection.tsx`
 * make on the web side, and for the same reason: every state a page can be in is a node test with
 * no filesystem, and the file that CAN reach a value is small enough to read in one sitting.
 *
 * ── THE FILE ──────────────────────────────────────────────────────────────────────────────────
 *
 * `slots.json`, beside the store, holding two maps and no values. It is deliberately NOT a table
 * in `orchestrator.db`: a binding is half of a credential grant and belongs with the credentials,
 * under the same 0700 directory, moving with `KONTRA_HOME` the way the rest of an operator's
 * secret state does. A grant that lived in the application database would survive a `rm -rf
 * ~/.kontra/secrets` and would point at secrets that no longer exist — and it would be backed up
 * by whoever backs up the app database, which is not the party who decided where credentials rest.
 *
 * ── RESOLUTION IS FOUR QUESTIONS IN ORDER, AND EVERY ANSWER IS A LINE IN THE LEDGER ───────────
 *
 *   1. Is the asker who it says it is?           — `identity.ts`, and the route asks it, not this.
 *   2. Did THIS actor version declare this slot? — no ⇒ `SlotUndeclared`. The fourth case.
 *   3. Has the operator bound anything to it?    — no ⇒ `SlotUnbound`.
 *   4. Does that secret still resolve?           — `SecretRevoked` / `NoSuchSecret` pass through.
 *
 * Each of those writes a {@link Resolution} before it throws, because the refusals are the
 * interesting half (`audit.ts`) — and the record never carries a value, a digest or a length.
 */

import { existsSync, mkdirSync, readFileSync, renameSync, writeFileSync } from 'node:fs';
import path from 'node:path';

import { compareVersionsDesc } from '../versions';
import { ResolutionLog, type Resolution, type ResolutionOutcome } from './audit';
import { secretsDir } from './keyring';
import {
  SlotRefused,
  SlotUndeclared,
  SlotUnbound,
  addedSlots,
  assertSlotActor,
  assertSlotName,
  bindingKey,
  declarationKey,
  slotStatuses,
  unboundRefusal,
  type ActorRef,
  type ActorSlots,
  type SlotBinding,
  type SlotDeclaration,
  type SlotSpec,
} from './slots';
import { secretStore, type SecretStore } from './store';
import { NoSuchSecret, SecretForbidden, SecretRevoked, actorIdentity, type SecretMeta } from './types';

/** What the file says it is, so a format change is recognised rather than guessed at. */
const SCHEMA = 'kontra.slots.v1';

interface SlotFile {
  schema: string;
  /** `probe@0.2.0` → what that version asks for. */
  declarations: Record<string, SlotDeclaration>;
  /** `probe/api_key` → what the operator granted. Keyed WITHOUT a version — see `slots.ts`. */
  bindings: Record<string, SlotBinding>;
}

export interface SlotStoreOptions {
  /** The store directory. Defaults to `KONTRA_SECRETS_DIR` or `~/.kontra/secrets`. */
  dir?: string;
  /** Injected clock, so a test can pin `declaredAt`/`boundAt`. */
  now?: () => number;
  /** Injected ledger, so a test reads what was written without a second path to it. */
  log?: ResolutionLog;
}

/** What an actor's worker asserts about itself when it asks for a slot. */
export interface SlotRequest {
  /** `actor:<name>`, as `store.verifyIdentity` returned it. Never taken off a request body. */
  identity: string;
  slot: string;
  /** The actor version the worker says it is running — self-asserted, see `audit.ts:Resolution`. */
  version: string;
  /** The run being served, when the worker knows one. */
  run?: string;
}

export class SlotStore {
  private readonly dir: string;
  private readonly file: string;
  private readonly now: () => number;
  private readonly ledger: ResolutionLog;
  /** Every mutation queues behind the last — read-modify-write on one file, as `fileBackend` does. */
  private chain: Promise<unknown> = Promise.resolve();

  constructor(
    private readonly secrets: SecretStore,
    opts: SlotStoreOptions = {}
  ) {
    this.dir = opts.dir ?? secretsDir();
    this.file = path.join(this.dir, 'slots.json');
    this.now = opts.now ?? Date.now;
    this.ledger = opts.log ?? new ResolutionLog({ dir: this.dir });
  }

  /** The resolution ledger — read by the UI, written by {@link resolveSlot} and nothing else. */
  get log(): ResolutionLog {
    return this.ledger;
  }

  /** Where the declarations and bindings rest. Shown beside the store's own location. */
  get location(): string {
    return this.file;
  }

  // --- declaring -------------------------------------------------------------------------------

  /**
   * Record what one `(actor, version)` asks for. Called by a worker as it registers itself.
   *
   * IDEMPOTENT AND LAST-WRITE-WINS, unlike the catalog's own registration gate. A slot list is not
   * a schema a caller has typed against: nothing downstream breaks when a version re-declares the
   * same slots, and a worker restarting is the ordinary case rather than a contract change. What a
   * re-declaration CANNOT do is change another version's declaration, which is what keeps
   * {@link ActorSlots.added} a true diff.
   */
  async declare(actor: string, version: string, slots: readonly SlotSpec[]): Promise<ActorSlots> {
    // `async`, so a bad name REJECTS rather than throwing synchronously. Half the callers of this
    // are `await`-ing inside a try/catch and the other half are `.catch()`ing a promise; a method
    // that threw before it returned one would be caught by the first and escape the second.
    const name = assertSlotActor(actor);
    const v = assertVersion(version);
    const kept = normaliseSlots(slots);
    await this.mutate((file) => {
      file.declarations[declarationKey(name, v)] = {
        actor: name,
        version: v,
        slots: kept,
        declaredAt: this.now(),
      };
      return file;
    });
    return this.view(name, v);
  }

  /** Every version of one actor that has declared, NEWEST FIRST (`../versions.ts`'s ordering). */
  declarations(actor: string): SlotDeclaration[] {
    return Object.values(this.read().declarations)
      .filter((d) => d.actor === actor)
      .sort((a, b) => compareVersionsDesc(a.version, b.version));
  }

  /** Every actor that has ever declared a slot, by name. */
  declaredActors(): string[] {
    return [...new Set(Object.values(this.read().declarations).map((d) => d.actor))].sort();
  }

  // --- binding ---------------------------------------------------------------------------------

  /** The operator's grants, for one actor or for all of them. */
  bindings(actor?: string): SlotBinding[] {
    return Object.values(this.read().bindings)
      .filter((b) => actor === undefined || b.actor === actor)
      .sort((a, b) => a.actor.localeCompare(b.actor) || a.slot.localeCompare(b.slot));
  }

  /**
   * Bind a slot to one of the operator's secrets, or REBIND it — the same verb for both, the way
   * the store has one verb for create and rotate.
   *
   * TWO REFUSALS, and both are about a grant that would not mean what it looks like:
   *
   *  - **The secret must EXIST.** Binding to a name that is not in the store writes a grant that
   *    fails at the first resolution, at which point the operator is debugging an actor rather
   *    than a typo. `missing` as a STATE is for a secret destroyed after the binding was made,
   *    which is a real situation; a binding that was never resolvable is just a mistake.
   *  - **An OWNED secret may only be bound to its OWNER's slot.** Otherwise binding is a way to
   *    hand one actor's credential to another without touching the ownership that is supposed to
   *    prevent exactly that, and it would look like an ordinary grant while doing it. Refused here
   *    AND again at use (`store.ts:resolve`), because the two checks fail for different reasons —
   *    this one so nobody makes the grant, that one so a grant made before an ownership was
   *    established still cannot be used.
   *
   * BINDING A SLOT NOTHING HAS DECLARED IS ALLOWED. An operator preparing an install before the
   * actor has ever been served has nothing to declare against yet, and refusing would make the
   * order of two independent acts matter. It is drawn as an unused grant in Settings rather than
   * silently kept, so it cannot rot unseen.
   */
  async bind(actor: string, slot: string, secret: string): Promise<SlotBinding> {
    const name = assertSlotActor(actor);
    const key = assertSlotName(slot);
    const meta = await this.secrets.describe(secret);
    if (!meta) {
      throw new SlotRefused(
        `there is no secret named ${JSON.stringify(secret)} — create it in Settings first, then bind ` +
          `${JSON.stringify(key)} to it`
      );
    }
    if (meta.owner && meta.owner !== actorIdentity(name)) {
      throw new SlotRefused(
        `${JSON.stringify(secret)} belongs to ${meta.owner} and cannot be bound into ${name}'s slot ` +
          `${JSON.stringify(key)} — an owned secret is resolvable only by its owner, and a binding is ` +
          'not a way around that'
      );
    }
    const binding: SlotBinding = { actor: name, slot: key, secret: meta.name, boundAt: this.now() };
    await this.mutate((file) => {
      file.bindings[bindingKey(name, key)] = binding;
      return file;
    });
    return binding;
  }

  /** Withdraw a grant. `false` when there was nothing to withdraw — a second click is not an error. */
  async unbind(actor: string, slot: string): Promise<boolean> {
    const key = bindingKey(assertSlotActor(actor), assertSlotName(slot));
    return this.mutate((file) => {
      const had = key in file.bindings;
      delete file.bindings[key];
      return { file, result: had };
    });
  }

  // --- reading ---------------------------------------------------------------------------------

  /**
   * What one actor version asks for and what it would get today, plus the diff against the version
   * before it.
   *
   * A version nobody has declared for answers with NO SLOTS and an empty `version`, which is not
   * the same statement as "this version needs nothing": it is "nothing has told us". The caller
   * that has to tell those apart is {@link refuseRun}, and it does.
   */
  async view(actor: string, version?: string): Promise<ActorSlots> {
    const decls = this.declarations(actor);
    const index = version ? decls.findIndex((d) => d.version === version) : 0;
    const pick = index >= 0 ? decls[index] : undefined;
    if (!pick) return { actor, version: version ?? '', slots: [], added: [] };
    const previous = decls[index + 1];
    const bindings = this.bindings(actor);
    const metas = await this.secretIndex();
    return {
      actor,
      version: pick.version,
      slots: slotStatuses(pick.slots, bindings, metas),
      added: previous ? addedSlots(pick.slots, previous.slots) : [],
      ...(previous ? { comparedWith: previous.version } : {}),
    };
  }

  /** The newest declared version of every actor that has declared one. What Settings draws. */
  async views(): Promise<ActorSlots[]> {
    const out: ActorSlots[] = [];
    for (const actor of this.declaredActors()) out.push(await this.view(actor));
    return out;
  }

  /**
   * The refusal a run start is answered with, or `null` when every named actor can resolve
   * everything it declares. Nothing here reads a value — a preflight that resolved credentials in
   * order to check them would be handing them out to prove they need not be handed out.
   *
   * AN ACTOR THAT HAS DECLARED NOTHING IS NOT A REFUSAL. It is either an actor with no credentials
   * or one whose worker has not registered yet, and refusing a run over the second would make
   * every first serve a failure.
   *
   * A NAMED VERSION THAT HAS NEVER DECLARED falls back to the newest declaration of that actor.
   * The refusal names the version it actually checked, so the fallback is visible rather than
   * implied — checking nothing would be the one answer that makes this gate worthless.
   */
  async refuseRun(actors: readonly ActorRef[]): Promise<string | null> {
    const seen = new Set<string>();
    const views: ActorSlots[] = [];
    for (const ref of actors) {
      if (typeof ref.name !== 'string' || !ref.name) continue;
      let view = await this.view(ref.name, ref.version);
      if (view.slots.length === 0 && ref.version) view = await this.view(ref.name);
      if (view.slots.length === 0) continue;
      const key = declarationKey(view.actor, view.version);
      if (seen.has(key)) continue;
      seen.add(key);
      views.push(view);
    }
    return unboundRefusal(views);
  }

  // --- resolving -------------------------------------------------------------------------------

  /**
   * A slot, from the actor that declared it, to bytes. THE ONLY PATH IN THIS FILE THAT RETURNS A
   * VALUE, and the only caller of {@link SecretStore.resolve} in the slot half of this package.
   *
   * THE SECRET'S NAME IS NOT IN THE ANSWER. The actor learns its own slot name, which it wrote,
   * and the value. Telling it which of the operator's secrets answered would give back exactly the
   * thing the indirection exists to withhold — a third-party actor's author would learn your
   * inventory by asking for the credential you granted it.
   */
  async resolveSlot(req: SlotRequest): Promise<{ slot: string; value: string }> {
    const actor = actorOf(req.identity);
    const slot = assertSlotName(req.slot);
    const version = typeof req.version === 'string' ? req.version.trim() : '';
    const run = typeof req.run === 'string' ? req.run.trim() : '';
    const audit = (outcome: ResolutionOutcome, secret: string, secretVersion = 0): void => {
      this.ledger.record({
        at: this.now(),
        actor,
        version,
        slot,
        run,
        secret,
        secretVersion,
        outcome,
      } satisfies Resolution);
    };

    // 2. DID THIS VERSION DECLARE IT. Against the version the worker says it is, exactly — not
    //    against the newest, and not against the union of every version. An actor that could
    //    inherit a slot from a build it is not running would make the declaration a hint.
    const declared = this.declarations(actor).find((d) => d.version === version);
    const spec = declared?.slots.find((s) => s.name === slot);
    if (!spec) {
      audit('undeclared', '');
      throw new SlotUndeclared(
        declared
          ? `${declarationKey(actor, version)} does not declare a slot named ${JSON.stringify(slot)} — ` +
            'an actor may only ask for credentials it declared, so that what it will ask for is ' +
            'visible before it runs'
          : `nothing has registered what ${declarationKey(actor, version || '?')} asks for, so ` +
            `${JSON.stringify(slot)} cannot be resolved. Serve this version so it registers its slots.`
      );
    }

    // 3. HAS THE OPERATOR BOUND ANYTHING.
    const bound = this.bindings(actor).find((b) => b.slot === slot);
    if (!bound) {
      audit('unbound', '');
      throw new SlotUnbound(
        `${actor} declares the slot ${JSON.stringify(slot)} and nothing is bound to it — bind it to ` +
          'one of your secrets in Settings'
      );
    }

    // 4. DOES THAT SECRET STILL RESOLVE. The binding principal is minted HERE and nowhere else,
    //    from the record just read, so the grant and the name it grants cannot come apart.
    try {
      const got = await this.secrets.resolve(
        { name: bound.secret },
        { kind: 'binding', identity: req.identity, slot, secret: bound.secret }
      );
      audit('resolved', bound.secret, got.version);
      return { slot, value: got.value };
    } catch (err) {
      audit(outcomeOf(err), bound.secret);
      throw err;
    }
  }

  // --- the file --------------------------------------------------------------------------------

  private async secretIndex(): Promise<Map<string, SecretMeta>> {
    return new Map((await this.secrets.list()).map((s) => [s.name, s]));
  }

  private read(): SlotFile {
    const empty: SlotFile = { schema: SCHEMA, declarations: {}, bindings: {} };
    if (!existsSync(this.file)) return empty;
    const parsed = JSON.parse(readFileSync(this.file, 'utf8')) as SlotFile;
    if (parsed.schema !== SCHEMA) {
      throw new Error(`${this.file} is ${JSON.stringify(parsed.schema)}, not ${SCHEMA} — refusing to read it`);
    }
    return { schema: SCHEMA, declarations: parsed.declarations ?? {}, bindings: parsed.bindings ?? {} };
  }

  /**
   * Read, change, write — one at a time, atomically. Same shape as `fileBackend.ts:mutate` and for
   * the same two reasons: two concurrent binds otherwise lose whichever lost the race, and a crash
   * mid-write must leave the previous grants rather than half of them.
   */
  private mutate<T>(fn: (file: SlotFile) => SlotFile | { file: SlotFile; result: T }): Promise<T> {
    const next = this.chain.then(() => {
      const file = this.read();
      const out = fn(file);
      const written = 'schema' in out ? out : out.file;
      const result = 'schema' in out ? (undefined as T) : out.result;
      if (!existsSync(this.dir)) mkdirSync(this.dir, { recursive: true, mode: 0o700 });
      const tmp = `${this.file}.${process.pid}.tmp`;
      writeFileSync(tmp, `${JSON.stringify(written, null, 2)}\n`, { mode: 0o600 });
      renameSync(tmp, this.file);
      return result;
    });
    // The chain must not stay rejected: one failed bind would otherwise poison every later one.
    this.chain = next.catch(() => undefined);
    return next;
  }
}

/** `actor:probe` → `probe`. The identity is what the token proved; nothing here trusts a body. */
function actorOf(identity: string): string {
  const bare = typeof identity === 'string' && identity.startsWith('actor:')
    ? identity.slice('actor:'.length)
    : '';
  return assertSlotActor(bare);
}

/** A version string, bounded so it is safe in the declaration key and in a filename-free store. */
function assertVersion(version: string): string {
  if (typeof version !== 'string' || !/^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$/.test(version)) {
    throw new SlotRefused(`${JSON.stringify(String(version))} is not an actor version`);
  }
  return version;
}

/** Slots as they are stored: named, deduplicated, in declaration order, descriptions kept. */
function normaliseSlots(slots: readonly SlotSpec[]): SlotSpec[] {
  const out: SlotSpec[] = [];
  const seen = new Set<string>();
  for (const raw of slots ?? []) {
    const name = assertSlotName((raw as SlotSpec)?.name);
    if (seen.has(name)) continue;
    seen.add(name);
    const description = typeof raw.description === 'string' ? raw.description.trim() : '';
    out.push({ name, ...(description ? { description } : {}) });
  }
  return out;
}

/** Which ledger line a store refusal is. Unknown failures are `forbidden` rather than `resolved` —
 *  the one outcome this must never record by accident is the one that says bytes were handed over. */
function outcomeOf(err: unknown): ResolutionOutcome {
  if (err instanceof SecretRevoked) return 'revoked';
  if (err instanceof NoSuchSecret) return 'missing';
  if (err instanceof SecretForbidden) return 'forbidden';
  return 'forbidden';
}

let shared: SlotStore | null = null;

/**
 * The process-wide slot store. Built on first use for the reason `secretStore()` is: a fresh
 * appliance has no store directory until somebody writes into it, and reading a declaration must
 * not be the act that mints key material.
 */
export function slotStore(secrets: SecretStore = secretStore()): SlotStore {
  if (!shared) shared = new SlotStore(secrets);
  return shared;
}
