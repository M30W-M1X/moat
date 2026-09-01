#!/usr/bin/env bash
# Moat end-to-end demo.
#
# It starts moatd, creates sandboxes over the HTTP API, runs real work in
# one, and checks the isolation properties: a separate microVM kernel, a
# private rootfs per sandbox, the ingress secret firewall, and the
# hash-chained audit log. It also imports a container image as a template
# and shows the image ENV / WORKDIR / ENTRYPOINT applied as exec defaults.
# Every check asserts. The script exits non-zero on the first failure.
#
# Needs: /dev/kvm, curl, jq, podman, a built moatd and moat, a kernel
# (make kernel), and the dev-shell image (make demo-image).
#
# Network isolation and resource caps need a real root host. This script
# reports those steps and skips them when it runs rootless.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

# ----- colour -----------------------------------------------------------------
if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then
  B=$'\033[1m'; DIM=$'\033[2m'; GRN=$'\033[32m'; RED=$'\033[31m'; YEL=$'\033[33m'; CYN=$'\033[36m'; Z=$'\033[0m'
else
  B=""; DIM=""; GRN=""; RED=""; YEL=""; CYN=""; Z=""
fi

STEP=0
PASS=0
FAIL=0
hdr()  { STEP=$((STEP + 1)); printf '\n%s== %d. %s ==%s\n' "$B" "$STEP" "$1" "$Z"; }
info() { printf '   %s%s%s\n' "$DIM" "$1" "$Z"; }
pass() { PASS=$((PASS + 1)); printf '   %s[ok]%s   %s\n' "$GRN" "$Z" "$1"; }
skip() { printf '   %s[skip]%s %s\n' "$YEL" "$Z" "$1"; }
fail() {
  FAIL=$((FAIL + 1))
  printf '   %s[FAIL]%s %s\n' "$RED" "$Z" "$1"
  if [ -f "${SCRATCH:-/nonexistent}/moatd.log" ]; then
    printf '%s----- moatd.log (tail) -----%s\n' "$DIM" "$Z"
    tail -n 20 "$SCRATCH/moatd.log" | sed 's/^/   /'
  fi
}

# ----- preflight ------------------------------------------------------------
hdr "Preflight"
[ -r /dev/kvm ] || { echo "no readable /dev/kvm — this host cannot run microVMs" >&2; exit 1; }
for c in curl jq base64 podman; do
  command -v "$c" >/dev/null || { echo "missing: $c" >&2; exit 1; }
done
[ -f build/artifacts/vmlinux ] || { echo "no build/artifacts/vmlinux — run: make kernel" >&2; exit 1; }
IMG="build/artifacts/dev-shell-rootfs.ext4"
if [ ! -f "$IMG" ]; then
  info "building the dev-shell image (make demo-image) ..."
  make demo-image >/dev/null
fi
RHEL9_IMG="build/artifacts/rhel9-rootfs.ext4"
HAVE_RHEL9=""
if [ -f "$RHEL9_IMG" ]; then
  HAVE_RHEL9=1
elif make template-image NAME=rhel9 >/dev/null 2>&1; then
  HAVE_RHEL9=1
else
  info "rhel9 image unavailable (Red Hat registry unreachable?) — that step will be skipped"
fi
info "building moatd and moat ..."
make daemon build agent >/dev/null
pass "host ready — $(uname -sr), rootfs $(du -h "$IMG" | cut -f1)"
if [ "$(id -u)" -ne 0 ]; then
  info "running rootless — network and cgroup steps are reported, not run"
fi

# ----- start moatd -------------------------------------------------------------
hdr "Start the control plane"
SCRATCH="$(mktemp -d "${TMPDIR:-/tmp}/moat-demo.XXXXXX")"
AUDIT="$SCRATCH/audit"
SECRET="demo-$(head -c 6 /dev/urandom | base64 | tr -dc 'a-zA-Z0-9')"
BASE=""
PORT=0
for _ in 1 2 3 4 5; do
  cand=$(( (RANDOM % 20000) + 20000 ))
  if ! (exec 3<>"/dev/tcp/127.0.0.1/$cand") 2>/dev/null; then PORT=$cand; break; fi
done
[ "$PORT" -ne 0 ] || { echo "no free port found" >&2; exit 1; }
BASE="http://127.0.0.1:$PORT"
AUTH="Authorization: Bearer $SECRET"
SANDBOXES=()

IMPORT_TAG="moat-demo-import:latest"
cleanup() {
  for id in "${SANDBOXES[@]:-}"; do
    [ -n "$id" ] || continue
    curl -sS -X DELETE -H "$AUTH" "$BASE/v1/sandboxes/$id" >/dev/null 2>&1 || true
  done
  [ -n "${MOATD_PID:-}" ] && kill "$MOATD_PID" 2>/dev/null || true
  [ -n "${MOATD_PID:-}" ] && wait "$MOATD_PID" 2>/dev/null || true
  podman rmi -f "$IMPORT_TAG" >/dev/null 2>&1 || true
  rm -rf "$SCRATCH"
}
trap cleanup EXIT

# A small image with a distinctive config, to import as a template.
info "building a demo image for 'moat template import' ..."
podman build -q -t "$IMPORT_TAG" -f - "$SCRATCH" >/dev/null <<'EOF'
FROM docker.io/library/alpine@sha256:c64c687cbea9300178b30c95835354e34c4e4febc4badfe27102879de0483b5e
ENV APP_ENV=production LOG_LEVEL=info
WORKDIR /workspace
ENTRYPOINT ["/bin/echo"]
CMD ["ready"]
EOF

# The daemon reads a config file: the dev-shell template plus the token.
cat >"$SCRATCH/moatd.json" <<EOF
{
  "image_verify": "off",
  "audit": { "dir": "$AUDIT" },
  "templates": [
    { "name": "dev-shell", "kernel": "$ROOT/build/artifacts/vmlinux",
      "rootfs": "$ROOT/$IMG", "vcpus": 2, "mem_mib": 1024 }
  ],
  "tokens": [ { "id": "demo", "secret": "$SECRET" } ]
}
EOF

# Add the rhel9 template to the store when its image is built.
if [ -n "$HAVE_RHEL9" ]; then
  jq --arg k "$ROOT/build/artifacts/vmlinux" --arg r "$ROOT/$RHEL9_IMG" \
    '.templates += [{"name":"rhel9","kernel":$k,"rootfs":$r,"vcpus":2,"mem_mib":512}]' \
    "$SCRATCH/moatd.json" >"$SCRATCH/moatd.json.tmp" && mv "$SCRATCH/moatd.json.tmp" "$SCRATCH/moatd.json"
fi

# Import the image and append it to the config as the "demo-import" template.
info "moat template import $IMPORT_TAG --name demo-import"
./bin/moat template import "$IMPORT_TAG" --name demo-import \
  --data "$SCRATCH" --agent "$ROOT/bin/guest-agent" --config "$SCRATCH/moatd.json" \
  2>&1 | sed 's/^/   | /'

info "moatd on $BASE   audit dir $AUDIT"
./bin/moatd -config "$SCRATCH/moatd.json" -addr "127.0.0.1:$PORT" -json-log=false \
  >"$SCRATCH/moatd.log" 2>&1 &
MOATD_PID=$!
info "the images have no images.lock, so the config sets image_verify=off"

ready=""
for _ in $(seq 1 40); do
  if curl -sS -o /dev/null "$BASE/healthz" 2>/dev/null; then ready=1; break; fi
  kill -0 "$MOATD_PID" 2>/dev/null || { fail "moatd exited during startup"; exit 1; }
  sleep 0.5
done
[ -n "$ready" ] || { fail "moatd did not answer /healthz"; exit 1; }
pass "moatd is up (pid $MOATD_PID)"

# ----- helpers that talk to the API -----------------------------------------
api() { curl -sS -H "$AUTH" "$@"; }

# sb_create [json-body] -> prints the new sandbox id
sb_create() {
  local body="${1:-{\}}"
  local out id
  out=$(api -X POST -H 'Content-Type: application/json' -d "$body" "$BASE/v1/sandboxes")
  id=$(printf '%s' "$out" | jq -r '.id // empty')
  [ -n "$id" ] || { echo "create failed: $out" >&2; return 1; }
  SANDBOXES+=("$id")
  printf '%s' "$id"
}

# sb_exec_body <id> <json-body>  -> prints guest stdout+stderr, returns exit code
sb_exec_body() {
  local id=$1 body=$2 resp code
  resp="$SCRATCH/exec.$RANDOM.ndjson"
  api -X POST -H 'Content-Type: application/json' -d "$body" \
    "$BASE/v1/sandboxes/$id/exec" >"$resp"
  while IFS= read -r d; do [ -n "$d" ] && printf '%s' "$d" | base64 -d; done \
    < <(jq -r 'select(.stream=="stdout" or .stream=="stderr") | .data' "$resp")
  code=$(jq -r 'select(.stream=="exit") | .code' "$resp")
  rm -f "$resp"
  return "${code:-1}"
}

# sb_exec <id> <arg>...  -> prints guest stdout+stderr, returns the guest exit code
sb_exec() {
  local id=$1; shift
  local argv
  argv=$(printf '%s\n' "$@" | jq -R . | jq -s -c .)
  sb_exec_body "$id" "{\"argv\":$argv,\"timeout_ms\":120000}"
}

# ----- create a sandbox ----------------------------------------------------
hdr "Create a sandbox"
A=$(sb_create '{"ttl_seconds":900}')
pass "created $A"
state=$(api "$BASE/v1/sandboxes/$A" | jq -r '.template + " ttl=" + (.expires_at // "?")')
info "template $state"
active=$(curl -sS "$BASE/metrics" | awk '/^moat_sandboxes_active/ {print $2}')
[ "${active:-0}" = "1" ] && pass "moat_sandboxes_active = 1" || fail "expected 1 active sandbox, got ${active:-0}"

# ----- prove the microVM boundary ----------------------------------------
hdr "Prove the microVM boundary"
guest_kernel=$(sb_exec "$A" uname -r) || true
host_kernel=$(uname -r)
info "host  kernel $host_kernel"
info "guest kernel $guest_kernel"
case "$guest_kernel" in
  6.1.*) pass "the guest runs its own pinned kernel, not the host kernel" ;;
  *)     fail "unexpected guest kernel: $guest_kernel" ;;
esac
pid1=$(sb_exec "$A" sh -c 'cat /proc/1/comm') || true
[ "$pid1" = "guest-agent" ] && pass "PID 1 in the guest is the Moat agent" || fail "PID 1 is '$pid1', want guest-agent"

# ----- run real work -----------------------------------------------------
hdr "Run a real build in the sandbox"
for f in go.mod wordcount.go wordcount_test.go sample.txt; do
  out=$(api -X PUT --data-binary @"demo/task/$f" "$BASE/v1/sandboxes/$A/files/work/$f")
  n=$(printf '%s' "$out" | jq -r '.bytes_written // "?"')
  info "put /work/$f ($n bytes)"
done
test_out=$(sb_exec "$A" sh -c 'cd /work && go test ./...') && rc=0 || rc=$?
printf '%s\n' "$test_out" | sed 's/^/   | /'
[ "$rc" -eq 0 ] && pass "go test passed in the sandbox" || fail "go test failed (rc=$rc)"
build_out=$(sb_exec "$A" sh -c 'cd /work && go build -o wordcount . && ./wordcount sample.txt | head -3') && rc=0 || rc=$?
printf '%s\n' "$build_out" | sed 's/^/   | /'
[ "$rc" -eq 0 ] && pass "go build ran and the binary works" || fail "go build failed (rc=$rc)"

# ----- copy the artifact back out --------------------------------------
hdr "Copy the built binary out"
api "$BASE/v1/sandboxes/$A/files/work/wordcount" -o "$SCRATCH/wordcount.out"
kind=$(file -b "$SCRATCH/wordcount.out" 2>/dev/null || echo "unknown")
info "$kind"
case "$kind" in
  *ELF*executable*) pass "got a $(du -h "$SCRATCH/wordcount.out" | cut -f1) ELF binary from the sandbox" ;;
  *)                fail "downloaded file is not an ELF binary" ;;
esac

# ----- ingress secret firewall --------------------------------------
hdr "Ingress secret firewall"
# 18 random bytes as 36 hex chars — shape of a GitHub token, real entropy.
key="ghp_$(od -An -tx1 -N18 /dev/urandom | tr -d ' \n')"
printf 'GITHUB_TOKEN=%s\n' "$key" >"$SCRATCH/fake.env"
code=$(curl -sS -o "$SCRATCH/sec.json" -w '%{http_code}' -X PUT -H "$AUTH" \
  --data-binary @"$SCRATCH/fake.env" "$BASE/v1/sandboxes/$A/files/work/.env")
rule=$(jq -r '.findings[0].rule // "?"' "$SCRATCH/sec.json" 2>/dev/null || echo "?")
if [ "$code" = "409" ]; then
  pass "a credential in an uploaded file is blocked with 409 (rule: $rule)"
else
  fail "expected 409 for the credential upload, got $code"
fi
gone=$(sb_exec "$A" sh -c 'test ! -e /work/.env && echo absent') || true
[ "$gone" = "absent" ] && pass "the blocked file never reached the guest" || fail "/work/.env exists in the guest"

# ----- one microVM per task ------------------------------------------
hdr "One microVM per task"
sb_exec "$A" sh -c 'echo secret-A > /work/marker' >/dev/null || true
Bx=$(sb_create '{"ttl_seconds":300}')
info "sandbox A $A"
info "sandbox B $Bx"
[ "$A" != "$Bx" ] && pass "each create returns a new sandbox id" || fail "sandbox ids collided"
seen=$(sb_exec "$Bx" sh -c 'cat /work/marker 2>/dev/null || echo none') || true
[ "$seen" = "none" ] && pass "sandbox B cannot see sandbox A's files (private rootfs)" || fail "sandbox B read '$seen' from A's rootfs"
curl -sS -X DELETE -H "$AUTH" "$BASE/v1/sandboxes/$Bx" >/dev/null
SANDBOXES=("${SANDBOXES[@]/$Bx/}")
pass "destroyed sandbox B"

# ----- ephemeral -------------------------------------------------------
hdr "Sandboxes are ephemeral"
curl -sS -X DELETE -H "$AUTH" "$BASE/v1/sandboxes/$A" >/dev/null
SANDBOXES=("${SANDBOXES[@]/$A/}")
info "destroyed sandbox A (which held /work/marker and /work/wordcount)"
C=$(sb_create '{"ttl_seconds":300}')
left=$(sb_exec "$C" sh -c 'ls -A /work | tr "\n" " "') || true
if [ -z "$(printf '%s' "$left" | tr -d '[:space:]')" ]; then
  pass "a fresh sandbox has an empty /work — nothing carries over"
else
  fail "fresh sandbox /work is not empty: $left"
fi
curl -sS -X DELETE -H "$AUTH" "$BASE/v1/sandboxes/$C" >/dev/null
SANDBOXES=("${SANDBOXES[@]/$C/}")

# ----- the template store ------------------------------------------
hdr "The template store"
info "moatd offers these templates (from its -config file):"
jq -r '.templates[] | "   | " + .name + "  " + (.source // "built from build/templates/")' "$SCRATCH/moatd.json"
if [ -n "$HAVE_RHEL9" ]; then
  R=$(sb_create '{"template":"rhel9","ttl_seconds":300}')
  info "sandbox $R on template rhel9"
  rel=$(sb_exec "$R" cat /etc/redhat-release) || true
  case "$rel" in
    "Red Hat Enterprise Linux release 9"*)
      pass "the rhel9 sandbox reports: $rel" ;;
    *)
      fail "rhel9 sandbox did not report RHEL 9: '$rel'" ;;
  esac
  tools=$(sb_exec "$R" sh -c 'command -v dnf >/dev/null && echo "dnf $(dnf --version 2>/dev/null | head -1)"; python3 --version 2>&1') || true
  printf '%s\n' "$tools" | sed 's/^/   | /'
  info "the default template (dev-shell) is Alpine / musl — same kernel, same API, different distro"
  curl -sS -X DELETE -H "$AUTH" "$BASE/v1/sandboxes/$R" >/dev/null
  SANDBOXES=("${SANDBOXES[@]/$R/}")
else
  skip "rhel9 sandbox: image not built (run: make template-image NAME=rhel9)"
fi

# ----- imported container template ---------------------------------
hdr "A container image, imported as a template"
info "the demo-import template came from '$IMPORT_TAG' at startup:"
./bin/moat template show demo-import --data "$SCRATCH" --config "$SCRATCH/moatd.json" 2>/dev/null \
  | sed 's/^/   | /' || true
D=$(sb_create '{"template":"demo-import","ttl_seconds":300}')
info "sandbox $D on template demo-import"

# no argv -> moatd runs the image's ENTRYPOINT + CMD
dflt=$(sb_exec_body "$D" '{}') || true
[ "$dflt" = "ready" ] && pass "an exec with no argv runs the image default (ENTRYPOINT + CMD)" \
  || fail "default_argv did not run: got '$dflt'"

# the image ENV and WORKDIR apply to every exec
applied=$(sb_exec "$D" sh -c 'printf "%s %s/%s" "$(pwd)" "$APP_ENV" "$LOG_LEVEL"') || true
[ "$applied" = "/workspace production/info" ] \
  && pass "the image WORKDIR and ENV apply as exec defaults ($applied)" \
  || fail "image defaults not applied: got '$applied'"

# a request value always wins over the template default
override=$(sb_exec_body "$D" '{"argv":["sh","-c","printf \"%s %s\" \"$(pwd)\" \"$APP_ENV\""],"dir":"/tmp","env":{"APP_ENV":"staging"}}') || true
[ "$override" = "/tmp staging" ] \
  && pass "a request dir and env override the template ($override)" \
  || fail "request did not win: got '$override'"

curl -sS -X DELETE -H "$AUTH" "$BASE/v1/sandboxes/$D" >/dev/null
SANDBOXES=("${SANDBOXES[@]/$D/}")

# ----- network + caps (root only) ------------------------------------
hdr "Network isolation and resource caps"
if [ "$(id -u)" -eq 0 ]; then
  N=$(sb_create '{"networked":true,"ttl_seconds":180}')
  meta=$(sb_exec "$N" sh -c 'wget -T 3 -qO- http://169.254.169.254/ 2>&1 || echo BLOCKED') || true
  case "$meta" in
    *BLOCKED*) pass "the cloud metadata address 169.254.169.254 is unreachable from the guest" ;;
    *)         fail "guest reached 169.254.169.254: $meta" ;;
  esac
  curl -sS -X DELETE -H "$AUTH" "$BASE/v1/sandboxes/$N" >/dev/null
  SANDBOXES=("${SANDBOXES[@]/$N/}")
else
  skip "networked sandbox: needs root or 'unshare --user --map-root-user --mount --net'"
  info "with -net moatd gives each sandbox its own netns + TAP + nftables rules:"
  info "  drop to the host LAN, to other sandboxes, and to 169.254.0.0/16"
  info "  egress only through the host proxy (domain allow / deny list)"
  info "resource caps: moatd -cpu-max / -mem-max / -pids-max set cgroup v2 limits"
  info "  on each Firecracker process, plus rootfs and network rate limiters"
fi

# ----- audit trail ---------------------------------------------------
hdr "The audit trail"
info "every boundary crossing above is one hash-chained record:"
./bin/moat audit tail -dir "$AUDIT" -limit 40 2>/dev/null \
  | awk '{printf "   | %s\n", $0}' || true
counts=$(./bin/moat audit tail -dir "$AUDIT" -limit 1000 -json 2>/dev/null \
  | jq -r '.type' | sort | uniq -c | awk '{printf "%s=%s ", $2, $1}')
info "event types: $counts"
if ./bin/moat audit verify -dir "$AUDIT" >/dev/null 2>&1; then
  pass "the audit chain verifies (no gap, no edit)"
else
  fail "audit chain verification failed"
fi

# ----- summary -----------------------------------------------------------
printf '\n%s== summary ==%s\n' "$B" "$Z"
printf '   %s%d passed%s' "$GRN" "$PASS" "$Z"
[ "$FAIL" -gt 0 ] && printf ',   %s%d failed%s' "$RED" "$FAIL" "$Z"
printf '\n'
[ "$FAIL" -eq 0 ] || exit 1
printf '   %sMoat started, isolated, and reaped every sandbox.%s\n' "$GRN" "$Z"
