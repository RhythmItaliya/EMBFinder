#!/bin/sh
# preremove.sh — run before deb/rpm package removal
set -e

if [ -d /etc/systemd/system ] && [ -f /etc/systemd/system/embfinder.service ]; then
  systemctl stop    embfinder 2>/dev/null || true
  systemctl disable embfinder 2>/dev/null || true
  rm -f /etc/systemd/system/embfinder.service
  systemctl daemon-reload
fi
