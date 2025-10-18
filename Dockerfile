FROM alpine:3.18

# Add CA certificates for HTTPS
RUN apk --no-cache add ca-certificates bash

# Create non-root user
RUN adduser -D -u 10001 appuser

# Copy the binary from builder
COPY --from=builder pod-restarter /usr/local/bin/

# Use non-root user
USER 10001

# Set the entrypoint
ENTRYPOINT ["/usr/local/bin/pod-restarter"]
