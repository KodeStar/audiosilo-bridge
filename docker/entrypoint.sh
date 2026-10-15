#!/bin/sh
# Run the Bridge as PUID:PGID (default 1000:1000; Unraid: 99:100) so the files it
# writes into the libraries belong to the same user as the rest of the share.
# The container starts as root only to own /bridge-data (the Bridge's own data,
# never a library) and to drop privileges; the Bridge never runs as root.
# Files it creates are 0644 and folders 0755 (umask 022), so the server reads
# them whatever user it runs as.
#
# Started with an explicit non-root user (docker --user / compose user:), it
# skips the chown and the drop.
set -e
umask 022

if [ "$(id -u)" = "0" ]; then
  PUID="${PUID:-1000}"
  PGID="${PGID:-1000}"
  mkdir -p /bridge-data
  chown -R "${PUID}:${PGID}" /bridge-data 2>/dev/null || true
  exec setpriv --reuid="${PUID}" --regid="${PGID}" --clear-groups --inh-caps=-all "$@"
fi

exec "$@"
