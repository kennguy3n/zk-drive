# syntax=docker/dockerfile:1.7

# ---- Builder stage ----
FROM golang:1.22-alpine AS builder
WORKDIR /src

RUN apk add --no-cache git

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server

# ---- Runtime stage ----
FROM gcr.io/distroless/static-debian12:nonroot AS runtime
WORKDIR /app

COPY --from=builder /out/server /app/server
COPY --from=builder /src/migrations /app/migrations

USER nonroot:nonroot
EXPOSE 8080

ENTRYPOINT ["/app/server"]
