FROM alpine:3

# Create non-root user
RUN adduser -D -u 10001 appuser

# Copy the binary built by the release workflow
COPY pod-restarter /usr/local/bin/

# Use non-root user
USER 10001

# Set the entrypoint
ENTRYPOINT ["/usr/local/bin/pod-restarter"]
