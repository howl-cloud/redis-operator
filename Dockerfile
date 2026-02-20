# syntax=docker/dockerfile:1

# --- Build stage ---
FROM golang:1.25.1 AS builder

WORKDIR /workspace

# Cache module downloads before copying source.
COPY go.mod go.sum ./
RUN go mod download

# Copy source and build the single binary that serves both subcommands.
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" -o /out/manager ./cmd/manager

# --- Runtime stage ---
# gcr.io/distroless/static-debian12 ships no shell, no package manager —
# only the CA bundle and timezone data.  Perfect for a static Go binary.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /out/manager /manager

# The binary is the single entry-point; the subcommand (controller / instance)
# is chosen at container start via args, e.g.:
#   docker run redis-operator controller
#   docker run redis-operator instance
ENTRYPOINT ["/manager"]
