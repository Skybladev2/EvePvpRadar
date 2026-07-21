# Build stage
# Base image tag: golang:1.26.5-alpine3.23
FROM golang:1.26.5-alpine3.23@sha256:622e56dbc11a8cfe87cafa2331e9a201877271cbff918af53d3be315f3da88cc AS builder

WORKDIR /app

# Pure Go only: build fails if any dependency requires cgo.
ENV CGO_ENABLED=0
ENV GOPROXY=https://goproxy.cn,https://proxy.golang.org,https://goproxy.io,direct

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

# Minify JS (//go:embed picks up the minified file; source stays prettyprinted in repo)
ARG ESBUILD_VERSION=v0.25.0
RUN go run github.com/evanw/esbuild/cmd/esbuild@${ESBUILD_VERSION} \
    --minify --target=es2020 --allow-overwrite \
    /app/static/app.js --outfile=/app/static/app.js

# Pin Go CLI tool versions so Docker builds don't auto-update to "too new" releases.
# You can override these build args in `docker compose build`.
ARG STATICCHECK_VERSION=v0.7.0
ARG OSV_SCANNER_VERSION=v1

RUN go run honnef.co/go/tools/cmd/staticcheck@${STATICCHECK_VERSION} ./... && \
    go run github.com/google/osv-scanner/cmd/osv-scanner@${OSV_SCANNER_VERSION} --lockfile go.mod && \
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
