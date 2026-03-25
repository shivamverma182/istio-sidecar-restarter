# Build stage
FROM golang:1.26-alpine AS builder

# Set working directory
WORKDIR /app

COPY cloudflare.crt /tmp

RUN cat /tmp/cloudflare.crt >> /etc/ssl/certs/ca-certificates.crt

# Add CA certificates for HTTPS
RUN apk --no-cache add ca-certificates bash && update-ca-certificates

RUN cp /tmp/cloudflare.crt /usr/local/share/ca-certificates/cloudflare.crt && update-ca-certificates


# Copy go mod files
COPY go.mod ./

# Download dependencies
RUN go mod download

# Copy source code
COPY . .

# Build the application with optimizations
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o pod-restarter

# Final stage
FROM alpine:3.20

COPY cloudflare.crt /tmp

# Copy certificate bundle
RUN cat /tmp/cloudflare.crt >> /etc/ssl/certs/ca-certificates.crt

# Add CA certificates for HTTPS
RUN apk --no-cache add ca-certificates bash && update-ca-certificates

RUN cp /tmp/cloudflare.crt /usr/local/share/ca-certificates/cloudflare.crt && update-ca-certificates

# Create non-root user
RUN adduser -D -u 10001 appuser

# Copy the binary from builder
COPY --from=builder /app/pod-restarter /usr/local/bin/

# Use non-root user
USER 10001

# Set the entrypoint
ENTRYPOINT ["/usr/local/bin/pod-restarter"]
