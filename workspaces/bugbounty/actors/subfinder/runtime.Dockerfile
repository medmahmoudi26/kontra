# Runtime layer for the subfinder actor (appended to the runtime stage by cli/deploy.go).
#
# The actor shells to subfinder, so the binary must be in the image. Pinned to an exact release
# rather than @latest: the enumeration result set is part of the scan's evidence, and a silently
# changing tool version would make two runs incomparable.
#
# ca-certificates is required — subfinder's passive sources are all HTTPS APIs, and without it
# every source fails with an x509 error and the actor returns zero subdomains while looking
# perfectly healthy.
ARG SUBFINDER_VERSION=2.12.0
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl unzip \
    && curl -sSfL -o /tmp/subfinder.zip \
       "https://github.com/projectdiscovery/subfinder/releases/download/v${SUBFINDER_VERSION}/subfinder_${SUBFINDER_VERSION}_linux_amd64.zip" \
    && unzip -o /tmp/subfinder.zip -d /usr/local/bin/ subfinder \
    && chmod +x /usr/local/bin/subfinder \
    && rm -f /tmp/subfinder.zip \
    && apt-get purge -y curl unzip && apt-get autoremove -y \
    && rm -rf /var/lib/apt/lists/* \
    && subfinder -version
