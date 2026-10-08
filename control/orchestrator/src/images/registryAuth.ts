/**
 * registryAuth.ts — the credential the orchestrator talks to this install's registry with.
 *
 * WITH AUTH ON, ZOT PERMITS NO ANONYMOUS READ. `docker-compose.yml`'s `registry-config` renders an
 * `accessControl` in which every repository tree has `"defaultPolicy": []` and the three generated
 * accounts are named explicitly. So it is not only a push that needs a credential: `_catalog`,
 * `tags/list`, a manifest GET and zot's GraphQL search are all 401 without one — and every one of
 * those failures is SWALLOWED here by design, because a tag whose manifest cannot be read is still
 * a tag. The symptom is therefore not an error: it is an Images page that says the registry holds
 * nothing, and an `inuse-` reconciler that quietly tags nothing while retention is armed.
 *
 * TWO ACCOUNTS FOR WRITES, CHOSEN BY REPOSITORY, BECAUSE THE SPLIT IS THE SECURITY PROPERTY.
 * `push-actors` may create in `actors/**` and `push-runtimes` in `kontra-runtimes/**`, and neither
 * may write the other — that is what stops an actor build from replacing the base image every other
 * actor is layered on. One client credential cannot cover both namespaces, so the repository
 * decides, exactly as the rendered policy does.
 *
 * THE TWIN OF `cli/registryauth.go`. The Go CLI reads with the same precedence and writes with the
 * same two accounts; if one of these changes the other has to.
 */

/** `pull` first: it may read and nothing else. The push accounts carry read over `**` as a fallback. */
const READ_ORDER = [
  ['KONTRA_REGISTRY_PULL_USER', 'KONTRA_REGISTRY_PULL_PASSWORD', 'pull'],
  ['KONTRA_REGISTRY_PUSH_ACTORS_USER', 'KONTRA_REGISTRY_PUSH_ACTORS_PASSWORD', 'push-actors'],
  ['KONTRA_REGISTRY_PUSH_RUNTIMES_USER', 'KONTRA_REGISTRY_PUSH_RUNTIMES_PASSWORD', 'push-runtimes'],
] as const;

const RUNTIMES_NAMESPACE = 'kontra-runtimes/';

function basic(user: string, password: string): Record<string, string> {
  return { authorization: `Basic ${Buffer.from(`${user}:${password}`).toString('base64')}` };
}

function credential(
  userVar: string,
  passwordVar: string,
  defaultUser: string,
  env: NodeJS.ProcessEnv
): Record<string, string> | null {
  const password = (env[passwordVar] ?? '').trim();
  if (!password) return null;
  const user = (env[userVar] ?? '').trim() || defaultUser;
  return basic(user, password);
}

/**
 * Headers for READING the registry. Empty when no account is configured, which is the quickstart's
 * default and an anonymous registry — so this is additive and never the reason a read fails.
 */
export function registryReadAuth(env: NodeJS.ProcessEnv = process.env): Record<string, string> {
  for (const [userVar, passwordVar, defaultUser] of READ_ORDER) {
    const got = credential(userVar, passwordVar, defaultUser, env);
    if (got) return got;
  }
  return {};
}

/**
 * Headers for WRITING one repository — a tag PUT or an untag DELETE. The namespace picks the
 * account, because the policy that admits the write is per-namespace.
 */
export function registryWriteAuth(
  repo: string,
  env: NodeJS.ProcessEnv = process.env
): Record<string, string> {
  const [userVar, passwordVar, defaultUser] = repo.startsWith(RUNTIMES_NAMESPACE)
    ? (['KONTRA_REGISTRY_PUSH_RUNTIMES_USER', 'KONTRA_REGISTRY_PUSH_RUNTIMES_PASSWORD', 'push-runtimes'] as const)
    : (['KONTRA_REGISTRY_PUSH_ACTORS_USER', 'KONTRA_REGISTRY_PUSH_ACTORS_PASSWORD', 'push-actors'] as const);
  return credential(userVar, passwordVar, defaultUser, env) ?? {};
}
