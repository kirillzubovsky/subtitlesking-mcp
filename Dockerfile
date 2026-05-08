# -------------------------
# Stage 1: Build the Go app
# -------------------------
FROM golang:1.20 AS builder
LABEL stage=builder

# Install ffmpeg for subtitle operations
RUN apt-get update && apt-get install -y ffmpeg

# Create and switch to a working directory
WORKDIR /app

# Copy go.mod and go.sum first, then download dependencies
COPY go.mod go.sum ./
RUN go mod download

# Copy the rest of the source code
COPY . .

# Build the Go application
RUN go build -o subtitlesking .

# -------------------------
# Stage 2: Final runtime
# -------------------------
FROM python:3.10-slim

# Install dependencies you need:
# - ffmpeg, sqlite3 for your DB
# - pip install for your Python requirements
RUN apt-get update && \
    apt-get install -y ffmpeg sqlite3 && \
    apt-get clean

# Copy the requirements.txt and install them
WORKDIR /app
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

# Copy the compiled Go binary from Stage 1
COPY --from=builder /app/subtitlesking /usr/local/bin/subtitlesking

# Expose any ports if your Go app listens on them
# EXPOSE 8080

# Run the Go binary as the entry point
ENTRYPOINT ["/usr/local/bin/subtitlesking"] 