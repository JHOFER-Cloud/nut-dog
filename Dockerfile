# Multi-arch (incl. linux/arm64 for the control-plane Pi). The Go binary is the
# controller; the image also carries NUT's server side (upsd + snmp-ups +
# dummy-ups), which the entrypoint configures from env and starts before the
# controller. nut-dog itself speaks NUT/SSH/WoL in Go, so no client tools needed.
FROM golang:1.25-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/nut-dog ./cmd/nut-dog

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
        nut-server nut-snmp gettext-base ca-certificates \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/nut-dog /usr/local/bin/nut-dog
COPY container/entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
