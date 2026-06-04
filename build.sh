#!/usr/bin/env bash
set -e

echo "Building ThalesOps Agent for Linux (amd64 and arm64)..."

# Use the short git SHA as the version, falling back to "dev" if not in a git repo
VERSION=$(git rev-parse --short HEAD 2>/dev/null || echo "dev")
echo "Version: $VERSION"
LDFLAGS="-X main.Version=${VERSION}"

# Create a releases directory
mkdir -p build/releases

# Build for x86_64 (amd64)
echo "Building for Linux amd64..."
GOOS=linux GOARCH=amd64 go build -ldflags "$LDFLAGS" -o build/releases/thalesops-agent-linux-amd64 main.go

# Build for ARM64
echo "Building for Linux arm64..."
GOOS=linux GOARCH=arm64 go build -ldflags "$LDFLAGS" -o build/releases/thalesops-agent-linux-arm64 main.go

# Write version file so the server can expose it at /releases/version.txt
echo "$VERSION" > build/releases/version.txt

# Copy the install script
cp install.sh build/install.sh

echo "Build complete! Upload the contents of the 'build/' folder to your staging-agent.com server."
