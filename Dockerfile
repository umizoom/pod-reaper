# Stage 1: Build the Go application
FROM golang:1.22 AS builder

# Set the working directory inside the container
WORKDIR /app

# Copy the Go modules manifests
COPY go.mod go.sum ./

# Download dependencies
RUN go mod download

# Copy the source code into the container
COPY . .

# Build the application
RUN go build -o controller .

# Stage 2: Create a minimal runtime image
# FROM alpine:3.18

# Install required certificates
# RUN apk add --no-cache ca-certificates

# Set the working directory
# WORKDIR /app

# Copy the compiled binary from the build stage
# COPY --from=builder /app/controller .


# Run the application
CMD ["./controller"]
