#!/bin/sh
set -e

if [ "$(id -u)" = "0" ]; then
    if [ -f /data/cfnat-aio.db ]; then
        chown cfnat:cfnat /data/cfnat-aio.db 2>/dev/null || true
    fi
    if [ -d /data ]; then
        chown -R cfnat:cfnat /data 2>/dev/null || true
    fi
    exec su-exec cfnat /usr/local/bin/cfnat-aio "$@"
else
    exec /usr/local/bin/cfnat-aio "$@"
fi