#!/usr/bin/env bash
# The compose team gate on a clean VPS (DM4-14, DM4-15, DM4-31; the M4 plan's gate run C, the gate item "team gate up
# on a clean VPS"). It provisions a throwaway DigitalOcean droplet, names it under kontra.run through Cloudflare's API,
# ships `git archive HEAD` to it, brings compose.yaml up with deploy/compose/kontra.vps.yaml (built-in ACME), checks it
# from this host, enrolls a node from a team member's clean machine (the kontra-demo image, scripts/demo-clean-laptop.sh)
# and hires through it with an API key, A2A and MCP. Then it destroys everything it made, and checks that it did.
#
#   1. pre-flight (read-only): doctl's account; the DigitalOcean SSH key's MD5 fingerprint equals this host's key's
#      (ssh-keygen -E md5 -lf <key>.pub); the size exists (else its -amd variant); no droplet tagged kontra-m4-gaterun;
#      the zone exists and has no record named the name nor with the run's comment. A listing that fails refuses the run;
#   2. doctl compute droplet create kontra-m4-teamgate-<epoch> (fra1, s-2vcpu-4gb, ubuntu-24-04-x64, tags
#      kontra-m4-gaterun and ttl-<epoch + 4 h>, test/vps/cloud-init.yaml), then ssh until cloud-init is done;
#   3. an A record for the name (ttl 60, not proxied, comment "M4 VPS gate run, delete after"), then wait until
#      1.1.1.1 resolves it to the droplet (DNS over HTTPS), before anything asks Let's Encrypt;
#   4. git archive HEAD | ssh … tar -x -C /opt/kontra (refused when /opt/kontra/.env exists: the host is configured
#      already); kontra.vps.yaml with the name, three random passwords in .env (KONTRA_PORT=443), a new signing key
#      owned by 65532; docker compose up -d --build --wait (20 min at most);
#   5. from this host: GET /healthz; the certificate's issuer is Let's Encrypt; of 22, 80, 443, 5432, 7233, 8443 and
#      9090 only 22 and 443 answer; over ssh, an API key and a token for hello (kontra-gate enroll); a team member's
#      clean machine (a kontra-demo container on this host) runs a2a server --exec 'echo Hello World' and kontra connect
#      --enroll; then SendMessage "hi" to /agents/hello with the key answers Hello World, and an MCP tools/call search on
#      /mcp with the key lists hello; docker compose ps, the image digests, kontra-gate version and the timings are
#      recorded.
# No secret is ever on a command line (every local user reads /proc/<pid>/cmdline): curl reads the Cloudflare token on
# its stdin and the gate's key from a 0600 file in the run's directory (mktemp -d, 0700), a2a reads the key from
# A2ACLI_AUTH, and the team member's node reads its token from a file.
#
# Teardown, in a trap on EXIT, INT and TERM (printed before anything is created): docker rm -f kontra-teammate-<epoch>;
# docker image rm kontra-demo:<sha> when this run built it; then DNS before the droplet, so that the name never points
# at an address DigitalOcean may give to someone else: DELETE the run's record by id, then every record with the run's
# comment; only once that succeeded, doctl compute droplet delete -f <id> (else the droplet is kept, and the run fails
# naming --teardown-only); ssh-keygen -R <ip>; rm -rf the run's temporary directory. Then it checks that doctl lists no
# droplet tagged kontra-m4-gaterun, that Cloudflare lists no record named the name or with the run's comment, and that
# no kontra-teammate container is left, and records the droplet's lifetime and cost. A listing that fails is "could not
# verify" and fails the run: never "nothing left".
# A crashed run is reaped with --teardown-only, in the same order: every record with the run's comment, then, once that
# succeeded, every droplet tagged kontra-m4-gaterun (its ttl tag says when it was due). A record named the name whose
# comment is another is not a run's: it is reported and left, and the reap fails. Droplets are only ever listed by that
# tag; a droplet named main-droplet is never deleted. The name must be m4-teamgate<…> in the zone.
#
# Usage: scripts/team-gate-vps.sh [--dry-run | --teardown-only | --host ADDR] [--published --expect <commit>] [--record <file>]
#   --dry-run        the pre-flight and the plan, printed; nothing created
#   --teardown-only  reap what a run left (DNS records by comment, then droplets by tag)
#   --host ADDR      steps 4 and 5 on a host you already have (root over ssh with the key; the name resolves to it):
#                    nothing is created or deleted on DigitalOcean or Cloudflare, and the gate stays up
#   --published      the team member installs kontra with `go install kontra.sh/kontra/cmd/kontra@<--expect>`
#                    instead of from /src (after the commit is on GitHub)
#   --record FILE    the run's record (default ./team-gate-vps-<UTC time>.log); keys and tokens are never written
#   Rehearsal of --host against a local compose gate (test/vps, KONTRA_KIT_TESTS=1), never for the gate run:
#   --network NET    the team member's container joins this docker network instead of the default bridge
#   --ca-file PEM    the team member and this host's checks also trust this CA, whose certificate the gate must serve
#                    instead of Let's Encrypt's
#   --connect-to A:P this host's checks reach the name's port 443 at A:P (curl --connect-to); the port check, which
#                    probes ADDR, and a2a send are skipped
# Settings (environment): CLOUDFLARE_API_TOKEN (required but with --host; DNS edit on the zone), KONTRA_VPS_NAME
# (m4-teamgate.kontra.run), KONTRA_VPS_ZONE (kontra.run), KONTRA_VPS_REGION (fra1), KONTRA_VPS_SIZE (s-2vcpu-4gb),
# KONTRA_VPS_DO_KEY (58735991), KONTRA_VPS_SSH_KEY (~/.ssh/kontra-client01-fleet), KONTRA_VPS_TTL (14400 s).
# Prerequisites: doctl (authenticated), docker (a running daemon), curl, jq, ssh, ssh-keygen, openssl, git, and
# CLOUDFLARE_API_TOKEN; run from the repository. --teardown-only needs only doctl, curl, jq and the token; --host needs
# neither doctl nor the token. Exit 2 when one is missing.
set -euo pipefail

usage() { sed -n '/^# Usage:/,/^# Prerequisites:/p' "$0" | sed 's/^# \{0,1\}//' >&2; exit 2; }

mode=run modes=0 published=0 expect="" record="" host="" network="" ca_file="" connect_to=""
while [ $# -gt 0 ]; do
  case "$1" in
    --dry-run) mode=dry; modes=$((modes + 1)) ;;
    --teardown-only) mode=reap; modes=$((modes + 1)) ;;
    --host) host=${2:?--host needs an address}; mode=host; modes=$((modes + 1)); shift ;;
    --published) published=1 ;;
    --expect) expect=${2:?--expect needs a commit}; shift ;;
    --record) record=${2:?--record needs a file}; shift ;;
    --network) network=${2:?--network needs a docker network}; shift ;;
    --ca-file) ca_file=${2:?--ca-file needs a PEM file}; shift ;;
    --connect-to) connect_to=${2:?--connect-to needs ADDR:PORT}; shift ;;
    -h | --help) usage ;;
    *) echo "team-gate-vps: unknown argument $1" >&2; usage ;;
  esac
  shift
done
if [ "$modes" -gt 1 ]; then
  echo "team-gate-vps: --dry-run, --teardown-only and --host exclude one another" >&2
  exit 2
fi
if [ "$published" = 1 ] && ! [[ "$expect" =~ ^[0-9a-f]{12,40}$ ]]; then
  echo "team-gate-vps: --published needs --expect <the merge commit, 12 to 40 hex digits>" >&2
  exit 2
fi
if [ "$mode" != host ] && [ -n "$network$ca_file$connect_to" ]; then
  echo "team-gate-vps: --network, --ca-file and --connect-to rehearse --host against a local gate: they need --host" >&2
  exit 2
fi
if [ "$mode" = host ] && ! [[ "$host" =~ ^[A-Za-z0-9.:-]+$ ]]; then
  echo "team-gate-vps: --host $host is not an address" >&2
  exit 2
fi
if [ -n "$connect_to" ] && ! [[ "$connect_to" =~ ^[A-Za-z0-9.:-]+:[0-9]+$ ]]; then
  echo "team-gate-vps: --connect-to $connect_to is not ADDR:PORT" >&2
  exit 2
fi

name=${KONTRA_VPS_NAME:-m4-teamgate.kontra.run}
zone=${KONTRA_VPS_ZONE:-kontra.run}
region=${KONTRA_VPS_REGION:-fra1}
size=${KONTRA_VPS_SIZE:-s-2vcpu-4gb}
do_key=${KONTRA_VPS_DO_KEY:-58735991}
ssh_key=${KONTRA_VPS_SSH_KEY:-$HOME/.ssh/kontra-client01-fleet}
life=${KONTRA_VPS_TTL:-14400}
tag=kontra-m4-gaterun
comment="M4 VPS gate run, delete after"
cf=https://api.cloudflare.com/client/v4
os_image=ubuntu-24-04-x64
remote=/opt/kontra

# The name is the one record a run creates and a reap deletes: never another gate's (gateway.kontra.run is staging's).
if ! [[ "$name" =~ ^[a-z0-9.-]+$ ]] || { [ "$mode" != host ] && { ! [[ "$name" =~ ^m4-teamgate[a-z0-9-]*\. ]] || [[ "$name" != *".$zone" ]]; }; }; then
  echo "team-gate-vps: KONTRA_VPS_NAME $name: want m4-teamgate<…>.$zone, the name a run creates and a reap deletes" >&2
  exit 2
fi

# Prerequisites, all checked before anything is created.
case "$mode" in
  reap) need=(doctl curl jq) ;;
  host) need=(curl jq docker ssh ssh-keygen openssl git) ;;
  *) need=(doctl curl jq docker ssh ssh-keygen openssl git) ;;
esac
missing=()
for tool in "${need[@]}"; do
  command -v "$tool" > /dev/null 2>&1 || missing+=("$tool")
done
if [ "$mode" != host ] && [ -z "${CLOUDFLARE_API_TOKEN:-}" ]; then
  missing+=("CLOUDFLARE_API_TOKEN (a Cloudflare token that edits DNS in $zone)")
fi
if [ "$mode" != reap ]; then
  if [ ! -r "$ssh_key" ] || [ ! -r "$ssh_key.pub" ]; then
    missing+=("the SSH key $ssh_key and $ssh_key.pub (KONTRA_VPS_SSH_KEY)")
  fi
fi
if [ -n "$ca_file" ] && [ ! -r "$ca_file" ]; then
  missing+=("--ca-file $ca_file (not readable)")
fi
if [ ${#missing[@]} -gt 0 ]; then
  printf 'team-gate-vps: missing prerequisites:\n' >&2
  printf '  %s\n' "${missing[@]}" >&2
  exit 2
fi
if [ "$mode" != host ] && ! doctl account get --format Email,Status --no-header > /dev/null 2>&1; then
  echo "team-gate-vps: doctl is not authenticated (doctl account get failed): doctl auth init" >&2
  exit 2
fi
if [ "$mode" != reap ] && ! docker info > /dev/null 2>&1; then
  echo "team-gate-vps: docker cannot reach its daemon (docker info failed); the team member's machine is a container" >&2
  exit 2
fi

# cfapi METHOD PATH [JSON]: Cloudflare's API, answering its JSON; fails unless success is true. curl reads the token on
# its stdin (-H @-), never from its command line, and it is never printed.
cfapi() {
  local out data=()
  [ $# -lt 3 ] || data=(-H 'Content-Type: application/json' --data "$3")
  if ! out=$(curl -sS --max-time 30 -X "$1" -H @- "${data[@]}" "$cf$2" <<< "Authorization: Bearer $CLOUDFLARE_API_TOKEN"); then
    echo "team-gate-vps: Cloudflare $1 $2: no answer" >&2
    return 1
  fi
  if [ "$(jq -r '.success' <<< "$out" 2> /dev/null)" != true ]; then
    echo "team-gate-vps: Cloudflare $1 $2: $(jq -c '.errors' <<< "$out" 2> /dev/null || head -c 300 <<< "$out")" >&2
    return 1
  fi
  printf '%s\n' "$out"
}

# records prints the zone's DNS records named $name and those with the run's comment, one per line, tab-separated:
# "ours" or "foreign" (whether the comment is the run's), the id, the type, the name and the content. It fails, saying
# so, when Cloudflare does not list them all: an empty answer always means none.
records() {
  local enc by_name by_comment
  enc=$(jq -rn --arg c "$comment" '$c|@uri')
  by_name=$(cfapi GET "/zones/$zone_id/dns_records?name=$name&per_page=100") || return 1
  by_comment=$(cfapi GET "/zones/$zone_id/dns_records?comment.exact=$enc&per_page=100") || return 1
  printf '%s\n%s\n' "$by_name" "$by_comment" | jq -rn --arg c "$comment" '
    [inputs | if (.result | type) != "array" or (.result_info.total_pages // 1) > 1 then error("not one page of records")
      else .result[] end]
    | unique_by(.id)[] | [(if .comment == $c then "ours" else "foreign" end), .id, .type, .name, .content] | @tsv' || {
    echo "team-gate-vps: Cloudflare's answer is not one page of DNS records" >&2
    return 1
  }
}

# tagged prints "ID NAME TAGS" for every droplet tagged $tag; droplets are never listed any other way. It fails, saying
# so, when doctl cannot list them: an empty answer always means none.
tagged() {
  local out
  if ! out=$(doctl compute droplet list --tag-name "$tag" --format ID,Name,Tags --no-header); then
    echo "team-gate-vps: doctl could not list the droplets tagged $tag" >&2
    return 1
  fi
  [ -z "$out" ] || printf '%s\n' "$out"
}

# delete_droplet ID NAME, refusing main-droplet whatever its tags.
deleted=0
delete_droplet() {
  if [ "$2" = main-droplet ]; then
    echo "team-gate-vps: refusing to delete $2 ($1), tagged $tag or not" >&2
    return 1
  fi
  echo "doctl compute droplet delete -f $1   # $2"
  doctl compute droplet delete -f "$1" || return 1
  deleted=1
}

# reap_records deletes the run's DNS records: $record_id first when it is set, then every record with the run's comment.
# A record named $name with another comment is no run's: it is reported and left. It succeeds only when the listing
# answered, every deletion succeeded and no such record exists; until then no droplet is deleted (a record left
# pointing at a released address would let whoever DigitalOcean gives it to next serve the name, with a certificate).
record_id=""
reap_records() {
  local list kind rid rtype rname content ok=1
  if [ -n "$record_id" ]; then
    echo "DELETE dns_records/$record_id ($name)"
    if cfapi DELETE "/zones/$zone_id/dns_records/$record_id" > /dev/null; then record_id=""; else ok=0; fi
  fi
  if ! list=$(records); then
    echo "team-gate-vps: could not list the DNS records of $zone: the records of a run are unknown"
    return 1
  fi
  while IFS=$'\t' read -r kind rid rtype rname content; do
    [ -n "$rid" ] || continue
    if [ "$kind" = ours ]; then
      echo "DELETE dns_records/$rid ($rtype $rname $content)"
      cfapi DELETE "/zones/$zone_id/dns_records/$rid" > /dev/null || ok=0
    else
      echo "team-gate-vps: refusing to delete $rtype $rname $content ($rid): its comment is not \"$comment\", so no run made it"
      ok=0
    fi
  done <<< "$list"
  [ "$ok" = 1 ]
}

# verify_gone checks that nothing of a run is left: no droplet tagged $tag but main-droplet (after a deletion it waits
# up to a minute while DigitalOcean removes the droplet) and no DNS record named $name or with the run's comment. A
# listing that fails is "could not verify", never "nothing left".
verify_gone() {
  local ok=1 left recs i
  for i in 1 2 3 4 5 6 7; do
    if ! left=$(tagged); then
      left=unknown
      break
    fi
    left=$(awk '$2 != "main-droplet"' <<< "$left")
    { [ -z "$left" ] || [ "$deleted" = 0 ] || [ "$i" = 7 ]; } && break
    sleep 10
  done
  if [ "$left" = unknown ]; then
    echo "team-gate-vps: could not verify that no droplet tagged $tag is left"
    ok=0
  elif [ -n "$left" ]; then
    echo "team-gate-vps: LEFT: droplets tagged $tag:"
    echo "$left"
    ok=0
  fi
  if ! recs=$(records); then
    echo "team-gate-vps: could not verify that no DNS record named $name or commented \"$comment\" is left"
    ok=0
  elif [ -n "$recs" ]; then
    echo "team-gate-vps: LEFT: DNS records named $name or commented \"$comment\":"
    echo "$recs"
    ok=0
  fi
  [ "$ok" = 1 ]
}

if [ "$mode" != host ]; then
  zone_id=$(cfapi GET "/zones?name=$zone" | jq -r '.result[0].id // empty') || exit 1
  [ -n "$zone_id" ] || { echo "team-gate-vps: Cloudflare has no zone $zone for this token" >&2; exit 2; }
fi

if [ "$mode" = reap ]; then
  echo "team-gate-vps: --teardown-only: DNS records commented \"$comment\", then droplets tagged $tag"
  now=$(date +%s)
  rc=0
  if ! reap_records; then
    echo "team-gate-vps: no droplet is deleted while a DNS record of a run may be left: a name must never point at an address DigitalOcean can give to someone else"
    rc=1
  elif ! list=$(tagged); then
    rc=1
  else
    while read -r id dname tags; do
      [ -n "$id" ] || continue
      due=$(tr ',' '\n' <<< "$tags" | sed -n 's/^ttl-\([0-9]*\)$/\1/p' | head -1)
      if [ -n "$due" ] && [ "$due" -lt "$now" ]; then
        echo "  $dname ($id): its ttl passed $(((now - due) / 60)) min ago"
      else
        echo "  $dname ($id): tagged $tag${due:+, due in $(((due - now) / 60)) min}"
      fi
      delete_droplet "$id" "$dname" || rc=1
    done <<< "$list"
  fi
  verify_gone || rc=1
  if [ "$rc" = 0 ]; then
    echo "team-gate-vps: nothing of a run is left"
  else
    echo "team-gate-vps: NOT verified clean (above); run --teardown-only again once doctl and Cloudflare answer" >&2
  fi
  exit "$rc"
fi

# Pre-flight, read-only.
repo=$(git rev-parse --show-toplevel 2> /dev/null) || { echo "team-gate-vps: run it from the repository" >&2; exit 2; }
cd "$repo"
sha=$(git rev-parse --short=12 HEAD)
if [ "$mode" != host ]; then
  do_fp=$(doctl compute ssh-key get "$do_key" --format FingerPrint --no-header | tr -d '[:space:]')
  local_fp=$(ssh-keygen -E md5 -lf "$ssh_key.pub" | awk '{print $2}' | sed 's/^MD5://')
  if [ -z "$do_fp" ] || [ "$do_fp" != "$local_fp" ]; then
    echo "team-gate-vps: DigitalOcean SSH key $do_key has fingerprint '$do_fp', $ssh_key.pub has '$local_fp': refusing" >&2
    exit 2
  fi
  sizes=$(doctl compute size list --format Slug,PriceHourly --no-header)
  has_size() { awk -v s="$1" '$1 == s {f = 1} END {exit !f}' <<< "$sizes"; }
  if ! has_size "$size"; then
    if has_size "$size-amd"; then
      size="$size-amd"
    else
      echo "team-gate-vps: DigitalOcean has no size $size (nor $size-amd)" >&2
      exit 2
    fi
  fi
  price=$(awk -v s="$size" '$1 == s {print $2}' <<< "$sizes")
  if ! left=$(tagged); then
    echo "team-gate-vps: refusing to start: the droplets tagged $tag could not be listed" >&2
    exit 2
  fi
  if [ -n "$left" ]; then
    printf 'team-gate-vps: droplets tagged %s exist (another run, or a crashed one: --teardown-only):\n%s\n' "$tag" "$left" >&2
    exit 2
  fi
  if ! recs=$(records); then
    echo "team-gate-vps: refusing to start: the DNS records of $zone could not be listed" >&2
    exit 2
  fi
  if [ -n "$recs" ]; then
    printf 'team-gate-vps: Cloudflare already has a record named %s or commented "%s" (--teardown-only):\n%s\n' "$name" "$comment" "$recs" >&2
    exit 2
  fi
fi

epoch=$(date +%s)
droplet="kontra-m4-teamgate-$epoch"
teammate="kontra-teammate-$epoch"
if [ "$mode" = host ]; then
  echo "team-gate-vps: $(date -u +%FT%TZ), commit $sha, steps 4 and 5 on root@$host, https://$name"
  echo "teardown, in a trap on EXIT/INT/TERM: docker rm -f $teammate; docker image rm kontra-demo:$sha (if built here);"
  echo "  rm -rf <the run's temporary directory>; then: no $teammate. Nothing on $host is removed: the gate stays up"
  if [ -n "$network$ca_file$connect_to" ]; then
    echo "rehearsal: network ${network:-default bridge}, CA ${ca_file:-system}, $name:443 at ${connect_to:-its DNS address}"
  fi
else
  echo "team-gate-vps: $(date -u +%FT%TZ), commit $sha, $droplet ($region, $size at \$$price/h, $os_image), https://$name"
  echo "teardown, in a trap on EXIT/INT/TERM: docker rm -f $teammate; docker image rm kontra-demo:$sha (if built here);"
  echo "  DNS first: DELETE the A record of $name by id, then every record commented \"$comment\"; once that succeeded,"
  echo "  doctl compute droplet delete -f <id of $droplet> (else it is kept); ssh-keygen -R <ip>;"
  echo "  rm -rf <the run's temporary directory>; then: no droplet tagged $tag, no record for $name, no $teammate"
fi
if [ "$mode" = dry ]; then
  echo "dry run: pre-flight passed; nothing created"
  exit 0
fi

tmp=$(mktemp -d "${TMPDIR:-/tmp}/kontra-vps.XXXXXX")
[ -n "$record" ] || record="$PWD/team-gate-vps-$(date -u +%Y%m%dT%H%M%SZ).log"
exec > >(tee -a "$record") 2>&1
echo "record: $record"
droplet_id="" ip=$host created="" built_image=0
teardown() {
  local rc=$? ok=1 list c
  set +e
  trap - EXIT INT TERM
  echo "team-gate-vps: teardown (exit $rc)"
  docker rm -f "$teammate" > /dev/null 2>&1
  [ "$built_image" = 1 ] && docker image rm "kontra-demo:$sha" > /dev/null 2>&1
  if [ "$mode" = run ]; then
    # DNS before the droplet: while the droplet exists, the address the record names is the run's.
    if reap_records; then
      if [ -z "$droplet_id" ]; then
        if list=$(tagged); then droplet_id=$(awk -v n="$droplet" '$2 == n {print $1}' <<< "$list"); else ok=0; fi
      fi
      if [ -n "$droplet_id" ]; then delete_droplet "$droplet_id" "$droplet" || ok=0; fi
    else
      echo "team-gate-vps: KEPT the droplet $droplet${droplet_id:+ ($droplet_id)}: a DNS record of the run may still name its address; run scripts/team-gate-vps.sh --teardown-only once Cloudflare answers"
      ok=0
    fi
    [ -n "$ip" ] && ssh-keygen -R "$ip" > /dev/null 2>&1
  fi
  rm -rf "$tmp"
  if [ "$mode" = run ]; then verify_gone || ok=0; fi
  if ! c=$(docker ps -aq --filter "name=$teammate"); then
    echo "team-gate-vps: could not verify that no container $teammate is left"
    ok=0
  elif [ -n "$c" ]; then
    echo "team-gate-vps: LEFT: container $teammate"
    ok=0
  fi
  if [ -n "$created" ]; then
    local secs=$(($(date +%s) - created))
    echo "droplet lifetime: $((secs / 60)) min $((secs % 60)) s; cost at most \$$(awk -v p="$price" -v s="$secs" \
      'BEGIN { h = int((s + 3599) / 3600); printf "%.4f (%d h at $%s/h)", h * p, h, p }')"
  fi
  if [ "$ok" = 1 ]; then
    if [ "$mode" = host ]; then echo "team-gate-vps: torn down; the gate on $host stays up"; else echo "team-gate-vps: torn down; nothing left"; fi
  else
    echo "team-gate-vps: teardown NOT verified (above)"
    rc=1
  fi
  exit "$rc"
}
trap teardown EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

ssh_opts=(-i "$ssh_key" -o StrictHostKeyChecking=accept-new -o "UserKnownHostsFile=$tmp/known_hosts" -o ConnectTimeout=10
  -o BatchMode=yes -o ServerAliveInterval=15)
# shellcheck disable=SC2029 # the remote commands are this script's own, expanded here on purpose
sshx() { ssh "${ssh_opts[@]}" "root@$ip" "$@"; }
step() { echo "== $(date -u +%T) $*"; }

if [ "$mode" = run ]; then
  step "2. create $droplet"
  created=$(date +%s)
  out=$(doctl compute droplet create "$droplet" --region "$region" --size "$size" --image "$os_image" --ssh-keys "$do_key" \
    --tag-names "$tag,ttl-$((epoch + life))" --user-data-file "$repo/test/vps/cloud-init.yaml" --wait \
    --format ID,PublicIPv4 --no-header)
  read -r droplet_id ip <<< "$out"
  if ! [[ "$droplet_id" =~ ^[0-9]+$ ]] || ! [[ "$ip" =~ ^[0-9.]+$ ]]; then
    echo "team-gate-vps: doctl answered '$out'"
    [[ "$droplet_id" =~ ^[0-9]+$ ]] || droplet_id="" # the teardown finds it by its name among the tagged
    exit 1
  fi
  echo "droplet $droplet_id at $ip"
  for i in $(seq 60); do
    sshx true 2> /dev/null && break
    [ "$i" = 60 ] && { echo "team-gate-vps: no ssh to $ip within 10 minutes"; exit 1; }
    sleep 10
  done
  # cloud-init exits 2 for "done with recoverable errors" (deprecation notices on 24.04): read its status instead.
  ci=$(sshx cloud-init status --wait 2>&1 || true)
  echo "cloud-init: $(tail -1 <<< "$ci")"
  grep -q 'status: done' <<< "$ci" || { echo "team-gate-vps: cloud-init did not finish: $ci"; exit 1; }
  sshx 'docker compose version && docker buildx version' | sed 's/^/  vps | /'

  step "3. DNS: A $name -> $ip"
  body=$(jq -nc --arg n "$name" --arg ip "$ip" --arg c "$comment" '{type: "A", name: $n, content: $ip, ttl: 60, proxied: false, comment: $c}')
  record_id=$(cfapi POST "/zones/$zone_id/dns_records" "$body" | jq -r '.result.id')
  echo "record $record_id"
  for i in $(seq 90); do
    answer=$(curl -sS --max-time 10 -H 'accept: application/dns-json' "https://1.1.1.1/dns-query?name=$name&type=A" |
      jq -r '.Answer[]? | select(.type == 1) | .data' 2> /dev/null || true)
    grep -qx "$ip" <<< "$answer" && break
    [ "$i" = 90 ] && { echo "team-gate-vps: 1.1.1.1 does not resolve $name to $ip within 15 minutes"; exit 1; }
    sleep 10
  done
  echo "1.1.1.1 resolves $name to $ip"
fi

step "4. ship git archive $sha and bring compose up"
if sshx "test -e $remote/.env"; then
  echo "team-gate-vps: $remote/.env exists on $ip: the host is configured already, and new passwords would not match its databases"
  exit 1
fi
git archive --format=tar HEAD | sshx "mkdir -p $remote && tar -x -C $remote"
sshx "NAME=$name DIR=$remote bash -s" << 'REMOTE'
set -euo pipefail
umask 022 # the gate (uid 65532) reads the configuration and enters signing/, whatever root's umask on the host
cd "$DIR"
pw() { openssl rand -hex 24; }
kdb=$(pw) tdb=$(pw) pdb=$(pw)
sed -e "s/gate\.example\.com/$NAME/g" -e "s|postgres://kontra:kontra@|postgres://kontra:$kdb@|" \
  deploy/compose/kontra.vps.yaml > deploy/compose/kontra.run.yaml
grep -q "public_url: https://$NAME\$" deploy/compose/kontra.run.yaml
grep -q "postgres://kontra:$kdb@postgres" deploy/compose/kontra.run.yaml
printf 'KONTRA_CONFIG=./deploy/compose/kontra.run.yaml\nKONTRA_PORT=443\nKONTRA_DB_PASSWORD=%s\nTEMPORAL_DB_PASSWORD=%s\nPOSTGRES_PASSWORD=%s\n' \
  "$kdb" "$tdb" "$pdb" > .env
chmod 0600 .env
mkdir -p deploy/compose/signing
openssl ecparam -name prime256v1 -genkey -noout | openssl pkcs8 -topk8 -nocrypt > deploy/compose/signing/kontra.key
chown 65532:65532 deploy/compose/signing/kontra.key
chmod 0400 deploy/compose/signing/kontra.key
echo "configured: deploy/compose/kontra.run.yaml, .env (KONTRA_PORT=443), a new signing key"
REMOTE
t0=$SECONDS
sshx "cd $remote && timeout 1200 docker compose up -d --build --wait" 2>&1 | tail -20 | sed 's/^/  vps | /'
echo "compose up --build --wait: $((SECONDS - t0)) s"

step "5. verify from this host"
# cx: how this host's checks reach the gate (a rehearsal's --connect-to and --ca-file; nothing for the gate run).
cx=()
[ -z "$connect_to" ] || cx+=(--connect-to "$name:443:$connect_to")
[ -z "$ca_file" ] || cx+=(--cacert "$ca_file")
if ! curl -fsS --max-time 20 "${cx[@]}" "https://$name/healthz" > /dev/null; then
  echo "team-gate-vps: GET https://$name/healthz failed"
  exit 1
fi
echo "GET https://$name/healthz: 200"
issuer=$(openssl s_client -connect "${connect_to:-$name:443}" -servername "$name" < /dev/null 2> /dev/null | openssl x509 -noout -issuer -subject || true)
echo "$issuer"
if [ -n "$ca_file" ]; then
  want="issuer=$(openssl x509 -noout -subject -in "$ca_file" | sed 's/^subject=//')"
  grep -qxF "$want" <<< "$issuer" || { echo "team-gate-vps: the certificate is not the rehearsal CA's ($want)"; exit 1; }
else
  grep -q "Let's Encrypt" <<< "$issuer" || { echo "team-gate-vps: the certificate is not Let's Encrypt's"; exit 1; }
fi
if [ -n "$connect_to" ]; then
  echo "rehearsal: the gate is at $connect_to, not at $ip: the port check is the VPS's, skipped"
else
  for p in 22 80 443 5432 7233 8443 9090; do
    if timeout 5 bash -c "exec 3<>/dev/tcp/$ip/$p" 2> /dev/null; then state=open; else state=closed; fi
    echo "port $p: $state"
    case "$p:$state" in 22:open | 443:open | 80:closed | 5432:closed | 7233:closed | 8443:closed | 9090:closed) ;;
      *) echo "team-gate-vps: port $p is $state"; exit 1 ;;
    esac
  done
fi
key=$(sshx "cd $remote && docker compose exec -T gate kontra-gate enroll -f /etc/kontra/kontra.yaml --api-key m4run" | tail -1)
token=$(sshx "cd $remote && docker compose exec -T gate kontra-gate enroll -f /etc/kontra/kontra.yaml hello" | tail -1)
if ! [[ "$key" =~ ^kk_[a-z0-9]+_[A-Za-z0-9]+$ ]] || ! [[ "$token" =~ ^ket_[A-Za-z0-9]+$ ]]; then
  echo "team-gate-vps: kontra-gate enroll gave no key or no token"
  exit 1
fi
echo "an API key ${key%%_*}_… and a token for hello (neither recorded)"
# The key goes to curl in a header file only this user reads (the run's directory is 0700), never on a command line.
keyh="$tmp/key.h"
(umask 077 && printf 'Authorization: Bearer %s\n' "$key" > "$keyh")

echo "the team member's clean machine: $teammate"
if ! docker image inspect "kontra-demo:$sha" > /dev/null 2>&1; then
  if [ "$published" = 1 ]; then
    scripts/demo-clean-laptop.sh --build-only --published > /dev/null
  else
    scripts/demo-clean-laptop.sh --build-only > /dev/null
  fi
  built_image=1
fi
install='(cd /src && go install ./cmd/kontra)'
[ "$published" = 1 ] && install="go install kontra.sh/kontra/cmd/kontra@$expect"
trust=":"
if [ -n "$ca_file" ]; then
  # shellcheck disable=SC2016 # $HOME is the team member's, expanded in the container
  trust='cat /etc/ssl/certs/ca-certificates.crt /opt/kontra-teammate/extra-ca.pem > "$HOME/ca.pem" && export SSL_CERT_FILE="$HOME/ca.pem"'
fi
mkdir -p "$tmp/teammate"
printf '%s\n' "$token" > "$tmp/teammate/token"
[ -z "$ca_file" ] || cp "$ca_file" "$tmp/teammate/extra-ca.pem"
cat > "$tmp/teammate/run.sh" << EOF
set -eu
$trust
$install
go install github.com/a2aproject/a2a-cli@v0.3.1 && mv "\$(go env GOPATH)/bin/a2a-cli" "\$(go env GOPATH)/bin/a2a"
export PATH="\$PATH:\$(go env GOPATH)/bin"
kontra version
a2a server --exec 'echo Hello World' --port 9999 &
exec kontra connect --gate https://$name --enroll @/opt/kontra-teammate/token --serve a2a:hello=http://127.0.0.1:9999
EOF
# docker cp keeps these modes (root becomes the owner) and the image runs as laptop (uid 1000): the files must be
# world-readable whatever this host's umask. On this host the run's directory (0700) still keeps the token to its owner.
chmod 0755 "$tmp/teammate"
chmod 0644 "$tmp/teammate"/*
net_args=()
[ -z "$network" ] || net_args=(--network "$network")
docker create --name "$teammate" --init "${net_args[@]}" "kontra-demo:$sha" bash /opt/kontra-teammate/run.sh > /dev/null
docker cp "$tmp/teammate/." "$teammate:/opt/kontra-teammate" > /dev/null
docker start "$teammate" > /dev/null
for i in $(seq 240); do
  logs=$(docker logs "$teammate" 2>&1 || true)
  grep -q "connected to https://$name" <<< "$logs" && break
  [ "$(docker container inspect -f '{{.State.Running}}' "$teammate")" = true ] || { docker logs "$teammate" 2>&1 | tail -20; exit 1; }
  [ "$i" = 240 ] && { echo "team-gate-vps: the team member's node did not connect within 20 minutes"; exit 1; }
  sleep 5
done
{ docker logs "$teammate" 2>&1 | grep -v -e '^go: downloading' | tail -8 | sed 's/^/  teammate | /'; } || true

body='{"jsonrpc":"2.0","id":1,"method":"SendMessage","params":{"message":{"messageId":"m4-vps-'"$epoch"'","role":"ROLE_USER","parts":[{"text":"hi"}]}}}'
answer=$(curl -sS --max-time 60 "${cx[@]}" -H @"$keyh" -H 'Content-Type: application/json' -H 'A2A-Version: 1.0' --data "$body" "https://$name/agents/hello")
echo "SendMessage \"hi\" to https://$name/agents/hello with the key: $(head -c 400 <<< "$answer")"
grep -qF 'Hello World' <<< "$answer" || { echo "team-gate-vps: no Hello World"; exit 1; }
if [ -z "$connect_to" ] && command -v a2a > /dev/null 2>&1; then
  out=$(A2ACLI_AUTH="Bearer $key" a2a send -a "https://$name/agents/hello/.well-known/agent-card.json" hi 2>&1)
  echo "a2a send: $out"
  grep -qF 'Hello World' <<< "$out" || { echo "team-gate-vps: a2a send printed no Hello World"; exit 1; }
fi
mcp=(-sS --max-time 60 "${cx[@]}" -H @"$keyh" -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream')
curl "${mcp[@]}" -D "$tmp/mcp.h" -o /dev/null --data \
  '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"team-gate-vps","version":"1"}}}' \
  "https://$name/mcp"
sid=$(sed -n 's/^[Mm][Cc][Pp]-[Ss]ession-[Ii][Dd]: *\([^[:space:]]*\).*/\1/p' "$tmp/mcp.h")
[ -n "$sid" ] || { echo "team-gate-vps: /mcp initialize gave no Mcp-Session-Id"; exit 1; }
mcp+=(-H "Mcp-Session-Id: $sid" -H 'MCP-Protocol-Version: 2025-11-25')
curl "${mcp[@]}" -o /dev/null --data '{"jsonrpc":"2.0","method":"notifications/initialized"}' "https://$name/mcp"
found=$(curl "${mcp[@]}" --data '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search","arguments":{}}}' "https://$name/mcp")
echo "MCP tools/call search on https://$name/mcp with the key: $(head -c 400 <<< "$found")"
json=$(sed -n 's/^data: //p' <<< "$found")
[ -n "$json" ] || json=$found
jq -e '.result.structuredContent.services[] | select(.ref == "hello")' <<< "$json" > /dev/null ||
  { echo "team-gate-vps: search does not list hello"; exit 1; }
curl "${mcp[@]}" -o /dev/null -X DELETE "https://$name/mcp" || true

step "record"
sshx "cd $remote && docker compose ps && docker compose images && docker compose exec -T gate kontra-gate version" | sed 's/^/  vps | /'
sshx "docker image ls --digests --format '{{.Repository}}:{{.Tag}} {{.Digest}} {{.ID}}'" | sed 's/^/  vps | /'
if [ "$mode" = host ]; then
  echo "team-gate-vps: PASS (root@$host, https://$name, $((SECONDS / 60)) min)"
else
  echo "team-gate-vps: PASS ($droplet, https://$name, $((SECONDS / 60)) min)"
fi
