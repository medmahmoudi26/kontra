#!/usr/bin/env bash
# The compose team gate on a clean VPS (DM4-14, DM4-15, DM4-31; the M4 plan's gate run C, the gate item "team gate up
# on a clean VPS"). It provisions a throwaway DigitalOcean droplet, names it under kontra.run through Cloudflare's API,
# ships `git archive HEAD` to it, brings compose.yaml up with deploy/compose/kontra.vps.yaml (built-in ACME), checks it
# from this host, enrolls a node from a team member's clean machine (the kontra-demo image, scripts/demo-clean-laptop.sh)
# and hires through it with an API key, A2A and MCP. Then it destroys everything it made, and checks that it did.
#
#   1. pre-flight (read-only): doctl's account; the DigitalOcean SSH key's MD5 fingerprint equals this host's key's
#      (ssh-keygen -E md5 -lf <key>.pub); the size exists (else its -amd variant); no droplet tagged kontra-m4-gaterun;
#      the zone exists and has no record for the name;
#   2. doctl compute droplet create kontra-m4-teamgate-<epoch> (fra1, s-2vcpu-4gb, ubuntu-24-04-x64, tags
#      kontra-m4-gaterun and ttl-<epoch + 4 h>, test/vps/cloud-init.yaml), then ssh until cloud-init is done;
#   3. an A record for the name (ttl 60, not proxied, comment "M4 VPS gate run, delete after"), then wait until
#      1.1.1.1 resolves it to the droplet (DNS over HTTPS), before anything asks Let's Encrypt;
#   4. git archive HEAD | ssh … tar -x -C /opt/kontra; kontra.vps.yaml with the name, three random passwords in .env
#      (KONTRA_PORT=443), a new signing key owned by 65532; docker compose up -d --build --wait (20 min at most);
#   5. from this host: GET /healthz; the certificate's issuer is Let's Encrypt; of 22, 80, 443, 5432, 7233, 8443 and
#      9090 only 22 and 443 answer; over ssh, an API key and a token for hello (kontra-gate enroll); a team member's
#      clean machine (a kontra-demo container on this host) runs a2a server --exec 'echo Hello World' and kontra connect
#      --enroll; then SendMessage "hi" to /agents/hello with the key answers Hello World, and an MCP tools/call search on
#      /mcp with the key lists hello; docker compose ps, the image digests, kontra-gate version and the timings are
#      recorded.
#
# Teardown, in a trap on EXIT, INT and TERM (printed before anything is created): docker rm -f kontra-teammate-<epoch>;
# docker image rm kontra-demo:<sha> when this run built it; doctl compute droplet delete -f <id>; DELETE the DNS
# record by id; ssh-keygen -R <ip>; rm -rf the run's temporary directory. Then it checks that doctl lists no droplet
# tagged kontra-m4-gaterun, that Cloudflare has no record for the name, and that no kontra-teammate container is left,
# and records the droplet's lifetime and cost. A crashed run is reaped with --teardown-only: every droplet tagged
# kontra-m4-gaterun (its ttl tag says when it was due) and every record with the run's comment or the name. Droplets
# are only ever listed by that tag; a droplet named main-droplet is never deleted.
#
# Usage: scripts/team-gate-vps.sh [--dry-run | --teardown-only] [--published --expect <commit>] [--record <file>]
#   --dry-run        the pre-flight and the plan, printed; nothing created
#   --teardown-only  reap what a run left (droplets by tag, DNS records by comment and name)
#   --published      the team member installs kontra with `go install kontra.sh/kontra/cmd/kontra@<--expect>`
#                    instead of from /src (after the commit is on GitHub)
#   --record FILE    the run's record (default ./team-gate-vps-<UTC time>.log); keys and tokens are never written
# Settings (environment): CLOUDFLARE_API_TOKEN (required; DNS edit on the zone), KONTRA_VPS_NAME
# (m4-teamgate.kontra.run), KONTRA_VPS_ZONE (kontra.run), KONTRA_VPS_REGION (fra1), KONTRA_VPS_SIZE (s-2vcpu-4gb),
# KONTRA_VPS_DO_KEY (58735991), KONTRA_VPS_SSH_KEY (~/.ssh/kontra-client01-fleet), KONTRA_VPS_TTL (14400 s).
# Prerequisites: doctl (authenticated), docker (a running daemon), curl, jq, ssh, ssh-keygen, openssl, git, and
# CLOUDFLARE_API_TOKEN; run from the repository. Exit 2 when one is missing.
set -euo pipefail

usage() { sed -n '/^# Usage:/,/^# Prerequisites:/p' "$0" | sed 's/^# \{0,1\}//' >&2; exit 2; }

mode=run published=0 expect="" record=""
while [ $# -gt 0 ]; do
  case "$1" in
    --dry-run) mode=dry ;;
    --teardown-only) mode=reap ;;
    --published) published=1 ;;
    --expect) expect=${2:?--expect needs a commit}; shift ;;
    --record) record=${2:?--record needs a file}; shift ;;
    -h | --help) usage ;;
    *) echo "team-gate-vps: unknown argument $1" >&2; usage ;;
  esac
  shift
done
if [ "$published" = 1 ] && ! [[ "$expect" =~ ^[0-9a-f]{12,40}$ ]]; then
  echo "team-gate-vps: --published needs --expect <the merge commit, 12 to 40 hex digits>" >&2
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

# Prerequisites, all checked before anything is created.
need=(doctl curl jq)
[ "$mode" = reap ] || need+=(docker ssh ssh-keygen openssl git)
missing=()
for tool in "${need[@]}"; do
  command -v "$tool" > /dev/null 2>&1 || missing+=("$tool")
done
[ -n "${CLOUDFLARE_API_TOKEN:-}" ] || missing+=("CLOUDFLARE_API_TOKEN (a Cloudflare token that edits DNS in $zone)")
if [ "$mode" != reap ]; then
  if [ ! -r "$ssh_key" ] || [ ! -r "$ssh_key.pub" ]; then
    missing+=("the SSH key $ssh_key and $ssh_key.pub (KONTRA_VPS_SSH_KEY)")
  fi
fi
if [ ${#missing[@]} -gt 0 ]; then
  printf 'team-gate-vps: missing prerequisites:\n' >&2
  printf '  %s\n' "${missing[@]}" >&2
  exit 2
fi
if ! doctl account get --format Email,Status --no-header > /dev/null 2>&1; then
  echo "team-gate-vps: doctl is not authenticated (doctl account get failed): doctl auth init" >&2
  exit 2
fi
if [ "$mode" != reap ] && ! docker info > /dev/null 2>&1; then
  echo "team-gate-vps: docker cannot reach its daemon (docker info failed); the team member's machine is a container" >&2
  exit 2
fi

# cfapi METHOD PATH [JSON]: Cloudflare's API, answering its JSON; fails unless success is true. The token is never
# printed.
cfapi() {
  local out
  if [ $# -ge 3 ]; then
    out=$(curl -sS --max-time 30 -X "$1" -H "Authorization: Bearer $CLOUDFLARE_API_TOKEN" -H 'Content-Type: application/json' \
      --data "$3" "$cf$2") || return 1
  else
    out=$(curl -sS --max-time 30 -X "$1" -H "Authorization: Bearer $CLOUDFLARE_API_TOKEN" "$cf$2") || return 1
  fi
  if [ "$(jq -r '.success' <<< "$out" 2> /dev/null)" != true ]; then
    echo "team-gate-vps: Cloudflare $1 $2: $(jq -c '.errors' <<< "$out" 2> /dev/null || head -c 300 <<< "$out")" >&2
    return 1
  fi
  printf '%s\n' "$out"
}

zone_id=$(cfapi GET "/zones?name=$zone" | jq -r '.result[0].id // empty') || exit 1
[ -n "$zone_id" ] || { echo "team-gate-vps: Cloudflare has no zone $zone for this token" >&2; exit 2; }

# records_for prints the ids of the zone's records named $name, and of those with the run's comment.
records_for() {
  local enc
  enc=$(jq -rn --arg c "$comment" '$c|@uri')
  { cfapi GET "/zones/$zone_id/dns_records?name=$name" | jq -r '.result[].id'
    cfapi GET "/zones/$zone_id/dns_records?comment.exact=$enc" | jq -r '.result[].id'; } | sort -u
}

# tagged prints "ID NAME TAGS" for every droplet tagged $tag; droplets are never listed any other way.
tagged() { doctl compute droplet list --tag-name "$tag" --format ID,Name,Tags --no-header; }

# delete_droplet ID NAME, refusing main-droplet whatever its tags.
delete_droplet() {
  if [ "$2" = main-droplet ]; then
    echo "team-gate-vps: refusing to delete $2 ($1), tagged $tag or not" >&2
    return 1
  fi
  echo "doctl compute droplet delete -f $1   # $2"
  doctl compute droplet delete -f "$1"
}

if [ "$mode" = reap ]; then
  echo "team-gate-vps: --teardown-only: droplets tagged $tag, DNS records named $name or commented \"$comment\""
  now=$(date +%s)
  rc=0
  while read -r id dname tags; do
    [ -n "$id" ] || continue
    due=$(tr ',' '\n' <<< "$tags" | sed -n 's/^ttl-\([0-9]*\)$/\1/p' | head -1)
    if [ -n "$due" ] && [ "$due" -lt "$now" ]; then
      echo "  $dname ($id): its ttl passed $(((now - due) / 60)) min ago"
    else
      echo "  $dname ($id): tagged $tag${due:+, due in $(((due - now) / 60)) min}"
    fi
    delete_droplet "$id" "$dname" || rc=1
  done < <(tagged)
  for rid in $(records_for); do
    echo "DELETE dns_records/$rid"
    cfapi DELETE "/zones/$zone_id/dns_records/$rid" > /dev/null || rc=1
  done
  left=$(tagged | awk '$2 != "main-droplet"' || true)
  [ -z "$left" ] || { echo "team-gate-vps: still tagged $tag:"; echo "$left"; rc=1; }
  [ -z "$(records_for)" ] || { echo "team-gate-vps: DNS records are left for $name"; rc=1; }
  [ "$rc" = 0 ] && echo "team-gate-vps: nothing of a run is left"
  exit "$rc"
fi

# Pre-flight, read-only.
repo=$(git rev-parse --show-toplevel 2> /dev/null) || { echo "team-gate-vps: run it from the repository" >&2; exit 2; }
cd "$repo"
sha=$(git rev-parse --short=12 HEAD)
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
left=$(tagged)
if [ -n "$left" ]; then
  printf 'team-gate-vps: droplets tagged %s exist (another run, or a crashed one: --teardown-only):\n%s\n' "$tag" "$left" >&2
  exit 2
fi
if [ -n "$(records_for)" ]; then
  echo "team-gate-vps: Cloudflare already has a record named $name or commented \"$comment\" (--teardown-only)" >&2
  exit 2
fi

epoch=$(date +%s)
droplet="kontra-m4-teamgate-$epoch"
teammate="kontra-teammate-$epoch"
echo "team-gate-vps: $(date -u +%FT%TZ), commit $sha, $droplet ($region, $size at \$$price/h, $os_image), https://$name"
echo "teardown, in a trap on EXIT/INT/TERM: docker rm -f $teammate; docker image rm kontra-demo:$sha (if built here);"
echo "  doctl compute droplet delete -f <id of $droplet>; DELETE the A record of $name by id; ssh-keygen -R <ip>;"
echo "  rm -rf <the run's temporary directory>; then: no droplet tagged $tag, no record for $name, no $teammate"
if [ "$mode" = dry ]; then
  echo "dry run: pre-flight passed; nothing created"
  exit 0
fi

tmp=$(mktemp -d "${TMPDIR:-/tmp}/kontra-vps.XXXXXX")
[ -n "$record" ] || record="$PWD/team-gate-vps-$(date -u +%Y%m%dT%H%M%SZ).log"
exec > >(tee -a "$record") 2>&1
echo "record: $record"
droplet_id="" ip="" record_id="" created="" built_image=0
teardown() {
  local rc=$?
  set +e
  trap - EXIT INT TERM
  echo "team-gate-vps: teardown (exit $rc)"
  docker rm -f "$teammate" > /dev/null 2>&1
  [ "$built_image" = 1 ] && docker image rm "kontra-demo:$sha" > /dev/null 2>&1
  if [ -z "$droplet_id" ]; then
    droplet_id=$(tagged | awk -v n="$droplet" '$2 == n {print $1}')
  fi
  [ -n "$droplet_id" ] && delete_droplet "$droplet_id" "$droplet"
  [ -n "$record_id" ] && { echo "DELETE dns_records/$record_id ($name)"; cfapi DELETE "/zones/$zone_id/dns_records/$record_id" > /dev/null; }
  [ -n "$ip" ] && ssh-keygen -R "$ip" > /dev/null 2>&1
  rm -rf "$tmp"
  local ok=1 i
  for i in 1 2 3 4 5 6; do
    [ -z "$(tagged)" ] && break
    sleep 10
  done
  [ -z "$(tagged)" ] || { echo "team-gate-vps: LEFT: droplets tagged $tag: $(tagged)"; ok=0; }
  [ -z "$(records_for)" ] || { echo "team-gate-vps: LEFT: a DNS record for $name"; ok=0; }
  [ -z "$(docker ps -aq --filter "name=$teammate")" ] || { echo "team-gate-vps: LEFT: container $teammate"; ok=0; }
  if [ -n "$created" ]; then
    local secs=$(($(date +%s) - created))
    echo "droplet lifetime: $((secs / 60)) min $((secs % 60)) s; cost at most \$$(awk -v p="$price" -v s="$secs" \
      'BEGIN { h = int((s + 3599) / 3600); printf "%.4f (%d h at $%s/h)", h * p, h, p }')"
  fi
  if [ "$ok" = 1 ]; then echo "team-gate-vps: torn down; nothing left"; else rc=1; fi
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

step "2. create $droplet"
created=$(date +%s)
out=$(doctl compute droplet create "$droplet" --region "$region" --size "$size" --image "$os_image" --ssh-keys "$do_key" \
  --tag-names "$tag,ttl-$((epoch + life))" --user-data-file "$repo/test/vps/cloud-init.yaml" --wait \
  --format ID,PublicIPv4 --no-header)
read -r droplet_id ip <<< "$out"
if ! [[ "$droplet_id" =~ ^[0-9]+$ ]] || ! [[ "$ip" =~ ^[0-9.]+$ ]]; then
  echo "team-gate-vps: doctl answered '$out'"
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

step "4. ship git archive $sha and bring compose up"
git archive --format=tar HEAD | sshx 'mkdir -p /opt/kontra && tar -x -C /opt/kontra'
sshx "NAME=$name bash -s" << 'REMOTE'
set -euo pipefail
cd /opt/kontra
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
sshx 'cd /opt/kontra && timeout 1200 docker compose up -d --build --wait' 2>&1 | tail -20 | sed 's/^/  vps | /'
echo "compose up --build --wait: $((SECONDS - t0)) s"

step "5. verify from this host"
if ! curl -fsS --max-time 20 "https://$name/healthz" > /dev/null; then
  echo "team-gate-vps: GET https://$name/healthz failed"
  exit 1
fi
echo "GET https://$name/healthz: 200"
issuer=$(openssl s_client -connect "$name:443" -servername "$name" < /dev/null 2> /dev/null | openssl x509 -noout -issuer -subject || true)
echo "$issuer"
grep -q "Let's Encrypt" <<< "$issuer" || { echo "team-gate-vps: the certificate is not Let's Encrypt's"; exit 1; }
for p in 22 80 443 5432 7233 8443 9090; do
  if timeout 5 bash -c "exec 3<>/dev/tcp/$ip/$p" 2> /dev/null; then state=open; else state=closed; fi
  echo "port $p: $state"
  case "$p:$state" in 22:open | 443:open | 80:closed | 5432:closed | 7233:closed | 8443:closed | 9090:closed) ;;
    *) echo "team-gate-vps: port $p is $state"; exit 1 ;;
  esac
done
key=$(sshx 'cd /opt/kontra && docker compose exec -T gate kontra-gate enroll -f /etc/kontra/kontra.yaml --api-key m4run' | tail -1)
token=$(sshx 'cd /opt/kontra && docker compose exec -T gate kontra-gate enroll -f /etc/kontra/kontra.yaml hello' | tail -1)
if [ -z "$key" ] || [ -z "$token" ]; then
  echo "team-gate-vps: kontra-gate enroll gave no key or token"
  exit 1
fi
echo "an API key ${key%%_*}_… and a token for hello (neither recorded)"

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
mkdir -p "$tmp/teammate"
printf '%s\n' "$token" > "$tmp/teammate/token"
cat > "$tmp/teammate/run.sh" << EOF
set -eu
$install
go install github.com/a2aproject/a2a-cli@v0.3.1 && mv "\$(go env GOPATH)/bin/a2a-cli" "\$(go env GOPATH)/bin/a2a"
export PATH="\$PATH:\$(go env GOPATH)/bin"
kontra version
a2a server --exec 'echo Hello World' --port 9999 &
exec kontra connect --gate https://$name --enroll @/opt/kontra-teammate/token --serve a2a:hello=http://127.0.0.1:9999
EOF
docker create --name "$teammate" --init "kontra-demo:$sha" bash /opt/kontra-teammate/run.sh > /dev/null
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

auth=(-H "Authorization: Bearer $key")
body='{"jsonrpc":"2.0","id":1,"method":"SendMessage","params":{"message":{"messageId":"m4-vps-'"$epoch"'","role":"ROLE_USER","parts":[{"text":"hi"}]}}}'
answer=$(curl -sS --max-time 60 "${auth[@]}" -H 'Content-Type: application/json' -H 'A2A-Version: 1.0' --data "$body" "https://$name/agents/hello")
echo "SendMessage \"hi\" to https://$name/agents/hello with the key: $(head -c 400 <<< "$answer")"
grep -qF 'Hello World' <<< "$answer" || { echo "team-gate-vps: no Hello World"; exit 1; }
if command -v a2a > /dev/null 2>&1; then
  out=$(a2a send -a "https://$name/agents/hello/.well-known/agent-card.json" --auth "Bearer $key" hi 2>&1)
  echo "a2a send: $out"
  grep -qF 'Hello World' <<< "$out" || { echo "team-gate-vps: a2a send printed no Hello World"; exit 1; }
fi
mcp=(-sS --max-time 60 "${auth[@]}" -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream')
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
sshx 'cd /opt/kontra && docker compose ps && docker compose images && docker compose exec -T gate kontra-gate version' | sed 's/^/  vps | /'
sshx "docker image ls --digests --format '{{.Repository}}:{{.Tag}} {{.Digest}} {{.ID}}'" | sed 's/^/  vps | /'
echo "team-gate-vps: PASS ($droplet, https://$name, $((SECONDS / 60)) min)"
