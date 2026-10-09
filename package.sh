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
ExecStart=/usr/bin/linchange --config /etc/linchange/linchange.yaml
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

# 5. Create default config (will be overwritten by user)
cat > "${INSTALL_ROOT}/etc/linchange/linchange.yaml" <<EOF
# linchange configuration
# Copy this to /etc/linchange/linchange.yaml and edit

# Database connection
database:
  host: localhost
  port: 5432
  name: linchange
  user: linchange
  password: changeme
  sslmode: disable
  max_open_conns: 25
  max_idle_conns: 5

# HTTP server
http:
  address: ":58080"
  tls_address: ":58443"
  tls_cert: "/etc/linchange/tls/cert.pem"
  tls_key: "/etc/linchange/tls/key.pem"
  static_dir: "/usr/share/linchange/web"

# Mail protocols
imap:
  address: ":143"
  tls_address: ":993"
  tls_cert: "/etc/linchange/tls/cert.pem"
  tls_key: "/etc/linchange/tls/key.pem"

pop3:
  address: ":110"
  tls_address: ":995"

smtp:
  address: ":25"
  tls_address: ":465"
  submission_address: ":587"

# MAPI/HTTP (Outlook)
mapihttp:
  enabled: true

# CalDAV
caldav:
  enabled: true

# Autodiscover
autodiscover:
  enabled: true

# EWS (Exchange Web Services)
ews:
  enabled: true

# ActiveSync
eas:
  enabled: true

# Blob storage (message bodies)
blobstore:
  root: "/var/lib/linchange/blobs"

# DKIM
dkim:
  private_key_path: "/etc/linchange/dkim"
  selector: "default"

# LDAP (optional)
ldap:
  enabled: false
  host: "ldap.example.com"
  port: 636
  use_tls: true
  bind_dn: "cn=admin,dc=example,dc=com"
  bind_password: "changeme"
  base_dn: "dc=example,dc=com"
  user_filter: "(mail=%s)"
  group_filter: "(member=%s)"

# Outbound mail delivery
outbound:
  max_concurrent_deliveries: 20
  retry_interval: "5m"
  max_retries: 10
  bounce_delay: "4h"

# Logging
log:
  level: "info"
  format: "json"
  output: "/var/log/linchange/linchange.log"

# Admin
admin:
  # Domain admin group DN for automatic admin grant
  domain_admins_group_dn: ""
EOF

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
Recommends: postgresql (>= 14), nginx, certbot
Conflicts: courier-imap, courier-pop, dovecot-imapd, dovecot-pop3d, postfix, exim4
Provides: mail-transport-agent, imap-server, pop3-server
Section: mail
Priority: optional
EOF

# 10. Create DEBIAN/postinst
cat > "${DEBIAN_DIR}/postinst" <<'EOF'
#!/bin/bash
set -e

# Create user/group via sysusers
if command -v systemd-sysusers >/dev/null; then
    systemd-sysusers /usr/lib/sysusers.d/linchange.conf
fi

# Create directories via tmpfiles
if command -v systemd-tmpfiles >/dev/null; then
    systemd-tmpfiles --create /usr/lib/tmpfiles.d/linchange.conf
fi

# Set permissions on config directory
chown root:linchange /etc/linchange
chmod 750 /etc/linchange

# Reload systemd
systemctl daemon-reload

# Enable service (but don't start - user must configure first)
systemctl enable linchange >/dev/null 2>&1 || true

echo "linchange installed. Configure /etc/linchange/linchange.yaml then run: systemctl start linchange"
EOF
chmod 755 "${DEBIAN_DIR}/postinst"

# 11. Create DEBIAN/prerm
cat > "${DEBIAN_DIR}/prerm" <<'EOF'
#!/bin/bash
set -e

# Stop service on removal
if [ "$1" = "remove" ] || [ "$1" = "purge" ]; then
    systemctl stop linchange >/dev/null 2>&1 || true
    systemctl disable linchange >/dev/null 2>&1 || true
fi
EOF
chmod 755 "${DEBIAN_DIR}/prerm"

# 12. Create DEBIAN/postrm
cat > "${DEBIAN_DIR}/postrm" <<'EOF'
#!/bin/bash
set -e

if [ "$1" = "purge" ]; then
    # Remove user/group
    if command -v systemd-sysusers >/dev/null; then
        systemd-sysusers --purge /usr/lib/sysusers.d/linchange.conf 2>/dev/null || true
    fi
    # Remove directories
    rm -rf /var/lib/linchange /var/log/linchange /etc/linchange
    # Remove systemd unit
    systemctl daemon-reload >/dev/null 2>&1 || true
fi
EOF
chmod 755 "${DEBIAN_DIR}/postrm"

# 13. Build the .deb
echo "Building .deb package..."
dpkg-deb --build --root-owner-group "${PKG_DIR}" "${OUTPUT_DIR}/${PROJECT_NAME}_${VERSION}_${ARCH}.deb"

echo "Package created: ${OUTPUT_DIR}/${PROJECT_NAME}_${VERSION}_${ARCH}.deb"
echo ""
echo "Install with: sudo dpkg -i ${OUTPUT_DIR}/${PROJECT_NAME}_${VERSION}_${ARCH}.deb"
echo "Then configure: sudo editor /etc/linchange/linchange.yaml"
echo "Start with: sudo systemctl start linchange"