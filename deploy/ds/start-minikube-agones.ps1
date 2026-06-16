# Start a local minikube cluster for Pandora Agones testing.
# Baselines:
#   - minikube binary: v1.38.1 (latest baseline)
#   - Kubernetes:      v1.35.1 (latest supported by Agones v1.58.0 baseline)
#
# This script never falls back to Kubernetes v1.34.x. If v1.35.1 cannot be
# downloaded from the current network/mirror, it fails loudly so the mirror or
# preloaded cache can be fixed.
#
# Usage:
#   powershell -ExecutionPolicy Bypass -File .\deploy\ds\start-minikube-agones.ps1
#   powershell -ExecutionPolicy Bypass -File .\deploy\ds\start-minikube-agones.ps1 -Profile pandora-agones

param(
    [string]$Profile = "pandora-agones",
    [string]$MinikubeVersion = "v1.38.1",
    [string]$KubernetesVersion = "v1.35.1",
    [int]$Cpus = 4,
    [int]$Memory = 6144,
    [string]$BaseImage = "registry.cn-hangzhou.aliyuncs.com/google_containers/kicbase:v0.0.50",
    [string]$BinaryMirror = "https://dl.k8s.io/release",
    [string]$ImageRepository = "",
    [switch]$UseAliyunK8sImageRepository
)

$ErrorActionPreference = "Stop"

$Minikube = Get-Command minikube -ErrorAction SilentlyContinue
if (-not $Minikube) {
    throw "minikube not found. Run deploy/ds/install-minikube-windows.ps1 first."
}

$Expected = $MinikubeVersion.TrimStart("v")
$VersionText = (& $Minikube.Source version) -join "`n"
if ($VersionText -notmatch "minikube version:\s*v?$([regex]::Escape($Expected))") {
    throw "minikube must be $MinikubeVersion. Current:`n$VersionText`nRun deploy/ds/install-minikube-windows.ps1 -Force first."
}

$Args = @(
    "start",
    "-p", $Profile,
    "--driver=docker",
    "--kubernetes-version=$KubernetesVersion",
    "--cpus=$Cpus",
    "--memory=$Memory",
    "--base-image=$BaseImage",
    "--binary-mirror=$BinaryMirror",
    "--preload=false",
    "--cache-images=false",
    "--interactive=false"
)

if ($UseAliyunK8sImageRepository) {
    $ImageRepository = "registry.cn-hangzhou.aliyuncs.com/google_containers"
}

if ($ImageRepository) {
    $Args += "--image-repository=$ImageRepository"
}

Write-Host "Starting minikube profile '$Profile'"
Write-Host "minikube:   $MinikubeVersion"
Write-Host "Kubernetes: $KubernetesVersion"
Write-Host "binary mirror: $BinaryMirror"
Write-Host "Note: this script does not downgrade to Kubernetes v1.34.x."

& $Minikube.Source @Args
if ($LASTEXITCODE -ne 0) {
    throw "minikube start failed. Keep Kubernetes at $KubernetesVersion; fix network/mirror/cache instead of downgrading."
}

kubectl config use-context $Profile
kubectl version
kubectl get nodes -o wide
