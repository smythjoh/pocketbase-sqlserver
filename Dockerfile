# ---- Build stage ----
FROM golang:1.23-alpine AS builder

WORKDIR /app

# Install build tools
RUN apk add --no-cache git

# Copy go mod and sum files
COPY go.mod go.sum ./
RUN go mod download

# Copy the entire codebase for building
COPY . .

# Build your custom PocketBase binary
RUN go build -o pocketbase ./examples/base/main.go

# ---- Run stage ----
FROM alpine:latest

WORKDIR /pb

RUN apk add --no-cache ca-certificates

# Copy the built binary from the builder stage
COPY --from=builder /app/pocketbase .

# Copy runtime assets (migrations, hooks, .env) if present
# COPY examples/base/pb_migrations ./pb_migrations
# COPY examples/base/pb_hooks ./pb_hooks
COPY examples/base/.env .env

EXPOSE 8080

CMD ["./pocketbase", "serve", "--http=0.0.0.0:8080"]