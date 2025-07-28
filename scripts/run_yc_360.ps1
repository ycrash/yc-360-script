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
    $scriptVersion = "2025.07.28.1950"
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
        $ProgressPreference = 'SilentlyContinue'
        Invoke-WebRequest -Uri $url -OutFile $zip_file -UseBasicParsing

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
        Write-Host "[INFO] Detected platform: $platform / architecture: $arch_folder"

        # Extract yc.exe (flat layout)
        Write-Host "[INFO] Extracting yc binary..."
        if (Get-Command jar -ErrorAction SilentlyContinue) {
            & jar -xf $zip_file "$platform/yc.exe"
        } elseif (Get-Command unzip -ErrorAction SilentlyContinue) {
            & unzip -q $zip_file "$platform/yc.exe"
        } else {
            throw "[ERROR] No extraction tool found (jar or unzip required)"
        }
        Write-Host "[INFO] yc.exe extracted to: $bin_dir\yc.exe"

        # Move binary
        $target = Join-Path $work_dir "$platform\yc.exe"
        if (-Not (Test-Path $target)) {
            throw "[ERROR] yc binary not found at $target"
        }
        Move-Item $target "$bin_dir\yc.exe" -Force
        icacls "$bin_dir\yc.exe" /grant Everyone:F | Out-Null

        # Clean up temp folders (except output)
        Remove-Item -Recurse -Force "$work_dir\linux","$work_dir\mac","$work_dir\windows","$zip_file" -ErrorAction SilentlyContinue

        # Set Java path if not provided
        if (-not $JavaHome -or $JavaHome -eq "") {
            $javaPath = Get-Command java -ErrorAction SilentlyContinue | Select-Object -ExpandProperty Source
            if ($javaPath) {
                $JavaHome = Split-Path (Split-Path $javaPath -Parent) -Parent
                Write-Host "[INFO] JavaHome not provided. Using detected JavaHome: $JavaHome"
            } else {
                Write-Error "[ERROR] Java not found in PATH and JavaHome was not provided."
                exit 1
            }
        } else {
            Write-Host "[INFO] Using provided JavaHome: $JavaHome"
        }

        # Set PIDs if not provided
        if (-not $TargetPids -or $TargetPids -eq "") {
            $javaProcesses = Get-Process java -ErrorAction SilentlyContinue
            if ($javaProcesses.Count -eq 0) {
                Write-Error "[ERROR] No running Java processes found, and TargetPids not provided."
                exit 1
            }
            $TargetPids = ($javaProcesses | Select-Object -ExpandProperty Id) -join ","
            Write-Host "[INFO] TargetPids not provided. Using detected Java PIDs: $TargetPids"
        } else {
            Write-Host "[INFO] Using provided TargetPids: $TargetPids"
        }

        # Print the detected PIDs
        $pidsArray = $TargetPids -split ','

        Write-Host "`n[INFO] Detected the following Java PIDs to capture:"
        $pidsArray | ForEach-Object { Write-Host " - $_" }

        # Execute yc for each PID
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

        # Navigate to output directory and list contents
        Set-Location $work_dir
        Write-Host "`n[INFO] Listing contents of output directory:"
        Get-ChildItem -Force | Format-Table -AutoSize

        # Open the output folder in File Explorer
        Start-Process explorer.exe $work_dir

        Write-Host "`n[INFO] Script executed successfully."

    } catch {
        Write-Host "`n[ERROR] yc-360 script encountered an error."
        Write-Host "------------------------------------------------------------"
        Write-Host "Need help? Visit our troubleshooting FAQ page:"
        Write-Host "https://test.docs.ycrash.io/yc-360/faq/run-yc-360-faq.html"
        Write-Host "------------------------------------------------------------"
        Write-Host "`n[ERROR DETAILS] $($_.Exception.Message)"
    }
}

# Explicit call to run when using iwr | iex
Start-yc360 @args