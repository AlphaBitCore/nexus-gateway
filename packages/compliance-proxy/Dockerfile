FROM golang:1.25-alpine AS builder

WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o nexus-compliance-proxy ./cmd/compliance-proxy

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /build/nexus-compliance-proxy /usr/local/bin/nexus-compliance-proxy

EXPOSE 3128 9090

USER nonroot:nonroot

ENTRYPOINT ["/usr/local/bin/nexus-compliance-proxy"]
CMD ["-config", "/etc/compliance-proxy/compliance-proxy.config.yaml"]
