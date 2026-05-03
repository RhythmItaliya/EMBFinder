#!/bin/sh
# postinstall.sh — run after deb/rpm package installation
set -e

# Install systemd service only if systemd is present
if [ -d /etc/systemd/system ]; then
  cat > /etc/systemd/system/embfinder.service << 'EOF'
[Unit]
Description=EMBFinder — Embroidery Visual Search Engine
After=network.target

[Service]
Type=simple
ExecStart=/usr/local/bin/embfinder
Restart=on-failure
RestartSec=5
# Run in headless mode (no browser auto-open for server installs)
Environment=HEADLESS=1
Environment=MODE=production
# Load user configuration if present
EnvironmentFile=-/etc/embfinder/env.example

[Install]
WantedBy=multi-user.target
EOF

  systemctl daemon-reload
  echo ""
  echo "EMBFinder installed successfully."
  echo ""
  echo "To run as a system service:"
  echo "  sudo systemctl enable --now embfinder"
  echo "  sudo systemctl status embfinder"
  echo ""
  echo "To run manually:"
  echo "  embfinder          (opens browser automatically)"
  echo "  HEADLESS=1 embfinder  (background HTTP server)"
  echo ""
  echo "Web UI: http://localhost:8765"
  echo ""
  echo "Note: Python services (AI embedder + renderer) must be started separately."
  echo "  docker compose up -d embedder emb-engine"
fi
