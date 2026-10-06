#!/bin/sh
#
# Ozymandis upgrader.
#
#   curl -sSL https://kingzion24.github.io/ozymandis/upgrade.sh | sudo sh
#
# Replaces the binary and restarts the service. It does not touch K3s, Postgres,
# the environment file, or the unit — an upgrade that re-provisions the machine
# is an install, and this is deliberately the smaller, duller operation.
#
# If the new version does not come up healthy, the previous binary is put back
# and the service restarted on it, so a bad release costs a restart rather than
# an outage.
#
# Flags (when piped, pass them after `sh -s --`):
#   --version vX.Y.Z   upgrade (or downgrade) to a specific release
#   --help             print this and exit

set -eu

REPO="kingzion24/ozymandis"
INSTALL_DIR="/usr/local/bin"
BIN="${INSTALL_DIR}/ozymandis"
PREV="${INSTALL_DIR}/ozymandis.prev"
CLI_BIN="${INSTALL_DIR}/oz"
# The CLI is staged beside its destination rather than left in the temporary
# directory, because that directory is removed before the health check and the
# CLI is installed after it.
CLI_STAGED="${INSTALL_DIR}/oz.staged"
ENV_FILE="/etc/ozymandis/ozymandis.env"
# Database dumps taken before each upgrade. Root-only: a dump holds every
# sealed secret and every password hash.
BACKUP_DIR="/var/lib/ozymandis/backups"
BACKUPS_KEPT=3

VERSION=""

say()  { printf '  %s\n' "$*"; }
step() { printf '\n\033[1m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[33mwarning:\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[31merror:\033[0m %s\n' "$*" >&2; exit 1; }

need_cmd() { command -v "$1" >/dev/null 2>&1; }

# Spelled out rather than read back from $0, which is "sh" when the script
# arrives on stdin — the exact case --help most needs to work in.
usage() {
	cat <<-EOF
		Ozymandis upgrader.

		  curl -sSL https://kingzion24.github.io/ozymandis/upgrade.sh | sudo sh

		Replaces the binary and restarts the service. It does not touch K3s,
		Postgres, the environment file, or the unit. The database is dumped to
		${BACKUP_DIR} first. If the new version does not come up healthy the
		previous binary is put back.

		Flags (when piped, pass them after 'sh -s --'):
		  --version vX.Y.Z    upgrade or downgrade to a specific release
		  --help              print this and exit
	EOF
}

parse_flags() {
	while [ $# -gt 0 ]; do
		case "$1" in
			--version) VERSION="${2:-}"; shift 2 ;;
			--help|-h) usage; exit 0 ;;
			*) die "unknown flag '$1'" ;;
		esac
	done
}

# backup_database dumps the control plane's database before the new binary is
# allowed to migrate it.
#
# The rollback below restores a binary and nothing else. Migrations run at
# startup and only go forwards, so a release that migrates and then turns out
# bad leaves the previous binary on a schema it was not written for, and until
# now there was nothing to go back to. This is that something.
#
# Best effort, and loud when it does not happen. An install on a managed
# database may have no pg_dump here, or one too old for the server; refusing to
# upgrade over that would strand it on the version it has. But it must not pass
# silently either, because the operator is about to rely on a rollback that has
# just become half of one.
backup_database() {
	step "Backing up the database"

	dsn=$(sed -n 's/^OZYMANDIS_DATABASE_URL=//p' "$ENV_FILE" | head -n1)
	if [ -z "$dsn" ]; then
		warn "no OZYMANDIS_DATABASE_URL in ${ENV_FILE} — upgrading WITHOUT a database backup"
		return 0
	fi
	if ! need_cmd pg_dump; then
		warn "pg_dump is not installed — upgrading WITHOUT a database backup"
		return 0
	fi

	mkdir -p "$BACKUP_DIR"
	chmod 0700 "$BACKUP_DIR"
	out="${BACKUP_DIR}/pre-${VERSION}-$(date -u +%Y%m%dT%H%M%SZ).sql.gz"

	# Written under a temporary name and moved into place, so a dump cut short
	# never sits there looking like a backup. -w: never stop to ask for a
	# password in a script nobody is watching. -Z: pg_dump's own gzip, so its
	# exit status is the command's and not a pipeline's. --clean --if-exists so
	# the dump replaces what is there when restored, rather than colliding
	# with it.
	umask 077
	if pg_dump -w --clean --if-exists --dbname="$dsn" -Z 6 -f "${out}.part" 2>"${out}.err"; then
		mv -f "${out}.part" "$out"
		rm -f "${out}.err"
		say "saved ${out}"
	else
		warn "the database backup failed — upgrading WITHOUT one:"
		sed 's/^/    /' "${out}.err" >&2 || true
		rm -f "${out}.part" "${out}.err"
		return 0
	fi

	# Keep the newest few. Names sort by time within a version, and ls -t is
	# the order that matters across versions.
	# shellcheck disable=SC2012
	ls -1t "$BACKUP_DIR"/pre-*.sql.gz 2>/dev/null \
		| tail -n +$((BACKUPS_KEPT + 1)) \
		| while IFS= read -r old; do rm -f "$old"; done
}

require_installed() {
	[ "$(id -u)" = "0" ] || die "must run as root — pipe to \`sudo sh\` rather than \`sh\`"
	[ -x "$BIN" ] || die "no ozymandis binary at ${BIN} — run the installer first"
	[ -f "$ENV_FILE" ] || die "no config at ${ENV_FILE} — run the installer first"
	systemctl list-unit-files ozymandis.service >/dev/null 2>&1 \
		|| die "ozymandis.service is not installed — run the installer first"
	for c in curl tar sha256sum systemctl; do
		need_cmd "$c" || die "$c is required"
	done

	case "$(uname -m)" in
		x86_64|amd64)  ARCH="amd64" ;;
		aarch64|arm64) ARCH="arm64" ;;
		*) die "only amd64 and arm64 are supported, found '$(uname -m)'" ;;
	esac
}

# port reads the listen port out of the config rather than assuming 8080, so an
# install that moved it is still health-checked on the port it actually serves.
port() {
	p=$(sed -n 's/^OZYMANDIS_ADDR=.*:\([0-9]\{1,\}\)$/\1/p' "$ENV_FILE" | head -n1)
	[ -n "$p" ] || p="8080"
	printf '%s' "$p"
}

# current_version asks the running service rather than the binary on disk. The
# binary takes no flags, and /healthz reports the version that is actually
# serving — which is the one an upgrade is moving away from.
#
# Not -f: /healthz answers 503 when the cluster is unreachable, and that
# response still carries the version.
current_version() {
	v=$(curl -sS --max-time 5 "http://127.0.0.1:$(port)/healthz" 2>/dev/null \
		| sed -n 's/.*"version"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n1)
	[ -n "$v" ] || v="unknown"
	printf '%s' "$v"
}

resolve_version() {
	[ -n "$VERSION" ] && return 0
	VERSION=$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" \
		| sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' \
		| head -n1) || true
	[ -n "$VERSION" ] || die "could not resolve the latest release — pass --version vX.Y.Z"
}

fetch() {
	num="${VERSION#v}"
	tarball="ozymandis_${num}_linux_${ARCH}.tar.gz"
	base="https://github.com/${REPO}/releases/download/${VERSION}"

	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT INT TERM

	curl -fsSL -o "${tmp}/${tarball}" "${base}/${tarball}" \
		|| die "download failed: ${base}/${tarball}"
	curl -fsSL -o "${tmp}/checksums.txt" "${base}/checksums.txt" \
		|| die "download failed: ${base}/checksums.txt"

	want=$(sed -n "s/^\([0-9a-f]\{64\}\)[[:space:]]\{1,\}\*\{0,1\}${tarball}\$/\1/p" \
		"${tmp}/checksums.txt" | head -n1)
	[ -n "$want" ] || die "no checksum published for ${tarball}"
	got=$(sha256sum "${tmp}/${tarball}" | cut -d' ' -f1)
	[ "$want" = "$got" ] || die "checksum mismatch for ${tarball}: expected ${want}, got ${got}"

	tar -xzf "${tmp}/${tarball}" -C "$tmp" || die "could not unpack ${tarball}"
	[ -f "${tmp}/ozymandis" ] || die "${tarball} does not contain a ozymandis binary"
	STAGED="${tmp}/ozymandis"
	# Optional: a release published before oz existed carries no CLI, and an
	# upgrade script that died on that would make an old tag unupgradeable.
	[ -f "${tmp}/oz" ] && STAGED_CLI="${tmp}/oz"
	TMPDIR_USED="$tmp"
	say "checksum ok"
}

healthy() {
	p=$(port)
	i=0
	while [ "$i" -lt 45 ]; do
		code=$(curl -fsS -o /dev/null -w '%{http_code}' \
			"http://127.0.0.1:${p}/healthz" 2>/dev/null || printf '000')
		[ "$code" = "200" ] && return 0
		systemctl is-active --quiet ozymandis || return 1
		i=$((i + 1))
		sleep 2
	done
	return 1
}

rollback() {
	printf '\033[33m==>\033[0m New version unhealthy — rolling back\n' >&2
	# Discarded rather than installed: a client from a version that never ran
	# healthy on this machine has no business being on it.
	rm -f "$CLI_STAGED"
	mv -f "$PREV" "$BIN"
	systemctl restart ozymandis
	if healthy; then
		printf '  restored %s\n' "$(current_version)" >&2
	else
		printf '  \033[31mrollback did not come up either\033[0m — check journalctl -u ozymandis\n' >&2
	fi
	exit 1
}

main() {
	parse_flags "$@"
	require_installed

	from=$(current_version)
	resolve_version

	step "Upgrading from ${from} to ${VERSION}"
	if [ "$from" = "$VERSION" ]; then
		say "already on ${VERSION} — reinstalling the same build"
	fi

	fetch
	backup_database

	# The outgoing binary is kept, not overwritten. It is the only copy that is
	# known to have worked on this machine.
	cp -f "$BIN" "$PREV"
	install -m 0755 "$STAGED" "${BIN}.new"
	mv -f "${BIN}.new" "$BIN"
	# Moved out of the temporary directory before it is removed, so the CLI
	# survives to be installed after the health check below.
	if [ -n "${STAGED_CLI:-}" ] && [ -f "$STAGED_CLI" ]; then
		install -m 0755 "$STAGED_CLI" "$CLI_STAGED"
	fi
	rm -rf "$TMPDIR_USED"
	trap - EXIT INT TERM

	step "Restarting"
	systemctl restart ozymandis

	if healthy; then
		# Kept, not removed. Coming up healthy is ninety seconds of evidence,
		# and a release that breaks an hour later is rolled back by hand from
		# this file — the only copy of a binary known to have worked here.
		say "previous binary kept at ${PREV}"

		# The CLI is swapped only once the server is known healthy, and is
		# deliberately outside the rollback.
		#
		# Ordering it here means a rollback leaves oz matching the server that
		# is actually running, rather than a newer client against an older
		# install. It is not part of the rollback itself because oz is a client:
		# a bad one is a broken command, not a broken install, and making the
		# health of the service depend on it would widen what a rollback has to
		# reason about for no gain.
		if [ -f "$CLI_STAGED" ]; then
			mv -f "$CLI_STAGED" "$CLI_BIN" && say "oz is now ${VERSION}"
		fi

		step "Done"
		say "ozymandis is now ${VERSION}"
		say "journalctl -u ozymandis -f"
		printf '\n'
	else
		rollback
	fi
}

main "$@"
