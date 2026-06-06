# Build stage
# Base image tag: golang:1.26.3-alpine3.23
FROM golang:1.26.4-alpine3.23@sha256:f23e8b227fb4493eabe03bede4d5a32d04092da71962f1fb79b5f7d1e6c2a17f AS builder

WORKDIR /app

# Pure Go only: build fails if any dependency requires cgo.
ENV CGO_ENABLED=0

COPY backend/go.*  /app/
RUN go mod download

COPY backend/*.go backend/*.json /app/
COPY backend/static /app/static
COPY backend/data /app/data
COPY backend/routefinder /app/routefinder
COPY backend/zkillboard-cache /app/zkillboard-cache
COPY backend/doublebuffer /app/doublebuffer
COPY backend/logging /app/logging

ARG IMAGE_TAG
ENV IMAGE_TAG=$IMAGE_TAG
RUN test -n "$IMAGE_TAG" || (echo "IMAGE_TAG build-arg is required" && exit 1)

# Pin Go CLI tool versions so Docker builds don't auto-update to "too new" releases.
# You can override these build args in `docker compose build`.
ARG STATICCHECK_VERSION=v0.7.0
ARG GOVULNCHECK_VERSION=v1.1.4

RUN go run honnef.co/go/tools/cmd/staticcheck@${STATICCHECK_VERSION} ./... && \
    go run golang.org/x/vuln/cmd/govulncheck@${GOVULNCHECK_VERSION} ./... && \
    go build -ldflags="-s -w -X main.commit=$IMAGE_TAG" -o backend .

# Runtime stage (scratch: empty image, minimal size)
FROM scratch

COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /app/backend /app/backend
COPY --from=builder /app/data /app/data
COPY --from=builder /app/routefinder /app/routefinder
COPY --from=builder /app/zkillboard-cache /app/zkillboard-cache
COPY --from=builder /app/doublebuffer /app/doublebuffer

ENV SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt
WORKDIR /app

EXPOSE 8080

CMD ["/app/backend"]
