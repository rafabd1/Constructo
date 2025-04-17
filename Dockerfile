# Use a standard Go image
FROM golang:1.22-alpine AS builder

# Set the working directory inside the container
WORKDIR /app

# Copy go.mod and go.sum first to leverage Docker cache
COPY go.mod go.sum ./

# Download dependencies
RUN go mod download

# Copy the rest of the application code
COPY . .

# Build the application
# Using CGO_ENABLED=0 to build a static binary (often helpful for containers)
# Adjust target architecture if needed (e.g., for ARM)
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /constructo cmd/constructo/main.go

# --- Final Stage ---
# Use a minimal base image like alpine
FROM alpine:latest

# Install necessary runtime dependencies (if any - bash is often useful)
# For creack/pty, usually no extra OS libs are needed beyond standard libc
RUN apk add --no-cache bash

# Copy the built binary from the builder stage
COPY --from=builder /constructo /usr/local/bin/constructo

# Copy necessary runtime files
COPY configs/ /app/configs/
COPY instructions/ /app/instructions/

# Set the working directory for the running container
WORKDIR /app

# Define the entrypoint
ENTRYPOINT ["constructo"]

# Optionally define a default command if entrypoint is just the binary
# CMD ["--help"] # Example 