#!/bin/sh
# Run the Bridge as PUID:PGID (default 1000:1000; Unraid: 99:100) so the files it
# writes into the libraries belong to the same user as the rest of the share.
# The container starts as root only to own /bridge-data (the Bridge's own data,
# never a library) and to drop privileges; the Bridge never runs as root.
# Files it creates are 0644 and folders 0755 (umask 022), so the server reads
# them whatever user it runs as.
#
# Started with an explicit non-root user (docker --user / compose user:), it
# skips the chown and the drop: that user must own the data folder (the image's
# own /bridge-data is 1000:1000's, the default PUID:PGID; a bind mount must be
# the user's).
set -e
umask 022

if [ "$(id -u)" = "0" ]; then
  PUID="${PUID:-1000}"
  PGID="${PGID:-1000}"
  mkdir -p /bridge-data
  # Only when the folder isn't the user's yet (a first start, or a changed
  # PUID/PGID): a walk of the whole data folder on every start grows with it.
  if [ "$(stat -c %u:%g /bridge-data)" != "${PUID}:${PGID}" ]; then
    chown -R "${PUID}:${PGID}" /bridge-data 2>/dev/null || true
    # The data folder holds the page's password hash and sessions (bridge.db,
    # 0644 under umask 022) beside the sealed secrets: the owner's alone.
    chmod 0700 /bridge-data 2>/dev/null || true
  fi
  exec setpriv --reuid="${PUID}" --regid="${PGID}" --clear-groups --inh-caps=-all "$@"
fi

# Say why before the Bridge fails to open its database there (a --data
# elsewhere is fine, so this only warns).
if [ -n "${AUDIOSILO_BRIDGE_DATA:-}" ] && [ ! -w "${AUDIOSILO_BRIDGE_DATA}" ]; then
  echo "audiosilo-bridge: ${AUDIOSILO_BRIDGE_DATA} isn't writable by $(id -u):$(id -g)." \
    "Mount a data folder that user owns, or start the container as root with PUID/PGID set." >&2
fi
exec "$@"
