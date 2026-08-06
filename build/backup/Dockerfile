# The image backup and restore tasks run in.
#
# One image with both restic and the Postgres client, because a database backup
# is `pg_dump | restic backup --stdin` — a single pipeline. Splitting it across
# two containers would mean the dump has to land on a disk in between, sized for
# the database, in a pod that has no such disk.
#
# Built from Alpine and the distribution's own packages rather than by
# downloading release tarballs: the package manager checks signatures, and a
# `curl | tar` in a Dockerfile does not.
FROM alpine:3.22

# Pinned, so a rebuild of this image produces the same tools. Restic's
# repository format is versioned and a repository written by a newer restic
# cannot be read by an older one — an image that moved on its own could upgrade
# a repository overnight and leave other installs unable to restore from it.
#
# postgresql16-client to match the postgres:16-alpine the Postgres source
# deploys. pg_dump refuses a server newer than itself, so these move together.
RUN apk add --no-cache \
      restic=0.18.0-r7 \
      postgresql16-client \
      ca-certificates \
 && rm -rf /var/cache/apk/*

# The uid tasks run as, matching backup.RunAsUser. The security posture refuses
# a container that would run as root, and restic needs somewhere to write its
# cache — so the user exists here rather than being invented by the pod spec,
# which cannot create a home directory.
RUN adduser -D -u 65532 -h /home/backup backup
USER 65532:65532

# Restic writes its cache here. Also set in the task's environment; repeated in
# the image so the same container is usable by hand when somebody is debugging a
# failed restore, which is exactly when nobody wants to reconstruct an
# environment.
ENV RESTIC_CACHE_DIR=/tmp/restic-cache

ENTRYPOINT ["/bin/sh"]
