FROM golang:1.25-alpine AS builder
WORKDIR /app

COPY go.work go.work.sum ./
COPY packages/shared/ packages/shared/
COPY packages/nexus-hub/ packages/nexus-hub/

WORKDIR /app/packages/nexus-hub
ARG BUILD_VERSION=dev
RUN go build -ldflags="-s -w -X main.buildVersion=${BUILD_VERSION}" \
    -o /nexus-hub ./cmd/nexus-hub/

FROM alpine:3.21
RUN apk add --no-cache ca-certificates
COPY --from=builder /nexus-hub /usr/local/bin/nexus-hub
COPY packages/nexus-hub/nexus-hub.config.yaml /etc/nexus-hub/config.yaml

EXPOSE 3060
ENTRYPOINT ["nexus-hub"]
CMD ["--config", "/etc/nexus-hub/config.yaml"]
