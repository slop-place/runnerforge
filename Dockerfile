# runnerforge builds to a single static binary with the web UI embedded, so the
# runtime image needs nothing but CA certificates.
FROM golang:1.27-alpine AS build

WORKDIR /src

# Dependencies first, so a source-only change does not re-download them.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
# CGO stays off: the SQLite driver is pure Go, which is what lets the runtime
# image be scratch rather than a distro.
RUN CGO_ENABLED=0 go build \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/runnerforge ./cmd/runnerforge

FROM alpine:3.21 AS certs
RUN apk add --no-cache ca-certificates

FROM scratch

COPY --from=certs /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /out/runnerforge /usr/local/bin/runnerforge

# Runs unprivileged. runnerforge never needs root: it talks to cloud and forge
# APIs over HTTPS and writes one SQLite file.
USER 65532:65532

# The database lives here; mount a volume over it for anything but a trial.
WORKDIR /data
VOLUME ["/data"]

EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/runnerforge"]
CMD ["serve", "-config", "/data/runnerforge.yaml"]
