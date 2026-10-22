# Multi-stage: Go build, then minimal runtime with CA certs (Atlas needs TLS).
# Context is the repo root.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/health-api/ ./cmd/health-api/
COPY internal/ ./internal/
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/health-api ./cmd/health-api

FROM alpine:3.21
RUN apk add --no-cache ca-certificates curl
COPY --from=build /out/health-api /usr/local/bin/health-api

EXPOSE 8000

CMD ["/usr/local/bin/health-api"]
