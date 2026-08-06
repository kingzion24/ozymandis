#!/bin/sh
#
# Ozymandis installer.
#
#   curl -sSL https://kingzion24.github.io/ozymandis/install.sh | sudo sh
#
# Provisions K3s, Postgres, and Ozymandis itself as a systemd unit on a fresh
# Debian or Ubuntu box. Safe to re-run: it replaces the binary and the unit and
# leaves every generated secret alone.
#
# Flags (when piped, pass them after `sh -s --`):
#   --version vX.Y.Z   install a specific release rather than the latest
#   --port N           listen port, default 8080
#   --rotate-token     issue a new dashboard token instead of keeping the old one
#   --skip-k3s         do not install K3s; use the kubeconfig already present
#   --database-url URL use an existing Postgres instead of installing one
#   --help             print this and exit

set -eu

REPO="codeblocktz/ozymandis"
INSTALL_DIR="/usr/local/bin"
CONF_DIR="/etc/ozymandis"
ENV_FILE="${CONF_DIR}/ozymandis.env"
K3S_KUBECONFIG="/etc/rancher/k3s/k3s.yaml"
KUBECONFIG_DST="${CONF_DIR}/kubeconfig"
UNIT_FILE="/etc/systemd/system/ozymandis.service"
SVC_USER="ozymandis"

VERSION=""
PORT="8080"
ROTATE_TOKEN="no"
SKIP_K3S="no"
DATABASE_URL=""

# ---------------------------------------------------------------- output ----

say()  { printf '  %s\n' "$*"; }
step() { printf '\n\033[1m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[33mwarning:\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[31merror:\033[0m %s\n' "$*" >&2; exit 1; }

# ----------------------------------------------------------------- checks ---

# require_root refuses rather than re-executing under sudo. The script arrives
# on stdin, so there is nothing to hand a second interpreter — re-reading it
# would mean a second download, and a second download is a second thing that
# could differ from the one that was audited.
require_root() {
	[ "$(id -u)" = "0" ] || die "must run as root — pipe to \`sudo sh\` rather than \`sh\`"
}

require_platform() {
	[ -r /etc/os-release ] || die "cannot read /etc/os-release — unsupported system"
	# shellcheck disable=SC1091
	. /etc/os-release
	case "${ID:-}:${ID_LIKE:-}" in
		debian*|ubuntu*|*:*debian*|*:*ubuntu*) ;;
		*) die "only Debian and Ubuntu are supported, found '${ID:-unknown}'" ;;
	esac

	case "$(uname -m)" in
		x86_64|amd64)  ARCH="amd64" ;;
		aarch64|arm64) ARCH="arm64" ;;
		*) die "only amd64 and arm64 are supported, found '$(uname -m)'" ;;
	esac

	[ -d /run/systemd/system ] || die "systemd is required and is not running"
}

need_cmd() { command -v "$1" >/dev/null 2>&1; }

require_tools() {
	need_cmd curl || die "curl is required"
	for c in tar sha256sum systemctl; do
		need_cmd "$c" || die "$c is required"
	done
}

# ------------------------------------------------------------------ flags ---

# Spelled out rather than read back from $0, which is "sh" when the script
# arrives on stdin — the exact case --help most needs to work in.
usage() {
	cat <<-EOF
		Ozymandis installer.

		  curl -sSL https://kingzion24.github.io/ozymandis/install.sh | sudo sh

		Provisions K3s, Postgres, and Ozymandis as a systemd unit on Debian or
		Ubuntu. Safe to re-run: it replaces the binary and the unit and leaves
		every generated secret alone.

		Flags (when piped, pass them after 'sh -s --'):
		  --version vX.Y.Z    install a specific release rather than the latest
		  --port N            listen port, default 8080
		  --rotate-token      issue a new dashboard token instead of keeping it
		  --skip-k3s          use the kubeconfig already present
		  --database-url URL  use an existing Postgres instead of installing one
		  --help              print this and exit
	EOF
}

parse_flags() {
	while [ $# -gt 0 ]; do
		case "$1" in
			--version)      VERSION="${2:-}"; shift 2 ;;
			--port)         PORT="${2:-}"; shift 2 ;;
			--database-url) DATABASE_URL="${2:-}"; shift 2 ;;
			--rotate-token) ROTATE_TOKEN="yes"; shift ;;
			--skip-k3s)     SKIP_K3S="yes"; shift ;;
			--help|-h)      usage; exit 0 ;;
			*) die "unknown flag '$1'" ;;
		esac
	done
	case "$PORT" in
		''|*[!0-9]*) die "--port must be a number, got '$PORT'" ;;
	esac
}

# ----------------------------------------------------------------- secrets --

# env_get reads a value out of the existing environment file.
#
# Re-running the installer must never mint a new OZYMANDIS_SECRET_KEY: it seals
# every stored secret, and losing it loses them with no recovery path. Same for
# the database password, which is the only copy anybody has.
env_get() {
	[ -f "$ENV_FILE" ] || return 1
	v=$(sed -n "s/^$1=//p" "$ENV_FILE" | head -n1)
	[ -n "$v" ] || return 1
	printf '%s' "$v"
}

rand_hex() { od -An -tx1 -N"${1:-24}" /dev/urandom | tr -d ' \n'; }
rand_b64() { head -c "${1:-32}" /dev/urandom | base64 | tr -d '\n'; }

# ---------------------------------------------------------------- release ---

resolve_version() {
	[ -n "$VERSION" ] && return 0
	step "Resolving the latest release"
	VERSION=$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" \
		| sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' \
		| head -n1) || true
	[ -n "$VERSION" ] || die "could not resolve the latest release — pass --version vX.Y.Z"
	say "$VERSION"
}

install_binary() {
	step "Installing ozymandis ${VERSION} (${ARCH})"

	num="${VERSION#v}"
	tarball="ozymandis_${num}_linux_${ARCH}.tar.gz"
	base="https://github.com/${REPO}/releases/download/${VERSION}"

	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT INT TERM

	curl -fsSL -o "${tmp}/${tarball}" "${base}/${tarball}" \
		|| die "download failed: ${base}/${tarball}"
	curl -fsSL -o "${tmp}/checksums.txt" "${base}/checksums.txt" \
		|| die "download failed: ${base}/checksums.txt"

	# Verified before it is unpacked, not after it is installed. A checksum
	# checked once the binary is already in place is a report, not a gate.
	want=$(sed -n "s/^\([0-9a-f]\{64\}\)[[:space:]]\{1,\}\*\{0,1\}${tarball}\$/\1/p" \
		"${tmp}/checksums.txt" | head -n1)
	[ -n "$want" ] || die "no checksum published for ${tarball}"
	got=$(sha256sum "${tmp}/${tarball}" | cut -d' ' -f1)
	[ "$want" = "$got" ] || die "checksum mismatch for ${tarball}: expected ${want}, got ${got}"
	say "checksum ok"

	tar -xzf "${tmp}/${tarball}" -C "$tmp" || die "could not unpack ${tarball}"
	[ -f "${tmp}/ozymandis" ] || die "${tarball} does not contain a ozymandis binary"

	install -m 0755 "${tmp}/ozymandis" "${INSTALL_DIR}/ozymandis.new"
	mv -f "${INSTALL_DIR}/ozymandis.new" "${INSTALL_DIR}/ozymandis"
	say "${INSTALL_DIR}/ozymandis"

	# The CLI, when the release carries one.
	#
	# Optional rather than required, so this script keeps working against a
	# release published before oz existed — an installer that dies on a missing
	# file turns an old tag into an uninstallable one. The server is what this
	# script is for; oz is a convenience beside it.
	if [ -f "${tmp}/oz" ]; then
		install -m 0755 "${tmp}/oz" "${INSTALL_DIR}/oz.new"
		mv -f "${INSTALL_DIR}/oz.new" "${INSTALL_DIR}/oz"
		say "${INSTALL_DIR}/oz"
	fi

	rm -rf "$tmp"
	trap - EXIT INT TERM
}

# -------------------------------------------------------------------- k3s ---

install_k3s() {
	if [ "$SKIP_K3S" = "yes" ]; then
		step "Skipping K3s (--skip-k3s)"
		[ -r "$K3S_KUBECONFIG" ] || die "--skip-k3s given but ${K3S_KUBECONFIG} is not readable"
		return 0
	fi

	if need_cmd k3s && systemctl is-active --quiet k3s; then
		step "K3s is already running"
		# Not reconfigured. If this cluster was installed with K3s's bundled
		# Traefik, it is still there and still holding :80 and :443 — see the
		# comment on the install below for why that matters.
		if k3s kubectl get deployment traefik -n kube-system >/dev/null 2>&1; then
			warn "this cluster runs K3s's bundled Traefik, which has no ACME resolver."
			say  "an ingress controller you install yourself will collide with it over"
			say  "ports 80 and 443. Remove it first:"
			say  "  k3s kubectl -n kube-system delete helmchart traefik"
		fi
	else
		step "Installing K3s"
		# --disable traefik: this installer does not own the edge.
		#
		# Ozymandis annotates its Ingresses with the name of an ACME resolver the
		# operator configures on their own ingress controller. K3s's bundled
		# Traefik ships with no resolvers at all, so leaving it in place would
		# serve every hostname the controller's built-in self-signed certificate
		# while every deploy stayed green.
		#
		# Worse, it would still be holding the host's :80 and :443. A controller
		# installed afterwards asks for the same ports and never schedules —
		# which is a deadlock that reads as a broken cluster rather than as two
		# things competing for one edge. One place configures the edge, and it is
		# not this script.
		curl -sfL https://get.k3s.io \
			| INSTALL_K3S_SKIP_SELINUX_RPM=true INSTALL_K3S_EXEC="--disable traefik" sh - \
			|| die "K3s install failed"
	fi

	step "Waiting for the node to be ready"
	i=0
	while [ "$i" -lt 60 ]; do
		if k3s kubectl wait --for=condition=Ready node --all --timeout=10s >/dev/null 2>&1; then
			say "ready"
			return 0
		fi
		i=$((i + 1))
		sleep 5
	done
	die "the K3s node did not become ready — check \`journalctl -u k3s\`"
}

# --------------------------------------------------------------- postgres ---

install_postgres() {
	if [ -n "$DATABASE_URL" ]; then
		step "Using the Postgres given on the command line"
		return 0
	fi

	# An existing DSN is reused verbatim. The role's password lives nowhere
	# else, so regenerating one would orphan the database the install is on.
	if existing=$(env_get OZYMANDIS_DATABASE_URL); then
		step "Keeping the existing database"
		DATABASE_URL="$existing"
		return 0
	fi

	step "Installing Postgres"
	if ! need_cmd psql; then
		DEBIAN_FRONTEND=noninteractive apt-get update -qq
		DEBIAN_FRONTEND=noninteractive apt-get install -y -qq postgresql \
			|| die "could not install postgresql"
	else
		say "already present"
	fi
	systemctl enable --now postgresql >/dev/null 2>&1 || true

	pw=$(rand_hex 24)
	role_exists=$(su - postgres -c \
		"psql -tAc \"SELECT 1 FROM pg_roles WHERE rolname='ozymandis'\"" 2>/dev/null || true)
	if [ "$role_exists" = "1" ]; then
		su - postgres -c "psql -qc \"ALTER ROLE ozymandis WITH LOGIN PASSWORD '${pw}'\"" >/dev/null \
			|| die "could not reset the ozymandis role password"
	else
		su - postgres -c "psql -qc \"CREATE ROLE ozymandis WITH LOGIN PASSWORD '${pw}'\"" >/dev/null \
			|| die "could not create the ozymandis role"
	fi

	db_exists=$(su - postgres -c \
		"psql -tAc \"SELECT 1 FROM pg_database WHERE datname='ozymandis'\"" 2>/dev/null || true)
	if [ "$db_exists" != "1" ]; then
		su - postgres -c "createdb -O ozymandis ozymandis" || die "could not create the ozymandis database"
	fi

	DATABASE_URL="postgres://ozymandis:${pw}@127.0.0.1:5432/ozymandis?sslmode=disable"
	say "database ready"
}

# ------------------------------------------------------------------ config --

configure() {
	step "Writing ${ENV_FILE}"

	id -u "$SVC_USER" >/dev/null 2>&1 || \
		useradd --system --no-create-home --shell /usr/sbin/nologin "$SVC_USER"

	mkdir -p "$CONF_DIR"
	chmod 0750 "$CONF_DIR"
	chown root:"$SVC_USER" "$CONF_DIR"

	# The k3s kubeconfig is root-only, and widening it would hand cluster-admin
	# to every local user. Copying gives the service its own 0600 copy instead.
	[ -r "$K3S_KUBECONFIG" ] || die "${K3S_KUBECONFIG} is not readable"
	install -o "$SVC_USER" -g "$SVC_USER" -m 0600 "$K3S_KUBECONFIG" "$KUBECONFIG_DST"

	secret_key=$(env_get OZYMANDIS_SECRET_KEY || rand_b64 32)
	if [ "$ROTATE_TOKEN" = "yes" ]; then
		auth_token=$(rand_hex 24)
	else
		auth_token=$(env_get OZYMANDIS_AUTH_TOKEN || rand_hex 24)
	fi
	owner_email=$(env_get OZYMANDIS_OWNER_EMAIL || printf '')

	# Preserved across re-runs like every other value, so an operator who named
	# their controller's resolver by hand does not lose it by upgrading.
	cert_resolver=$(env_get OZYMANDIS_CERT_RESOLVER || printf '')

	umask 077
	cat > "${ENV_FILE}.new" <<-EOF
		# Written by the Ozymandis installer. Re-running preserves every value here
		# except the port and the binary; edit freely and restart the service.
		#
		# OZYMANDIS_SECRET_KEY seals stored secrets. There is no recovery path if it
		# is lost, so back this file up before you need it.
		OZYMANDIS_DATABASE_URL=${DATABASE_URL}
		OZYMANDIS_KUBECONFIG=${KUBECONFIG_DST}
		OZYMANDIS_ADDR=:${PORT}
		OZYMANDIS_AUTH_TOKEN=${auth_token}
		OZYMANDIS_SECRET_KEY=${secret_key}
		OZYMANDIS_OWNER_EMAIL=${owner_email}

		# The ACME resolver the ingress controller obtains certificates from — a
		# name from its own certificatesResolvers configuration, such as
		# "letsencrypt". Every hostname is then issued for individually.
		#
		# Written EMPTY on a fresh install, deliberately. This script installs K3s
		# with --disable traefik and installs no ingress controller of its own, so
		# on a fresh machine there is no controller here yet and no resolver name
		# that could be correct. Naming one that does not exist does not fail:
		# hostnames are served the controller's own certificate, browsers refuse
		# it, and nothing here or in the dashboard reports why. Empty serves plain
		# http instead — visibly wrong rather than invisibly wrong. Set this once
		# you have installed a controller, to the name of ITS resolver.
		OZYMANDIS_CERT_RESOLVER=${cert_resolver}

		# Set OZYMANDIS_BASE_URL to a public https URL to turn magic-link sign-in on,
		# and OZYMANDIS_APP_DOMAIN to the domain apps get a hostname under.
		#OZYMANDIS_BASE_URL=
		#OZYMANDIS_APP_DOMAIN=
	EOF
	chown root:"$SVC_USER" "${ENV_FILE}.new"
	chmod 0640 "${ENV_FILE}.new"
	mv -f "${ENV_FILE}.new" "$ENV_FILE"

	AUTH_TOKEN="$auth_token"
	say "secrets preserved across re-runs"
}

install_unit() {
	step "Installing the systemd unit"
	cat > "$UNIT_FILE" <<-EOF
		[Unit]
		Description=Ozymandis — a self-hosted PaaS for Kubernetes
		Documentation=https://github.com/${REPO}
		After=network-online.target postgresql.service k3s.service
		Wants=network-online.target

		[Service]
		Type=simple
		User=${SVC_USER}
		Group=${SVC_USER}
		EnvironmentFile=${ENV_FILE}
		ExecStart=${INSTALL_DIR}/ozymandis
		Restart=always
		RestartSec=5

		NoNewPrivileges=yes
		PrivateTmp=yes
		ProtectSystem=full
		ProtectHome=yes
		ProtectControlGroups=yes
		ProtectKernelTunables=yes
		RestrictSUIDSGID=yes

		[Install]
		WantedBy=multi-user.target
	EOF
	systemctl daemon-reload
	systemctl enable ozymandis >/dev/null 2>&1
	systemctl restart ozymandis
	say "ozymandis.service started"
}

# ------------------------------------------------------------------ verify ---

wait_healthy() {
	step "Waiting for the dashboard"
	i=0
	while [ "$i" -lt 60 ]; do
		code=$(curl -fsS -o /dev/null -w '%{http_code}' \
			"http://127.0.0.1:${PORT}/healthz" 2>/dev/null || printf '000')
		if [ "$code" = "200" ]; then
			say "healthy"
			return 0
		fi
		# 503 is the documented answer when the process is up but the cluster is
		# not reachable yet, so it is worth continuing to wait on.
		if ! systemctl is-active --quiet ozymandis; then
			printf '\n'
			journalctl -u ozymandis -n 40 --no-pager >&2 || true
			die "ozymandis exited — see the log above"
		fi
		i=$((i + 1))
		sleep 2
	done
	printf '\n'
	journalctl -u ozymandis -n 40 --no-pager >&2 || true
	die "the dashboard did not become healthy within 2 minutes"
}

public_addr() {
	ip=$(curl -fsS --max-time 5 https://api.ipify.org 2>/dev/null || true)
	[ -n "$ip" ] || ip=$(hostname -I 2>/dev/null | awk '{print $1}')
	[ -n "$ip" ] || ip="<server-ip>"
	printf '%s' "$ip"
}

summary() {
	addr=$(public_addr)
	cat <<-EOF

		  Ozymandis ${VERSION} is running.

		    URL     http://${addr}:${PORT}
		    Token   ${AUTH_TOKEN}

		  Paste the token when the dashboard asks for it.

		    Config    ${ENV_FILE}
		    Service   systemctl status ozymandis
		    Logs      journalctl -u ozymandis -f

		  This dashboard is served over plain HTTP, so the token crosses the
		  network in the clear. Put it behind a domain and TLS before you rely
		  on it, or reach it over an SSH tunnel in the meantime:

		    ssh -L ${PORT}:127.0.0.1:${PORT} root@${addr}

		  THIS INSTALLER DOES NOT SET UP TLS, and there is no ingress controller
		  on this cluster — K3s was installed with --disable traefik. Apps you
		  deploy are reachable over plain http and nothing will report that as a
		  fault. The edge is yours to wire, in one place, deliberately:

		    1. Install an ingress controller with an ACME resolver. Traefik with
		       a Let's Encrypt resolver over TLS-ALPN-01 is what this is built
		       against.
		    2. Set OZYMANDIS_CERT_RESOLVER in ${ENV_FILE} to that resolver's
		       name, then: systemctl restart ozymandis

		  Leave OZYMANDIS_CERT_RESOLVER empty until step 1 is done. A name that
		  matches no resolver is not an error anywhere — hostnames are served the
		  controller's own certificate and every deploy still goes green. See the
		  Certificates section of the README.

		  Back up ${ENV_FILE}. OZYMANDIS_SECRET_KEY seals your stored secrets and
		  cannot be regenerated.

	EOF
}

# -------------------------------------------------------------------- main ---

main() {
	parse_flags "$@"
	require_root
	require_platform
	require_tools

	printf '\n\033[1mOzymandis installer\033[0m — %s/%s\n' "$(uname -s)" "$ARCH"

	resolve_version
	install_binary
	install_k3s
	install_postgres
	configure
	install_unit
	wait_healthy
	summary
}

# Called on the last line so a truncated download runs nothing. Everything
# above is a definition; a connection that drops midway leaves a shell that has
# learned some functions and provisioned no part of a machine.
main "$@"
