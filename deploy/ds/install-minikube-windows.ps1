# Install or update minikube on Windows.
# Defaults to the current latest minikube baseline used by this repo: v1.38.1.
# Domestic mirror is preferred for China networks; GitHub is only a fallback.
#
# Usage:
#   powershell -ExecutionPolicy Bypass -File .\deploy\ds\install-minikube-windows.ps1
#   powershell -ExecutionPolicy Bypass -File .\deploy\ds\install-minikube-windows.ps1 -Force
#   powershell -ExecutionPolicy Bypass -File .\deploy\ds\install-minikube-windows.ps1 -TargetDir "C:\Tools\minikube"

param(
    [string]$Version = "v1.38.1",
    [string]$TargetDir = "$env:LOCALAPPDATA\Programs\minikube",
    [switch]$Force
)

$ErrorActionPreference = "Stop"

$ExpectedVersion = $Version.TrimStart("v")
$Existing = Get-Command minikube -ErrorAction SilentlyContinue
if ($Existing -and -not $Force) {
    $VersionText = (& $Existing.Source version) -join "`n"
    if ($VersionText -match "minikube version:\s*v?$([regex]::Escape($ExpectedVersion))") {
        Write-Host "minikube already at $Version ($($Existing.Source)); skip. Use -Force to reinstall." -ForegroundColor Green
        exit 0
    }
}

$MirrorBase = "https://registry.npmmirror.com/-/binary/minikube/$Version"
$GithubBase = "https://github.com/kubernetes/minikube/releases/download/$Version"
$FileName = "minikube-windows-amd64.exe"
$ShaName = "$FileName.sha256"

$TempDir = Join-Path $env:TEMP "pandora-minikube-$Version"
New-Item -ItemType Directory -Force -Path $TempDir | Out-Null

$ExePath = Join-Path $TempDir $FileName
$ShaPath = Join-Path $TempDir $ShaName

function Invoke-DownloadFirstAvailable {
    param(
        [string[]]$Urls,
        [string]$OutFile
    )

    $LastError = $null
    foreach ($Url in $Urls) {
        try {
            Write-Host "Downloading $Url"
            Invoke-WebRequest -Uri $Url -OutFile $OutFile -UseBasicParsing
            return
        }
        catch {
            $LastError = $_
            Write-Warning "Download failed: $Url"
        }
    }

    throw $LastError
}

Invoke-DownloadFirstAvailable `
    -Urls @("$MirrorBase/$FileName", "$GithubBase/$FileName") `
    -OutFile $ExePath

Invoke-DownloadFirstAvailable `
    -Urls @("$MirrorBase/$ShaName", "$GithubBase/$ShaName") `
    -OutFile $ShaPath

$ExpectedSha = (Get-Content -LiteralPath $ShaPath -Raw).Trim().Split(" ", "`t", "`r", "`n")[0].ToLowerInvariant()
$ActualSha = (Get-FileHash -LiteralPath $ExePath -Algorithm SHA256).Hash.ToLowerInvariant()

if ($ExpectedSha -ne $ActualSha) {
    throw "SHA256 mismatch for $FileName. expected=$ExpectedSha actual=$ActualSha"
}

New-Item -ItemType Directory -Force -Path $TargetDir | Out-Null
$TargetPath = Join-Path $TargetDir "minikube.exe"
Copy-Item -LiteralPath $ExePath -Destination $TargetPath -Force

Write-Host "Installed minikube $Version -> $TargetPath" -ForegroundColor Green
Write-Host "If this directory is not on PATH, add it: $TargetDir"
& $TargetPath version
