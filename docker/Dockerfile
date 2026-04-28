FROM golang:1.24-bookworm AS builder

ARG VERSION=unknown

WORKDIR /build

# Copy go mod files first for better caching
COPY go.mod go.sum ./
RUN go mod download

# Copy only required source files for build
COPY cmd/solimen cmd/solimen
COPY pkg/ pkg/

RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o solimen ./cmd/solimen

FROM debian:bookworm-slim

ARG VERSION=unknown

LABEL org.opencontainers.image.version=${VERSION}

RUN adduser --disabled-password --gecos "" --shell /usr/sbin/nologin appuser

RUN apt-get update && apt-get install -y --no-install-recommends \
    chromium \
    # Fonts for proper rendering
    fonts-liberation \
    fonts-noto-color-emoji \
    # Additional libraries for Chromium
    libnss3 \
    libnspr4 \
    libatk1.0-0 \
    libatk-bridge2.0-0 \
    libcups2 \
    libdrm2 \
    libxkbcommon0 \
    libxcomposite1 \
    libxdamage1 \
    libxfixes3 \
    libxrandr2 \
    libgbm1 \
    libpango-1.0-0 \
    libcairo2 \
    libasound2 \
    libatspi2.0-0 \
    # Additional X11 libraries
    libx11-6 \
    libxcb1 \
    libx11-xcb1 \
    xvfb \
    # Process management
    supervisor \
    # Health check
    wget \
  && apt-get clean \
  && rm -rf /var/lib/apt/lists/* /tmp/* /var/tmp/*

WORKDIR /app

# Copy the binary from builder
COPY --from=builder /build/solimen /app/solimen

# Copy the extension directory
COPY --from=builder /build/cmd/solimen/extension /app/extension

# Copy supervisor configuration
COPY docker/solimen/supervisord.conf /etc/supervisor/conf.d/supervisord.conf
COPY docker/solimen/.env /app/.env
COPY docker/solimen/.version /app/.version

# Handle directories and lock down permissions
RUN mkdir -p /var/log/supervisor \
  && chown -R appuser:appuser /app \
  && chmod 550 /app/solimen

# Expose the API port
EXPOSE 5011

HEALTHCHECK --interval=15s --timeout=5s --start-period=30s --retries=3 \
  CMD wget -q --spider http://localhost:${SOLIMEN_PORT:-5011}/health || exit 1

# Use supervisor to manage solimen
CMD ["/usr/bin/supervisord", "-c", "/etc/supervisor/conf.d/supervisord.conf"]
