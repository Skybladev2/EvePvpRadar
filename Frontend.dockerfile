# Base image tag: debian:bookworm-slim (digest pins the rootfs; bump when rebuilding intentionally).
FROM debian:bookworm-slim@sha256:13cb01d584d2c23f475c088c168a48f9a08f033a10460572fbfd10912ec5ba7c

# Build-time Debian mirror overrides for the deb822 .sources format used by bookworm.
# Defaults are deb.debian.org (Fastly CDN). If that CDN is unreachable from your network,
# set --build-arg DEBIAN_MIRROR=http://mirror.yandex.ru/debian (or any reachable mirror).
#
# The two URIs in the .sources file are:
#   http://deb.debian.org/debian          (main archive)
#   http://deb.debian.org/debian-security  (security updates)
#
# You may override one or both:
#   DEBIAN_MIRROR          - main archive mirror
#   DEBIAN_SECURITY_MIRROR - security mirror (defaults to DEBIAN_MIRROR with "-security" appended)
ARG DEBIAN_MIRROR=http://deb.debian.org/debian
ARG DEBIAN_SECURITY_MIRROR=${DEBIAN_MIRROR}-security

RUN sed -i "s|URIs: http://deb.debian.org/debian$|URIs: ${DEBIAN_MIRROR}|" /etc/apt/sources.list.d/debian.sources && \
    sed -i "s|URIs: http://deb.debian.org/debian-security$|URIs: ${DEBIAN_SECURITY_MIRROR}|" /etc/apt/sources.list.d/debian.sources && \
    apt-get update && apt-get install -y --no-install-recommends \
    nginx=1.22.1-9+deb12u8 \
    libnginx-mod-http-lua=1:0.10.23-1+deb12u1 \
    libnginx-mod-http-headers-more-filter=1:0.34-3 \
    libnginx-mod-http-modsecurity=1.0.3-1+b2 \
    modsecurity-crs=3.3.4-1+deb12u1 \
    && rm -rf /var/lib/apt/lists/*

RUN mkdir -p /etc/nginx/modsecurity

# Ensure CRS rules are available at /usr/share/modsecurity-crs/rules for modsecurity.conf include.
RUN mkdir -p /usr/share/modsecurity-crs && \
    if [ ! -d /usr/share/modsecurity-crs/rules ]; then \
      if [ -d /etc/modsecurity/crs/rules ]; then \
        ln -s /etc/modsecurity/crs/rules /usr/share/modsecurity-crs/rules; \
      else \
        echo "CRS rules directory not found"; \
        exit 1; \
      fi; \
    fi

RUN rm -f /etc/nginx/sites-enabled/default

# Nginx only proxies to backend; backend serves all HTML and static assets
COPY frontend/nginx-default.conf /etc/nginx/conf.d/default.conf
COPY frontend/nginx-metrics.conf /etc/nginx/conf.d/metrics.conf
COPY frontend/crs-setup.conf /etc/nginx/modsecurity/crs-setup.conf
COPY frontend/modsecurity.conf /etc/nginx/modsecurity/modsecurity.conf
COPY frontend/nginx.conf /etc/nginx/nginx.conf

EXPOSE 80 8080
CMD ["nginx", "-g", "daemon off;"]
