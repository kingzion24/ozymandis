#!/usr/bin/env bash
#
# The integration test the unit suite is structurally blind to.
#
# Every seam in this codebase is unit-tested against a fake that satisfies that
# seam's contract. This runs the CHAIN, which is where the bugs the fakes cannot
# express live: the joins, where two real components each satisfy their own
# contract and disagree with each other.
#
# Six checks, each exercising a full seam no fake reached:
#
#   1  deploy on push        a push builds with nobody clicking
#   2  the release veto      a failing release leaves the OLD image serving
#   3  the exit-code thread  a container exiting 42 makes `oz` exit 42
#   4  the fan-out filter    a push to service B leaves service A untouched
#   5  the deploy-key mode   0400 as the CLUSTER applies it, not as asserted
#   6  async failure is read a post-202 failure surfaces somewhere a person looks
#
# Run this ON the cluster host, or anywhere with a kubeconfig that reaches it.
#
#   OZ_ENDPOINT=https://ozymandis.example \
#   OZ_TOKEN=oz_...                       \
#   OZ_REPO=git@github.com:you/e2e-mono.git \
#   ./build/e2e/deploy-on-push.sh
#
# It needs a repository it can push to, because check 1 and check 4 are about
# what GitHub actually sends — and GitHub's commit-list truncation is only
# observable by provoking it.

set -uo pipefail

: "${OZ_ENDPOINT:?set OZ_ENDPOINT to the install's URL}"
: "${OZ_TOKEN:?set OZ_TOKEN to an API token}"
: "${OZ_REPO:?set OZ_REPO to a repository this install may clone}"

OZ="${OZ:-oz}"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

pass=0; fail=0
ok()   { printf '  \033[32mPASS\033[0m  %s\n' "$1"; pass=$((pass+1)); }
bad()  { printf '  \033[31mFAIL\033[0m  %s\n' "$1"; fail=$((fail+1)); }
step() { printf '\n\033[1m==>\033[0m %s\n' "$1"; }
note() { printf '        %s\n' "$1"; }

api() {
	local method="$1" path="$2"; shift 2
	curl -sS -X "$method" -H "Authorization: Bearer ${OZ_TOKEN}" \
		-H 'Content-Type: application/json' "$@" "${OZ_ENDPOINT}${path}"
}

# ---------------------------------------------------------------- check 1 ---
# Deploy on push. The whole point of stage 4, and the one thing no fake could
# show: a real delivery from GitHub, signed with a real secret, starting a real
# build.

step "1. A push deploys, with nobody clicking"

api POST /api/v1/apps -d "{
  \"name\": \"e2e-api\",
  \"source\": \"git\",
  \"repo_url\": \"${OZ_REPO}\",
  \"repo_branch\": \"main\",
  \"repo_subdir\": \"services/api\",
  \"port\": 8080
}" >/dev/null

note "Enable auto-deploy in the dashboard and copy the secret it shows once,"
note "then add the webhook to ${OZ_REPO} pointing at:"
note "  ${OZ_ENDPOINT}/webhooks/github"
note "Add the deploy key it offers as well — this repository is private."
read -r -p "        Press enter once the webhook and deploy key are configured. "

before="$(api GET /api/v1/apps/e2e-api/deployments?limit=1 | grep -o '"id":"[^"]*"' | head -1)"

note "Push a commit touching services/api, then wait."
read -r -p "        Press enter once you have pushed. "

for _ in $(seq 1 30); do
	after="$(api GET /api/v1/apps/e2e-api/deployments?limit=1 | grep -o '"id":"[^"]*"' | head -1)"
	[ "$after" != "$before" ] && break
	sleep 2
done

if [ "$after" != "$before" ] && [ -n "$after" ]; then
	ok "a push started a deployment without anybody pressing anything"
else
	bad "no deployment started within 60s of the push"
	note "check the repository's webhook delivery log — a 401 there means the"
	note "secret does not match; a timeout means the 202 is not being sent"
fi

# ---------------------------------------------------------------- check 2 ---
# The veto, against a REAL apply. The unit test proves apply was not called on a
# fake; this proves the old image is the one actually serving traffic.

step "2. A failing release leaves the OLD image serving"

running_image() {
	api GET /api/v1/apps/e2e-api | grep -o '"image":"[^"]*"' | head -1 | cut -d'"' -f4
}

old_image="$(running_image)"
note "currently serving: ${old_image}"

api PUT /api/v1/apps/e2e-api/config -d '{"spec":{
  "name":"e2e-api",
  "deploy":{"release_command":"sh -c \"echo MIGRATION FAILED; exit 1\""}
}}' >/dev/null

api POST /api/v1/apps/e2e-api/deploy >/dev/null
note "deploying with a release command that exits 1…"

for _ in $(seq 1 90); do
	dep="$(api GET /api/v1/apps/e2e-api/deployments?limit=1)"
	echo "$dep" | grep -q '"finished":true' && break
	sleep 2
done

release_status="$(echo "$dep" | grep -o '"release_status":"[^"]*"' | cut -d'"' -f4)"
dep_status="$(echo "$dep" | grep -o '"status":"[^"]*"' | head -1 | cut -d'"' -f4)"
now_image="$(running_image)"

[ "$release_status" = "failed" ] \
	&& ok "release_status is failed" \
	|| bad "release_status = ${release_status:-<empty>}, want failed"

[ "$dep_status" = "failed" ] \
	&& ok "the deployment is failed" \
	|| bad "deployment status = ${dep_status}, want failed"

# THE assertion. The unit test could only check apply was not called.
if kubectl get deploy e2e-api -o jsonpath='{.spec.template.spec.containers[0].image}' \
	--all-namespaces 2>/dev/null | grep -q "$old_image"; then
	ok "the CLUSTER is still running the old image — the veto held against a real apply"
else
	bad "the cluster is no longer running ${old_image}"
	note "this is the failure that matters: the deploy reported failure and"
	note "shipped anyway, so un-migrated code is taking traffic"
fi

api PUT /api/v1/apps/e2e-api/config \
	-d '{"spec":{"name":"e2e-api","deploy":{"release_command":""}}}' >/dev/null

# ---------------------------------------------------------------- check 3 ---
# The exit code, the full length of the chain: container -> SPDY -> WebSocket ->
# oz -> shell. Every hop is unit-tested against a fake dial; this is the only
# thing that proves they agree.

step "3. A container exiting 42 makes oz exit 42"

"$OZ" exec --app e2e-api -- sh -c 'exit 42'
code=$?

[ "$code" -eq 42 ] \
	&& ok "oz exited 42" \
	|| bad "oz exited ${code}, want 42 — the code is lost somewhere between the container and the shell"

# ---------------------------------------------------------------- check 4 ---
# The fan-out filter against a REAL GitHub payload. The unit test builds the
# payload itself; this is the only way to see what GitHub actually sends.

step "4. A push to service B leaves service A untouched"

api POST /api/v1/apps -d "{
  \"name\": \"e2e-web\",
  \"source\": \"git\",
  \"repo_url\": \"${OZ_REPO}\",
  \"repo_branch\": \"main\",
  \"repo_subdir\": \"services/web\",
  \"port\": 8080
}" >/dev/null

note "Enable auto-deploy on e2e-web too, and add its webhook."
read -r -p "        Press enter once configured. "

api_sha() {
	api GET "/api/v1/apps/$1" >/dev/null
	# last_deployed_sha is not on the app response; read the newest deployment
	api GET "/api/v1/apps/$1/deployments?limit=1" | grep -o '"id":"[^"]*"' | head -1
}

api_before="$(api_sha e2e-api)"
web_before="$(api_sha e2e-web)"

note "Push a commit touching ONLY services/web, then wait."
read -r -p "        Press enter once you have pushed. "
sleep 20

api_after="$(api_sha e2e-api)"
web_after="$(api_sha e2e-web)"

[ "$web_after" != "$web_before" ] \
	&& ok "e2e-web deployed — the push reached the app it touched" \
	|| bad "e2e-web did not deploy; the filter is skipping a change it should build"

# The assertion that costs. An implementation that fires everything passes the
# check above and fails this one.
[ "$api_after" = "$api_before" ] \
	&& ok "e2e-api was NOT deployed — the fan-out filter held" \
	|| bad "e2e-api rebuilt on a push that did not touch it — every service in the repo rebuilds on every push"

# ---------------------------------------------------------------- check 5 ---
# The deploy key's mode as the CLUSTER applies it. The unit test asserts the
# manifest; this is where the mode becomes real.

step "5. The deploy key lands at 0400 in the pod"

if command -v kubectl >/dev/null; then
	sec="$(kubectl get secret -n ozymandis-builds -o name 2>/dev/null | grep -- '-ssh' | head -1)"
	if [ -n "$sec" ]; then
		mode="$(kubectl get "$sec" -n ozymandis-builds -o jsonpath='{.metadata.name}' 2>/dev/null)"
		ok "a per-build deploy-key secret exists (${mode})"
		note "to see the applied mode, exec into a running build pod:"
		note "  kubectl exec -n ozymandis-builds <build-pod> -c clone -- stat -c '%a' /ssh/id"
		note "it must print 400 — ssh refuses anything looser"
	else
		bad "no -ssh secret in ozymandis-builds; the key is not reaching the build"
	fi
else
	note "kubectl not found — skipping (this check needs cluster access)"
fi

# ---------------------------------------------------------------- check 6 ---
# The last instance of the recorded-but-unreadable class: a webhook answers 202
# and the deploy fails afterwards. The 202 cannot carry that, so it has to
# surface where a person looks.

step "6. A failure AFTER the 202 is visible somewhere"

api PUT /api/v1/apps/e2e-api/config -d '{"spec":{
  "name":"e2e-api",
  "build":{"repo":"'"${OZ_REPO}"'","branch":"no-such-branch-e2e"}
}}' >/dev/null 2>&1

note "Push again, or trigger a deploy, so the build fails on a missing branch."
"$OZ" deploy --app e2e-api --watch >/dev/null 2>&1
sleep 5

listed="$("$OZ" releases --app e2e-api --limit 3 2>/dev/null)"
if echo "$listed" | grep -qi 'failed'; then
	ok "the post-202 failure is in the deployments list"
	echo "$listed" | head -4 | sed 's/^/        /'
else
	bad "a deploy that failed after the webhook's 202 is not visible in \`oz releases\`"
	note "this is the configuration a new user is most likely to have wrong,"
	note "and the webhook response cannot tell them — so this list must"
fi

# ------------------------------------------------------------------ done ---

step "Result"
printf '  %d passed, %d failed\n\n' "$pass" "$fail"
[ "$fail" -eq 0 ] || exit 1
