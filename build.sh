#!/usr/bin/env bash
set -e

echo "Building ThalesOps Agent for Linux (amd64 and arm64)..."

# Create a releases directory
mkdir -p build/releases

# Build for x86_64 (amd64)
echo "Building for Linux amd64..."
GOOS=linux GOARCH=amd64 go build -o build/releases/thalesops-agent-linux-amd64 main.go

# Build for ARM64
echo "Building for Linux arm64..."
GOOS=linux GOARCH=arm64 go build -o build/releases/thalesops-agent-linux-arm64 main.go

# Copy the install script
cp install.sh build/install.sh

echo "Build complete! Upload the contents of the 'build/' folder to your staging-agent.com server."
