FROM python:3.11-slim

ENV PYTHONUNBUFFERED=1 \
    PLAYWRIGHT_BROWSERS_PATH=/ms-playwright

# s6-overlay supervises `gateway run` in the gateway container (mirrors
# the official nousresearch/hermes-agent image where `gateway run` is
# supervised by s6 as an s6-rc service).
# Debian (not alpine): Playwright's bundled chromium needs glibc.
ARG S6_OVERLAY_VERSION=3.2.1.0
RUN apt-get update && apt-get install -y --no-install-recommends \
    bash git openssh-client curl sqlite3 ca-certificates xz-utils \
    nodejs npm \
  && rm -rf /var/lib/apt/lists
ADD https://github.com/just-containers/s6-overlay/releases/download/v${S6_OVERLAY_VERSION}/s6-overlay-noarch.tar.xz /tmp
ADD https://github.com/just-containers/s6-overlay/releases/download/v${S6_OVERLAY_VERSION}/s6-overlay-x86_64.tar.xz /tmp
RUN tar -C / -Jxpf /tmp/s6-overlay-noarch.tar.xz \
 && tar -C / -Jxpf /tmp/s6-overlay-x86_64.tar.xz \
 && rm /tmp/s6-overlay-*.tar.xz

RUN pip install --no-cache-dir hermes-agent pymongo mcp python-dotenv requests

# Playwright MCP (browser automation for the bots): the official
# @playwright/mcp server + headless shell only (--only-shell skips the
# full chromium + ffmpeg, which the headless MCP never uses). Browsers live
# in /ms-playwright so the s6-setuidgid 1000:1000 services can execute them.
RUN npm install -g @playwright/mcp \
 && npx -y playwright install --with-deps --only-shell chromium \
 && chmod -R a+rX /ms-playwright

COPY entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh

# s6 service definition for the multiplexed gateway.
# Use per-profile s6 naming (gateway-default) as Hermes service_manager expects
# (S6_SERVICE_PREFIX=gateway-); plain "gateway" is legacy and not listed.
RUN mkdir -p /etc/services.d/gateway-default
RUN printf '#!/command/with-contenv bash\nexec s6-setuidgid 1000:1000 hermes gateway run --force --accept-hooks --no-supervise\n' > /etc/services.d/gateway-default/run \
  && chmod +x /etc/services.d/gateway-default/run

# cont-init renders configs before s6 starts services (when launched via /init)
RUN mkdir -p /etc/cont-init.d
RUN printf '#!/command/with-contenv bash\n/usr/local/bin/entrypoint.sh render-only\n' > /etc/cont-init.d/01-render-config \
 && chmod +x /etc/cont-init.d/01-render-config

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
