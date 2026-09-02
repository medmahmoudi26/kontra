#!/usr/bin/env bash
# Provision a NEW kontra control droplet, from this machine, on DigitalOcean.
#
# WHAT THIS IS. `install.sh` installs kontra onto a machine you already have; `fleet.ts` creates
# Machines that call home to a controller that already exists. Neither creates a controller, and
# both controllers built so far were made by hand. This is that missing step, written down.
#
# WHAT IT DELIBERATELY DOES NOT DO — each of these cost a real debugging session:
#
#   * It never copies `.env`. It never copies `.kontra/config.yaml`. `cli/internal/config/config.go:envFor` maps
#     EVERY variable the stack needs out of config.yaml, so a fresh controller needs no .env at
#     all — and the .env on the dev host carries `KONTRA_DUCKLAKE_PG_HOST=safedeps-postgres`, a
#     container from an unrelated stack. That value crash-loops the materializer on EAI_AGAIN,
#     and since the materializer is the only worker polling `kontra-datasets`, `resolveBatch`
#     sits *Scheduled* forever. The run hangs with no error in any surface.
#
#   * It uses `kontra infra up`, never `docker compose up -d`. Only the CLI injects KONTRA_BIND
#     and KONTRA_REDIS_BIND from `controller:`. Plain compose gives a control plane that is safe
#     (loopback) but unfleetable: no Machine can reach a loopback Redis.
#
#   * It ships the tree with `git archive HEAD`, not rsync. rsync of the working tree drags in
#     .venv, node_modules and build output (819 MB vs 4.2 MB), and re-running it silently reverts
#     the two machine-specific files it must never touch. git archive carries tracked files only,
#     so .kontra/ and .env are excluded by construction rather than by a flag someone can forget.
#
#   * It BUILDS the orchestrator image on the target. `VITE_KONTRA_EXPLORE_TOKEN` is a compose
#     BUILD arg (docker-compose.yml:628), so a `docker save` from the dev host would bake THIS
#     installation's explore token into the client's SPA bundle. That is both a credential leak
#     and a 401 the moment their server mints its own. The 8 GB default size is what makes
#     building on the box affordable — a 2 GB box cannot do it (see --size).
#
#   * It generates a FRESH SSH keypair for the fleet. The dev host reaches its Machines with
#     /root/.ssh/axiom_rsa; handing that key to another installation means one compromised
#     controller owns every fleet on the account.
#
#   * It puts the controller in its OWN VPC. Redis has no `requirepass` anywhere in the compose
#     file and binds the controller's VPC address, so every droplet sharing a VPC can read and
#     write the state store unauthenticated — plus Temporal 7233, S3 8333, the API 8088 and, since
#     ADR 0036, the unauthenticated OCI registry on 5000 that every Machine pulls its Bundle from.
#     A DO VPC is flat. Two installations in `default-sfo3` are one installation with extra steps.
#
# USAGE
#   scripts/provision-controller.sh --name <slug> [--dry-run]
#     [--region sfo3] [--size s-4vcpu-8gb] [--image ubuntu-24-04-x64]
#
#   --dry-run prints every mutating call and creates nothing. Run it first. This script bills.
set -euo pipefail
cd "$(dirname "$0")/.."
REPO_ROOT="$PWD"

NAME=""; REGION="sfo3"; SIZE="s-4vcpu-8gb"; IMAGE="ubuntu-24-04-x64"; DRY=0
# /root/kontra-local, not /opt or ~/kontra: `KONTRA_CHECKOUT:-/root/kontra-local` is the compose
# default, and a checkout anywhere else needs a variable set to say so. Matching the default is
# one fewer thing that can be wrong.
REMOTE_CHECKOUT="/root/kontra-local"

while [ $# -gt 0 ]; do
  case "$1" in
    --name)   NAME="$2"; shift 2 ;;
    --region) REGION="$2"; shift 2 ;;
    --size)   SIZE="$2"; shift 2 ;;
    --image)  IMAGE="$2"; shift 2 ;;
    --dry-run) DRY=1; shift ;;
    -h|--help) sed -n '1,45p' "$0"; exit 0 ;;
    *) echo "unknown flag: $1" >&2; exit 2 ;;
  esac
done
[ -n "$NAME" ] || { echo "error: --name <slug> is required (names the droplet, its VPC and its ssh key)" >&2; exit 2; }
case "$NAME" in *[!a-z0-9-]*) echo "error: --name must be lowercase alphanumeric + dashes" >&2; exit 2 ;; esac

DROPLET="kontra-ctl-$NAME"
VPC_NAME="kontra-$NAME-$REGION"
KEY_NAME="kontra-$NAME-fleet"
KEY_PATH="$HOME/.ssh/kontra-$NAME-fleet"

say()  { printf '\n\033[1m==> %s\033[0m\n' "$*"; }
run()  { if [ "$DRY" = 1 ]; then printf '  [dry-run] %s\n' "$*"; else eval "$@"; fi; }

# --- preflight, all of it at once -------------------------------------------------------------
say "preflight"
missing=()
command -v doctl >/dev/null || missing+=("doctl — https://docs.digitalocean.com/reference/doctl/how-to/install/")
command -v ssh   >/dev/null || missing+=("ssh")
command -v git   >/dev/null || missing+=("git")
if [ ${#missing[@]} -gt 0 ]; then printf 'missing:\n'; printf '  %s\n' "${missing[@]}"; exit 1; fi

# The token comes from THIS installation's config, not from a variable someone exported once.
if [ -z "${DIGITALOCEAN_TOKEN:-}" ] && [ -f "$REPO_ROOT/.kontra/config.yaml" ]; then
  DIGITALOCEAN_TOKEN="$(sed -n 's/^ *digitalocean_token: *"\(.*\)"$/\1/p' "$REPO_ROOT/.kontra/config.yaml" | head -1)"
fi
[ -n "${DIGITALOCEAN_TOKEN:-}" ] || { echo "error: no DIGITALOCEAN_TOKEN, and none in .kontra/config.yaml" >&2; exit 1; }
export DIGITALOCEAN_ACCESS_TOKEN="$DIGITALOCEAN_TOKEN"
doctl account get --format Email,Status >/dev/null || { echo "error: the DO token is not usable" >&2; exit 1; }
echo "  doctl ok, region $REGION, size $SIZE, image $IMAGE"

# A dirty tree ships uncommitted work — or, worse, ships HEAD while you believe it shipped your
# edits. git archive reads HEAD and nothing else, so say so before four minutes of install.
if ! git diff-index --quiet HEAD -- 2>/dev/null; then
  echo "  WARNING: working tree is dirty. git archive ships HEAD ($(git rev-parse --short HEAD)), not your edits."
  [ "$DRY" = 1 ] || { printf '  continue? [y/N] '; read -r a; [ "$a" = y ] || exit 1; }
fi

# --- 1. a dedicated VPC ------------------------------------------------------------------------
say "VPC $VPC_NAME"
VPC_ID="$(doctl vpcs list --format Name,ID --no-header 2>/dev/null | awk -v n="$VPC_NAME" '$1==n{print $2}')"
if [ -n "$VPC_ID" ]; then
  echo "  exists: $VPC_ID"
else
  run "doctl vpcs create --name '$VPC_NAME' --region '$REGION'"  # no --wait: doctl vpcs create has no such flag, and the API returns the created VPC
  [ "$DRY" = 1 ] || VPC_ID="$(doctl vpcs list --format Name,ID --no-header | awk -v n="$VPC_NAME" '$1==n{print $2}')"
  echo "  created: ${VPC_ID:-<dry-run>}"
fi

# --- 2. a fleet keypair that is NOT the dev host's ----------------------------------------------
say "fleet ssh key $KEY_NAME"
if [ -f "$KEY_PATH" ]; then
  echo "  local key exists: $KEY_PATH"
else
  run "ssh-keygen -t ed25519 -N '' -C '$KEY_NAME' -f '$KEY_PATH'"
fi
KEY_ID="$(doctl compute ssh-key list --format Name,ID --no-header 2>/dev/null | awk -v n="$KEY_NAME" '$1==n{print $2}')"
if [ -n "$KEY_ID" ]; then
  echo "  registered on DO: $KEY_ID"
else
  run "doctl compute ssh-key import '$KEY_NAME' --public-key-file '$KEY_PATH.pub'"
  [ "$DRY" = 1 ] || KEY_ID="$(doctl compute ssh-key list --format Name,ID --no-header | awk -v n="$KEY_NAME" '$1==n{print $2}')"
  echo "  imported: ${KEY_ID:-<dry-run>}"
fi

# --- 3. the droplet ------------------------------------------------------------------------------
say "droplet $DROPLET"
PUBLIC_IP="$(doctl compute droplet list --format Name,PublicIPv4 --no-header 2>/dev/null | awk -v n="$DROPLET" '$1==n{print $2}')"
if [ -n "$PUBLIC_IP" ]; then
  echo "  exists at $PUBLIC_IP — reusing (delete it first for a clean build)"
else
  run "doctl compute droplet create '$DROPLET' \
        --region '$REGION' --size '$SIZE' --image '$IMAGE' \
        --vpc-uuid '$VPC_ID' --ssh-keys '$KEY_ID' \
        --tag-names kontra-controller --wait"
fi
if [ "$DRY" = 1 ]; then echo; echo "[dry-run] stopping before any remote step."; exit 0; fi
PUBLIC_IP="$(doctl compute droplet list --format Name,PublicIPv4 --no-header | awk -v n="$DROPLET" '$1==n{print $2}')"
PRIVATE_IP="$(doctl compute droplet list --format Name,PrivateIPv4 --no-header | awk -v n="$DROPLET" '$1==n{print $2}')"
[ -n "$PRIVATE_IP" ] || { echo "error: droplet has no private IP — is it in the VPC?" >&2; exit 1; }
echo "  public $PUBLIC_IP   private $PRIVATE_IP"

# DO recycles VPC and public addresses aggressively. A kept known_hosts entry from a previous
# droplet refuses the new one with `Host key verification failed`, which reads as unreachable.
ssh-keygen -R "$PUBLIC_IP" >/dev/null 2>&1 || true
# -i, EXPLICITLY. The droplet was created with ONLY this installation's own fleet key in
# authorized_keys — that is the whole point of generating one — so relying on the agent or on
# ~/.ssh/id_* means `Permission denied (publickey)` on every one of the 60 retries below, which
# reads as "the box never booted" rather than "we offered the wrong key". IdentitiesOnly stops
# ssh burning its attempt budget on every other key the agent holds first.
SSH="ssh -i $KEY_PATH -o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new -o ConnectTimeout=10 root@$PUBLIC_IP"

say "waiting for sshd"
for i in $(seq 1 60); do
  $SSH true 2>/dev/null && break
  [ "$i" = 60 ] && { echo "error: no ssh after 5 minutes" >&2; exit 1; }
  sleep 5
done
echo "  up"

# cloud-init runs unattended-upgrades on first boot and HOLDS THE APT LOCK. install.sh's very
# first act is apt-get; starting before this returns fails on a lock, not on anything real.
say "waiting for cloud-init (holds the apt lock)"
$SSH 'cloud-init status --wait >/dev/null 2>&1 || true; cloud-init status || true'

# --- 4. swap, sized off actual RAM ----------------------------------------------------------------
say "swap"
$SSH 'bash -s' <<'REMOTE'
set -e
if [ -f /swapfile ]; then echo "  /swapfile exists"; exit 0; fi
mem_kb=$(awk '/MemTotal/{print $2}' /proc/meminfo)
if [ "$mem_kb" -lt 8000000 ]; then
  fallocate -l 2G /swapfile && chmod 600 /swapfile && mkswap -q /swapfile && swapon /swapfile
  echo '/swapfile none swap sw 0 0' >> /etc/fstab
  echo "  2G swap added (RAM $((mem_kb/1024)) MiB)"
else
  echo "  RAM $((mem_kb/1024)) MiB — no swap needed"
fi
REMOTE

# --- 5. the tree ------------------------------------------------------------------------------
say "shipping HEAD ($(git rev-parse --short HEAD)) -> $REMOTE_CHECKOUT"
$SSH "mkdir -p $REMOTE_CHECKOUT"
git archive --format=tar HEAD | gzip -1 | $SSH "tar xzf - -C $REMOTE_CHECKOUT"
echo "  done"

# --- 6. install.sh ----------------------------------------------------------------------------
say "install.sh (docker, Go 1.26.4, duckdb, buf, the CLI — several minutes)"
$SSH "cd $REMOTE_CHECKOUT && KONTRA_BOOTSTRAP=yes ./install.sh"

# --- 7. config.yaml — the ONLY configuration this installation gets ------------------------------
#
# install.sh ended in `kontra init`, which wrote the template and MINTED `tokens.state`. Every
# other value is blank, and blank means two opposite things in that file: `run: ""` is OPEN,
# `state: ""` fails closed. Fill the rest here, on the box, with values generated on the box.
say "config.yaml"
$SSH "PRIVATE_IP='$PRIVATE_IP' VPC_ID='$VPC_ID' REGION='$REGION' KEY_ID='$KEY_ID' \
      DO_TOKEN='$DIGITALOCEAN_TOKEN' CHECKOUT='$REMOTE_CHECKOUT' bash -s" <<'REMOTE'
set -euo pipefail
# WHERE `kontra init` ACTUALLY WROTE. `cli/internal/config/config.go:kontraRoot` resolves KONTRA_HOME, else a
# `.kontra/` that ALREADY EXISTS in the checkout you are standing in, else ~/.kontra. On a fresh
# box the second never matches, so init writes ~/.kontra — and this script assumed the checkout
# path purely because the dev host happens to have one. Probe in the CLI's own order rather than
# pick a side, so the script works on both.
cfg="$CHECKOUT/.kontra/config.yaml"
[ -f "$cfg" ] || cfg="$HOME/.kontra/config.yaml"
[ -f "$cfg" ] || { echo "error: kontra init wrote neither $CHECKOUT/.kontra/ nor $HOME/.kontra/" >&2; exit 1; }
echo "  config: $cfg"

# base64url, matching cli/internal/config/config.go:mintToken (256 bits, RawURLEncoding).
mint() { openssl rand -base64 32 | tr '+/' '-_' | tr -d '=\n'; }
PANEL="$(mint)"; PASSPHRASE="$(mint)"; PGPASS="$(mint)"

sed -i \
  -e "s|^controller: \"\"|controller: \"$PRIVATE_IP\"|" \
  -e "s|^  digitalocean_token: \"\"|  digitalocean_token: \"$DO_TOKEN\"|" \
  -e "s|^  pulumi_passphrase: \"\"|  pulumi_passphrase: \"$PASSPHRASE\"|" \
  -e "s|^  ssh_key: \"\"|  ssh_key: \"/root/.ssh/kontra_fleet\"|" \
  -e "s|^  panel: \"\"|  panel: \"$PANEL\"|" \
  -e "s|^  ducklake_password: \"\"|  ducklake_password: \"$PGPASS\"|" \
  "$cfg"

# region/vpc/ssh_key_ids are in the Config struct but NOT in CONFIG_TEMPLATE, so they have to be
# appended rather than substituted. Anchored on ssh_key so they land inside `fleet:`.
grep -q '^  region:' "$cfg" || sed -i "/^  ssh_key: /a\\
  # Where this installation's fleets land. Its OWN vpc: a DO VPC is flat and redis has no\\
  # requirepass, so sharing one with another controller shares the state store.\\
  region: \"$REGION\"\\
  vpc: \"$VPC_ID\"\\
  ssh_key_ids: \"$KEY_ID\"" "$cfg"

# `tokens.explore` stays BLANK on purpose — it falls back to the state token, and the SPA bakes
# VITE_KONTRA_EXPLORE_TOKEN at image build time. Minting a second one here would hand the bundle
# a token the server does not expect, and the Datasets console 401s into an empty grid.
echo "  controller=$PRIVATE_IP vpc=$VPC_ID region=$REGION ssh_key_ids=$KEY_ID"
echo "  minted: pulumi_passphrase, tokens.panel, data.ducklake_password (state was minted by init)"
grep -n '^controller:\|^  region:\|^  vpc:\|^  ssh_key' "$cfg"
REMOTE

# The fleet private key, to the path config.yaml now names. Sent last so a failed run above never
# leaves a key on a box with nothing else on it.
say "fleet private key -> /root/.ssh/kontra_fleet"
$SSH 'install -m 600 /dev/stdin /root/.ssh/kontra_fleet' < "$KEY_PATH"

# --- 8. up ---------------------------------------------------------------------------------------
# Where the config actually landed, asked rather than assumed — the summary below used to name
# the checkout path unconditionally and sent an operator to a file that is not there.
CFG_PATH="$($SSH "test -f $REMOTE_CHECKOUT/.kontra/config.yaml && echo $REMOTE_CHECKOUT/.kontra/config.yaml || echo \$HOME/.kontra/config.yaml")"

say "kontra infra up (builds the orchestrator image on the box — several minutes)"
$SSH "cd $REMOTE_CHECKOUT && kontra infra up"

say "verify"
$SSH "cd $REMOTE_CHECKOUT && kontra doctor" || true

cat <<SUMMARY

  droplet    $DROPLET
  public     $PUBLIC_IP
  private    $PRIVATE_IP   (controller: — what fleet Machines dial)
  vpc        $VPC_NAME / $VPC_ID
  fleet key  $KEY_PATH  (DO id $KEY_ID)
  checkout   $REMOTE_CHECKOUT
  config     $CFG_PATH
  ssh        ssh -i $KEY_PATH root@$PUBLIC_IP

  STILL YOURS TO DECIDE:
    * tokens.run is "" = OPEN. Anyone who reaches the API can start a workflow that provisions
      machines. Set it in $CFG_PATH, then \`kontra infra up\` again.
    * With KONTRA_BIND at the VPC address, localhost:7233/8088/8333 are CLOSED on that box. Any
      host-side CLI call must name $PRIVATE_IP (cli/api.go defaults to localhost, and
      cli/infra.go has a hardcoded localhost:8333 that always reports seaweed DOWN).
    * A FLEET NEEDS A REGISTRY ON 5000 AT $PRIVATE_IP (ADR 0036). Every Machine fetches its
      Bundle's layer from it, so a control plane without one cannot run a fleet run — and a
      registry reachable only on loopback fails at placement, on every Machine at once.
    * The box holds a DO token with FULL control of your account. Scope it down if the client
      gets shell access.
    * Verify the firewall by scanning FROM ANOTHER HOST in bash, never by reading ufw status:
      docker's DOCKER-USER chain is consulted before ufw's INPUT. Include a positive control.
SUMMARY
