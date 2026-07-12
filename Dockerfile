# Multi-arch (incl. linux/arm64 for the control-plane Pi). The Go binary is the
# controller; the image also carries NUT's server side (upsd + snmp-ups +
# dummy-ups), which the entrypoint configures from env and starts before the
# controller. nut-dog itself speaks NUT/SSH/WoL in Go, so no client tools needed.

# NUT is built from source (see the nut-build stage): Debian ships 2.8.0, whose
# CyberPower MIB (0.51) has no ups.realpower mapping and blanks ups.status on the
# RMCARD (NULL cal OID). 2.8.5 (MIB 0.56) fixes both. Pinned + sha256-verified;
# Renovate watches the release tag (custom manager in renovate.json). The sha256
# must be updated by hand when the version bumps -- the build fails loudly if it
# is stale, which is the intended guard.
# switch to nix docker image at some point see JHC-532
# renovate: datasource=github-releases depName=networkupstools/nut
ARG NUT_VERSION=2.8.5
ARG NUT_SHA256=18bf32e59eb764b13da3c4fa70384926d7fa584cb31d2fe7f137a570633eeec1

FROM golang:1.26-bookworm AS build
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-X main.version=${VERSION}" -o /out/nut-dog ./cmd/nut-dog

# Compile NUT from the release tarball. Only the two drivers we use are built;
# upsd + upsdrvctl come by default. Installed into /staging for a clean copy.
FROM debian:bookworm-slim AS nut-build
ARG NUT_VERSION
ARG NUT_SHA256
RUN apt-get update && apt-get install -y --no-install-recommends \
    build-essential pkg-config libsnmp-dev libssl-dev wget ca-certificates \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /build
RUN wget -qO nut.tar.gz "https://github.com/networkupstools/nut/releases/download/v${NUT_VERSION}/nut-${NUT_VERSION}.tar.gz" \
    && echo "${NUT_SHA256}  nut.tar.gz" | sha256sum -c - \
    && tar xzf nut.tar.gz
WORKDIR /build/nut-${NUT_VERSION}
RUN ./configure \
    --prefix=/usr \
    --sysconfdir=/etc/nut \
    --with-statepath=/run/nut \
    --with-drivers=snmp-ups,dummy-ups \
    --with-drvpath=/usr/lib/nut \
    --with-snmp \
    --with-openssl \
    --with-user=root --with-group=root \
    --disable-dependency-tracking \
    && make -j"$(nproc)" \
    && make install DESTDIR=/staging

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
    libsnmp40 libssl3 gettext-base ca-certificates \
    && rm -rf /var/lib/apt/lists/*
COPY --from=nut-build /staging/usr/ /usr/
COPY --from=nut-build /staging/etc/ /etc/
RUN ldconfig
COPY --from=build /out/nut-dog /usr/local/bin/nut-dog
COPY container/entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
