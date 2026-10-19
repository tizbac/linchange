#!/bin/bash
# linchange .deb package builder
# Usage: ./package.sh [version] [--output-dir DIR]

set -euo pipefail

# Defaults
VERSION="${1:-$(git describe --tags --always --dirty 2>/dev/null || echo 'dev')}"
OUTPUT_DIR="${2:-/tmp/linchange-pkg}"
PROJECT_NAME="linchange"
MAINTAINER="linchange <admin@linchange.example>"
DESCRIPTION="Self-hosted mail server with webmail, calendar, and admin panel"
HOMEPAGE="https://github.com/linchange/linchange"
LICENSE="GPL-3.0-or-later"

# Architecture
ARCH=$(dpkg --print-architecture)

# Paths
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BUILD_DIR="${OUTPUT_DIR}/build"
PKG_DIR="${BUILD_DIR}/${PROJECT_NAME}_${VERSION}_${ARCH}"
DEBIAN_DIR="${PKG_DIR}/DEBIAN"
INSTALL_ROOT="${PKG_DIR}"

# Clean previous build
rm -rf "${BUILD_DIR}"
mkdir -p "${DEBIAN_DIR}"
mkdir -p "${INSTALL_ROOT}/usr/bin"
mkdir -p "${INSTALL_ROOT}/usr/lib/systemd/system"
mkdir -p "${INSTALL_ROOT}/etc/linchange"
mkdir -p "${INSTALL_ROOT}/usr/share/linchange/web"
mkdir -p "${INSTALL_ROOT}/var/lib/linchange"
mkdir -p "${INSTALL_ROOT}/var/log/linchange"

echo "Building ${PROJECT_NAME}_${VERSION}_${ARCH}.deb"

# 1. Build the Go binary
echo "Building Go binary..."
cd "${SCRIPT_DIR}"
CGO_ENABLED=0 go build -ldflags="-s -w -X main.version=${VERSION}" -o "${INSTALL_ROOT}/usr/bin/linchange" ./cmd/linchange

# 2. Build the web frontend
echo "Building web frontend..."
cd "${SCRIPT_DIR}/web"
npm ci
npm run build
cp -r dist/linchange-web/browser/* "${INSTALL_ROOT}/usr/share/linchange/web/"

# 3. Copy config example
cp "${SCRIPT_DIR}/config/linchange.example.yaml" "${INSTALL_ROOT}/etc/linchange/linchange.example.yaml"

# 4. Copy systemd service
cat > "${INSTALL_ROOT}/usr/lib/systemd/system/linchange.service" <<'EOF'
[Unit]
Description=linchange mail server
After=network.target postgresql.service
Wants=postgresql.service

[Service]
Type=simple
User=linchange
Group=linchange
ExecStart=/usr/bin/linchange --config /etc/linchange/linchange.yaml --web-dir /usr/share/linchange/web
Restart=on-failure
RestartSec=5
LimitNOFILE=65536
# Security hardening
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/var/lib/linchange /var/log/linchange
CapabilityBoundingSet=CAP_NET_BIND_SERVICE

[Install]
WantedBy=multi-user.target
EOF

# 5. Default configuration is NOT shipped: /etc/linchange/linchange.yaml is
#    created by "linchange setup" (or by hand from the example below), because
#    a pre-filled file with a database password in it would be the same on
#    every installation. The example ships alongside, documented.

# 6. Create tmpfiles.d for directories
mkdir -p "${INSTALL_ROOT}/usr/lib/tmpfiles.d"
cat > "${INSTALL_ROOT}/usr/lib/tmpfiles.d/linchange.conf" <<'EOF'
d /var/lib/linchange 0750 linchange linchange -
d /var/lib/linchange/blobs 0750 linchange linchange -
d /var/log/linchange 0750 linchange linchange -
d /etc/linchange 0750 root linchange -
EOF

# 7. Create sysusers.d for user/group
mkdir -p "${INSTALL_ROOT}/usr/lib/sysusers.d"
cat > "${INSTALL_ROOT}/usr/lib/sysusers.d/linchange.conf" <<'EOF'
u linchange - "linchange mail server" /var/lib/linchange
g linchange - "linchange mail server"
EOF

# 8. Create logrotate config
mkdir -p "${INSTALL_ROOT}/etc/logrotate.d"
cat > "${INSTALL_ROOT}/etc/logrotate.d/linchange" <<'EOF'
/var/log/linchange/*.log {
    daily
    missingok
    rotate 14
    compress
    delaycompress
    notifempty
    create 0640 linchange linchange
    sharedscripts
    postrotate
        systemctl reload linchange > /dev/null 2>&1 || true
    endscript
}
EOF

# 9. Create DEBIAN/control
cat > "${DEBIAN_DIR}/control" <<EOF
Package: ${PROJECT_NAME}
Version: ${VERSION}
Architecture: ${ARCH}
Maintainer: ${MAINTAINER}
Description: ${DESCRIPTION}
 A complete self-hosted mail server with:
  - SMTP/IMAP/POP3
  - MAPI/HTTP (Outlook native)
  - CalDAV/CardDAV
  - ActiveSync (EAS)
  - EWS (Exchange Web Services)
  - Autodiscover
  - Webmail with rich text editor
  - Calendar with scheduling assistant
  - Admin panel for domains, users, aliases, shared mailboxes
  - DKIM signing
  - LDAP authentication
  - Full-text search
Homepage: ${HOMEPAGE}
License: ${LICENSE}
Depends: systemd (>= 247), postgresql-client (>= 14), ca-certificates, tzdata
Recommends: postgresql (>= 14)
Conflicts: courier-imap, courier-pop, dovecot-imapd, dovecot-pop3d, postfix, exim4
Provides: mail-transport-agent, imap-server, pop3-server
Section: mail
Priority: optional
EOF

# 10. Maintainer scripts, shared with the Docker packaging path so the two
#     cannot drift apart. They live in the repository where they can be read,
#     linted and run before packaging.
for script in postinst prerm postrm; do
    cp "${SCRIPT_DIR}/packaging/debian/${script}" "${DEBIAN_DIR}/${script}"
    chmod 755 "${DEBIAN_DIR}/${script}"
done

# 12. Build the .deb
echo "Building .deb package..."
dpkg-deb --build --root-owner-group "${PKG_DIR}" "${OUTPUT_DIR}/${PROJECT_NAME}_${VERSION}_${ARCH}.deb"

echo "Package created: ${OUTPUT_DIR}/${PROJECT_NAME}_${VERSION}_${ARCH}.deb"
echo ""
echo "Install with: sudo dpkg -i ${OUTPUT_DIR}/${PROJECT_NAME}_${VERSION}_${ARCH}.deb"
echo "Then configure: linchange setup --config /etc/linchange/linchange.yaml"
echo "Start with: sudo systemctl start linchange"