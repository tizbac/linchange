# Linchange Dockerfile
# Multi-stage build: build frontend, build backend, create final image

# =============================================================================
# Stage 1: Build the Angular frontend
# =============================================================================
FROM node:22-alpine AS frontend-builder

WORKDIR /app/web

# Match the npm major declared in web/package.json's packageManager. Node 22's
# bundled npm 10 resolves optional platform bindings differently and rejects the
# npm 12 lockfile; using the same version here keeps `npm ci` reproducible.
RUN npm install -g npm@11.20.0

# Copy package files and the install-time scripts.
#
# npm ci runs the postinstall hook, and that hook is a file in web/scripts, so
# it has to be on disk before the install rather than arriving with the rest of
# the source. The destination is spelled out because a bare "./" would copy the
# directory's contents into the working directory and leave the hook where npm
# cannot find it.
COPY web/package*.json ./
COPY web/scripts/ ./scripts/

# Install dependencies
RUN npm ci

# Copy source
COPY web/ .

# Build the Angular app
RUN npm run build

# =============================================================================
# Stage 2: Build the Go backend
# =============================================================================
FROM golang:1.27-alpine AS backend-builder

WORKDIR /app

# Install build dependencies
RUN apk add --no-cache \
    gcc \
    musl-dev \
    git \
    make

# Copy go modules first for caching
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build the binary
RUN CGO_ENABLED=1 go build -o linchange ./cmd/linchange/

# =============================================================================
# Stage 3: Final runtime image
# =============================================================================
FROM alpine:3.21 AS runtime

# Install runtime dependencies
RUN apk add --no-cache \
    ca-certificates \
    tzdata \
    postgresql-client \
    # For healthchecks
    curl \
    # For timezone support
    && rm -rf /var/cache/apk/*

# Create non-root user
RUN addgroup -g 1000 linchange && \
    adduser -D -u 1000 -G linchange linchange

WORKDIR /app

# Copy binary from builder
COPY --from=backend-builder /app/linchange .

# Copy built frontend assets
COPY --from=frontend-builder /app/web/dist/linchange-web/browser ./static

# Copy example config for reference
COPY --from=backend-builder /app/config/linchange.example.yaml ./config/linchange.example.yaml

# Create data directory
RUN mkdir -p /var/lib/linchange && \
    chown -R linchange:linchange /var/lib/linchange /app

# Switch to non-root user
USER linchange

# Expose ports (operator maps these via docker run -p or compose)
# 25     - SMTP inbound (requires root/privileged for <1024, operator handles)
# 587    - SMTP submission
# 465    - SMTP submission TLS
# 143    - IMAP
# 993    - IMAPS
# 110    - POP3
# 995    - POP3S
# 8080   - HTTP
# 8443   - HTTPS
EXPOSE 25 143 587 993 110 995 8080 8443
# STUN/TURN for calls. The relay range is published by docker-compose rather
# than EXPOSE, which cannot name a range.
EXPOSE 3478/tcp 3478/udp

# Health check
HEALTHCHECK --interval=30s --timeout=10s --start-period=40s --retries=3 \
    CMD curl -f http://localhost:8080/healthz || exit 1

# Environment variables with defaults
ENV LINCHANGE_CONFIG=/app/config/linchange.yaml \
    LINCHANGE_DATA_DIR=/var/lib/linchange

# The config file MUST be provided by the operator (via volume mount or config map)
# No default config is provided - the server will fail fast if missing
ENTRYPOINT ["./linchange"]

# Default command runs the server with the config file. --web-dir points at the
# web app built by the frontend stage above; without it the server has nothing
# to serve at / and every browser request is a 404.
CMD ["--config", "/app/config/linchange.yaml", "--web-dir", "/app/static"]