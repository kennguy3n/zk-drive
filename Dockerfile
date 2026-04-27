# syntax=docker/dockerfile:1.7

# ---- Builder stage ----
FROM golang:1.22-alpine AS builder
WORKDIR /src

RUN apk add --no-cache git

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/worker ./cmd/worker

# ---- Runtime stage ----
# debian:bookworm-slim (instead of distroless static) so the worker
# can shell out to pdftoppm (poppler-utils) for PDF preview rendering.
# poppler-utils is GPL but used only as an external subprocess by the
# worker, so it does not affect the proprietary licence of the Go
# binaries copied in below.
FROM debian:bookworm-slim AS runtime
WORKDIR /app

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates \
        poppler-utils \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --system --gid 65532 nonroot \
    && useradd --system --uid 65532 --gid 65532 --home-dir /app nonroot

COPY --from=builder /out/server /app/server
COPY --from=builder /out/worker /app/worker
COPY --from=builder /src/migrations /app/migrations

USER nonroot:nonroot
EXPOSE 8080

ENTRYPOINT ["/app/server"]
