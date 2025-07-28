#!/usr/bin/env bash

# ---------------------------------------------------------------------------
# yCrash Enterprise Automation Master Script 
# ---------------------------------------------------------------------------

set -euo pipefail
IFS=$'\n\t'
trap 'echo "[ERROR] Script interrupted"; exit 1' SIGINT SIGTERM
trap 'echo "[INFO] Script exited successfully."' EXIT

SCRIPT_VERSION="2025.07.28.1141"
echo "[INFO] Starting yc-360 installer (version $SCRIPT_VERSION)"

# Setup workspace
work_dir="/tmp/yc_360_$$"
bin_dir="$work_dir/yc_bin"
zip_file="$work_dir/yc-360.zip"
url="https://tier1app.com/dist/ycrash/yc-360-latest.zip"

mkdir -p "$bin_dir"
cd "$work_dir"

# Download yc-360 binary
echo "[INFO] Downloading yc-360..."
if ! curl -sSLo "$zip_file" "$url"; then
    echo "[ERROR] Failed to download binary file"
    exit 1
fi

# Detect OS
os=$(uname -s)
arch=$(uname -m)
echo "[INFO] Detected OS: $os, Architecture: $arch"

# Normalize OS
if [[ "$os" == "Darwin" ]]; then
    platform="mac"
elif [[ "$os" == "Linux" ]]; then
    platform="linux"
else
    echo "[ERROR] Unsupported OS: $os"
    exit 1
fi

# Normalize Architecture
if [[ "$arch" == "x86_64" ]]; then
    arch_folder="amd64"
elif [[ "$arch" == "aarch64" || "$arch" == "arm64" ]]; then
    arch_folder="arm64"
else
    echo "[ERROR] Unsupported architecture: $arch"
    exit 1
fi

# Extract correct binary
echo "[INFO] Extracting yc-360 binary for $platform/$arch_folder..."
if ! jar -xf "$zip_file" "$platform/$arch_folder/yc"; then
    echo "[ERROR] Failed extraction"
    exit 1
fi

# Validate
target="$work_dir/$platform/$arch_folder/yc"
if [[ ! -f "$target" ]]; then
    echo "[ERROR] yc binary not found at $target"
    exit 1
fi

# Move binary
mv "$target" "$bin_dir/yc" && chmod +x "$bin_dir/yc"

# Cleanup partial folders
rm -rf "$work_dir/linux" "$work_dir/mac" "$zip_file"

# --- Argument parsing (optional override support) ---
user_java_home=""
user_pids=""
extra_args=()

while [[ $# -gt 0 ]]; do
    case "$1" in
        -j)
            user_java_home="$2"
            shift 2
            ;;
        -p)
            user_pids="$2"
            shift 2
            ;;
        *)
            extra_args+=("$1")
            shift
            ;;
    esac
done

# Auto-detect JAVA_HOME if not supplied
if [[ -z "$user_java_home" ]]; then
    java_bin=$(command -v java || true)
    if [[ -z "$java_bin" || ! -x "$java_bin" ]]; then
        echo "[ERROR] Java not found in PATH"
        exit 1
    fi
    user_java_home=$(realpath "$java_bin" | sed 's#/bin/java##')
fi

echo "[INFO] Using JAVA_HOME: $user_java_home"

# Auto-detect PIDs if not supplied
if [[ -z "$user_pids" ]]; then
    if command -v jps >/dev/null 2>&1; then
        user_pids=$(jps | awk '$2 != "Jps" {print $1}' | xargs || true)
    else
        # Use ps to avoid catching grep or this script
        user_pids=$(ps -eo pid,comm,args | awk '/[j]ava/ && $2 != "grep" {print $1}' | xargs || true)
    fi
fi

if [[ -z "$user_pids" ]]; then
    echo "[ERROR] No running Java process found"
    exit 1
fi

# Print all detected PIDs
echo "[INFO] Found Java PIDs:"
for pid in $user_pids; do
    echo " - $pid"
done

# Convert space-separated PIDs string to array
IFS=' ' read -r -a user_pids_array <<< "$user_pids"

# Run yc for each Java process
for pid in "${user_pids_array[@]}"; do
    if [[ ${#extra_args[@]} -eq 0 ]]; then
        echo "[INFO] Running yc with default options: -onlyCapture for PID: $pid"
        "$bin_dir/yc" -j "$user_java_home" -onlyCapture -p "$pid"
    else
        echo "[INFO] Running yc with extra options: ${extra_args[*]} for PID: $pid"
        "$bin_dir/yc" -j "$user_java_home" "${extra_args[@]}" -p "$pid"
    fi
done

# Cleanup after run
rm -rf "$bin_dir" "$work_dir/$platform"
echo "[INFO] yc-360 run completed. Output saved in: $work_dir"

# Move into the work directory and list files
cd "$work_dir"
echo "[INFO] Listing contents of output directory:"
ls -l

# Trap for any unhandled errors to show FAQ link
trap 'echo "
[ERROR] yc-360 script encountered an error.
------------------------------------------------------------
Need help? Visit our troubleshooting FAQ page:
https://test.docs.ycrash.io/yc-360/faq/run-yc-360-faq.html
------------------------------------------------------------" >&2' ERR