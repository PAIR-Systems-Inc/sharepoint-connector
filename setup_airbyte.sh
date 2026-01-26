#!/bin/bash
# Airbyte Open Source Setup Script
# Uses the official abctl installation method

set -e

echo "=========================================="
echo "Airbyte Open Source Setup Script"
echo "=========================================="
echo ""

# Check if Docker is installed
if ! command -v docker &> /dev/null; then
    echo "❌ Docker is not installed. Please install Docker first."
    echo "Visit: https://docs.docker.com/get-docker/"
    exit 1
fi

# Check if Docker is running
if ! docker info &> /dev/null; then
    echo "❌ Docker daemon is not running. Please start Docker first."
    exit 1
fi

echo "✓ Docker is installed and running"
echo ""

# Check if abctl is already installed
if command -v abctl &> /dev/null; then
    echo "✓ abctl is already installed"
    abctl version
    echo ""
else
    echo "Installing abctl..."
    curl -LsfS https://get.airbyte.com | bash -
    
    if ! command -v abctl &> /dev/null; then
        echo "❌ abctl installation failed. Please install manually:"
        echo "   curl -LsfS https://get.airbyte.com | bash -"
        exit 1
    fi
    
    echo "✓ abctl installed successfully"
    echo ""
fi

# Check if Airbyte is already installed
if abctl local status &> /dev/null; then
    echo "Airbyte appears to be already installed."
    read -p "Do you want to reinstall? (y/n) " -n 1 -r
    echo
    if [[ $REPLY =~ ^[Yy]$ ]]; then
        echo "Uninstalling existing Airbyte installation..."
        abctl local uninstall --persisted 2>/dev/null || true
    else
        echo "Starting existing Airbyte installation..."
        echo ""
        echo "To get credentials, run: abctl local credentials"
        echo "Access Airbyte at: http://localhost:8000"
        exit 0
    fi
fi

echo "Installing Airbyte..."
echo "This may take up to 30 minutes depending on your internet connection."
echo ""

# Install Airbyte
abctl local install

echo ""
echo "=========================================="
echo "Installation Complete!"
echo "=========================================="
echo ""
echo "To get your credentials, run:"
echo "  abctl local credentials"
echo ""
echo "Access Airbyte at: http://localhost:8000"
echo ""
