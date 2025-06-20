#!/usr/bin/env bash
set -euo pipefail

echo "[INFO] Starting buggyapp launcher..."

# Setup
work_dir="/tmp/buggyapp_$$"
zip_file="$work_dir/buggyapp.zip"
url="https://tier1app.com/dist/buggyapp/buggyapp-latest.zip"

mkdir -p "$work_dir"
cd "$work_dir"

# Download buggyapp zip
echo "[INFO] Downloading buggyapp..."
if ! curl -sSLo "$zip_file" "$url"; then
    echo "[ERROR] Failed to download buggyapp.zip"
    exit 1
fi

# Extract contents
echo "[INFO] Extracting buggyapp..."
if ! unzip -q "$zip_file" -d "$work_dir"; then
    echo "[ERROR] Failed to unzip buggyapp"
    exit 1
fi

# Make launch.sh executable
chmod +x "$work_dir/launch.sh"

# Launch buggyapp
echo "[INFO] Launching buggyapp..."
"$work_dir/launch.sh"

echo "[INFO] Buggyapp launched successfully. Working directory: $work_dir"