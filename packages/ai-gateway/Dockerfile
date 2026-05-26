FROM golang:1.25-alpine AS builder
WORKDIR /app

COPY go.work go.work.sum ./
COPY packages/shared/ packages/shared/
COPY packages/ai-gateway/ packages/ai-gateway/

WORKDIR /app/packages/ai-gateway
RUN go build -ldflags="-s -w" -o /ai-gateway ./cmd/ai-gateway/

FROM alpine:3.21
RUN apk add --no-cache ca-certificates && \
    adduser -D -u 65534 nonroot
USER nonroot
COPY --from=builder /ai-gateway /usr/local/bin/ai-gateway

EXPOSE 3050
ENTRYPOINT ["ai-gateway"]
