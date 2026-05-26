FROM golang:1.25-alpine AS builder
WORKDIR /app

COPY go.work go.work.sum ./
COPY packages/shared/ packages/shared/
COPY packages/control-plane/ packages/control-plane/

WORKDIR /app/packages/control-plane
RUN go build -ldflags="-s -w" -o /control-plane ./cmd/control-plane/

FROM alpine:3.21
RUN apk add --no-cache ca-certificates && \
    adduser -D -u 65534 nonroot
USER nonroot
COPY --from=builder /control-plane /usr/local/bin/control-plane

EXPOSE 3001
ENTRYPOINT ["control-plane"]
