#!/usr/bin/env bash
set -euo pipefail

# Create fixtures directory if it doesn't exist
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
FIXTURES_DIR="$SCRIPT_DIR/../fixtures"
mkdir -p "$FIXTURES_DIR"
FIXTURES_DIR="$(cd "$FIXTURES_DIR" && pwd)"

echo "[INFO] Setting up test fixtures..."

# Download BuggyApp distribution if not present
if [ ! -f "$FIXTURES_DIR/launch.sh" ]; then
    echo "[INFO] Downloading BuggyApp distribution..."
    if ! curl -sSLo "$FIXTURES_DIR/buggyapp.zip" \
        https://tier1app.com/dist/buggyapp/buggyapp-latest.zip; then
        echo "[ERROR] Failed to download BuggyApp from tier1app.com"
        exit 1
    fi

    echo "[INFO] Extracting BuggyApp distribution..."
    if ! unzip -q "$FIXTURES_DIR/buggyapp.zip" -d "$FIXTURES_DIR"; then
        echo "[ERROR] Failed to unzip BuggyApp"
        rm -f "$FIXTURES_DIR/buggyapp.zip"
        exit 1
    fi

    rm -f "$FIXTURES_DIR/buggyapp.zip"

    # Make launch script executable
    if [ -f "$FIXTURES_DIR/launch.sh" ]; then
        chmod +x "$FIXTURES_DIR/launch.sh"
    elif [ -f "$FIXTURES_DIR/launch.bat" ]; then
        echo "[INFO] Windows launch script found"
    else
        echo "[ERROR] Could not find launch script in downloaded archive"
        echo "[INFO] Contents of fixtures directory:"
        ls -la "$FIXTURES_DIR/"
        exit 1
    fi

    echo "[INFO] BuggyApp distribution downloaded successfully"
else
    echo "[INFO] BuggyApp already exists at $FIXTURES_DIR/"
fi

# Compile MyClass.java if it exists
MYCLASS_JAVA="$SCRIPT_DIR/../../internal/agent/testdata/MyClass.java"
if [ -f "$MYCLASS_JAVA" ] && [ ! -f "$FIXTURES_DIR/MyClass.class" ]; then
    echo "[INFO] Compiling MyClass.java..."
    javac -d "$FIXTURES_DIR" "$MYCLASS_JAVA"
fi

echo "[INFO] Test fixtures ready at: $FIXTURES_DIR"
