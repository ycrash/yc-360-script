# ---------------------------------------------------------------------------
# yCrash Enterprise Automation Master Script 
# ---------------------------------------------------------------------------

function Start-yc360 {
    param (
        [string]$JavaHome = "",
        [string]$TargetPids = "",
        [string[]]$ExtraArgs = @()
    )

    $ErrorActionPreference = "Stop"
    Set-StrictMode -Version Latest

    # Version Info
    $scriptVersion = "2025.06.20.2137"
    Write-Host "`n[INFO] Starting yc-360 installer... (Version: $scriptVersion)"

    try {
        # Setup workspace
        $pidSuffix = [System.Diagnostics.Process]::GetCurrentProcess().Id
        $work_dir = Join-Path $env:TEMP "yc_agent_$pidSuffix"
        $bin_dir = Join-Path $work_dir "yc_bin"
        $zip_file = Join-Path $work_dir "yc-360.zip"
        $url = "https://tier1app.com/dist/ycrash/yc-360-latest.zip"

        New-Item -ItemType Directory -Force -Path $bin_dir | Out-Null
        Set-Location $work_dir

        # Download yc-360
        Write-Host "[INFO] Downloading yc-360..."
        if (Get-Command curl.exe -ErrorAction SilentlyContinue) {
            & curl.exe -sSL -o $zip_file $url
        } else {
            Write-Host "[INFO] 'curl' not found, using Invoke-WebRequest..."
            $ProgressPreference = 'SilentlyContinue'
            Invoke-WebRequest -Uri $url -OutFile $zip_file -UseBasicParsing
        }

        # Detect architecture
        $arch = $env:PROCESSOR_ARCHITECTURE
        $platform = "windows"
        switch ($arch.ToLower()) {
            "amd64" { $arch_folder = "amd64" }
            "x86_64" { $arch_folder = "amd64" }
            "arm64" { $arch_folder = "arm64" }
            "aarch64" { $arch_folder = "arm64" }
            default  { throw "[ERROR] Unsupported architecture: $arch" }
        }

        # Extract yc.exe (flat layout)
        Write-Host "[INFO] Extracting yc binary..."
        if (Get-Command jar -ErrorAction SilentlyContinue) {
            & jar -xf $zip_file "$platform/yc.exe"
        } elseif (Get-Command unzip -ErrorAction SilentlyContinue) {
            & unzip -q $zip_file "$platform/yc.exe"
        } else {
            throw "[ERROR] No extraction tool found (jar or unzip required)"
        }

        # Move binary
        $target = Join-Path $work_dir "$platform\yc.exe"
        if (-Not (Test-Path $target)) {
            throw "[ERROR] yc binary not found at $target"
        }
        Move-Item $target "$bin_dir\yc.exe" -Force
        icacls "$bin_dir\yc.exe" /grant Everyone:F | Out-Null

        # Clean up temp folders (except output)
        Remove-Item -Recurse -Force "$work_dir\linux","$work_dir\mac","$work_dir\windows","$zip_file" -ErrorAction SilentlyContinue

        # Detect JAVA_HOME
        if (-Not $JavaHome) {
            $javaCmd = Get-Command java -ErrorAction SilentlyContinue
            if (-not $javaCmd) {
                Write-Warning "[WARN] Java not found in PATH. Skipping execution."
                return
            }
            $javaPath = $javaCmd.Source
            $JavaHome = Split-Path -Parent (Split-Path -Parent $javaPath)
            Write-Host "[INFO] Detected JAVA_HOME: $JavaHome"
        }

        # Detect PIDs
        if (-not $TargetPids) {
            if (Get-Command jps -ErrorAction SilentlyContinue) {
                $TargetPids = (& jps | Where-Object { $_ -notmatch 'Jps' } | ForEach-Object {
                    ($_ -split '\s+')[0]
                }) -join ","
            }

            if (-not $TargetPids) {
                $javaProcesses = Get-Process | Where-Object { $_.Name -like "java*" }
                $TargetPids = $javaProcesses.Id -join ","
            }

            if (-not $TargetPids) {
                Write-Warning "`n[WARN] No running Java process found. Nothing to capture."
                return
            }
        }

        # Execute yc for each PID
        $pidsArray = $TargetPids -split ','

        foreach ($targetPid in $pidsArray) {
            if (-not $ExtraArgs -or $ExtraArgs.Count -eq 0) {
                Write-Host "[INFO] Running yc with default: -onlyCapture for PID: $targetPid"
                & "$bin_dir\yc.exe" -j "$JavaHome" -onlyCapture -p "$targetPid"
            } else {
                Write-Host "[INFO] Running yc with extra args: $ExtraArgs for PID: $targetPid"
                & "$bin_dir\yc.exe" -j "$JavaHome" @ExtraArgs -p "$targetPid"
            }
        }

        # Final cleanup
        Remove-Item -Recurse -Force "$bin_dir" -ErrorAction SilentlyContinue
        Write-Host "`n[INFO] yc-360 run completed. Output saved in: $work_dir"

    } catch {
        Write-Warning "`n[WARN] Script encountered an error: $_"
    }
}

# Explicit call to run when using iwr | iex
Start-yc360 @args