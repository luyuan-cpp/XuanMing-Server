# Pandora 开发环境基础设施一键启动
#
# 用法:
#   pwsh tools/scripts/dev_up.ps1
#   pwsh tools/scripts/dev_up.ps1 -Pull   # 拉最新镜像后启动
#
# 启动 docker-compose 全套(MySQL/Redis/Kafka/etcd/Prometheus/Grafana),等所有服务 healthy。

param(
    [switch]$Pull
)

$ErrorActionPreference = "Stop"

$ProjectRoot = Resolve-Path "$PSScriptRoot/../.."
$ComposeFile = "$ProjectRoot/deploy/docker-compose.dev.yml"
$EnvFile     = "$ProjectRoot/deploy/env/dev.env"

Write-Host "===== Pandora dev infra up =====" -ForegroundColor Cyan
Write-Host "Project:      $ProjectRoot"
Write-Host "Compose file: $ComposeFile"
Write-Host "Env file:     $EnvFile"
Write-Host ""

if (-not (Test-Path $ComposeFile)) {
    Write-Host "[ERR] compose file not found: $ComposeFile" -ForegroundColor Red
    exit 1
}
if (-not (Test-Path $EnvFile)) {
    Write-Host "[ERR] env file not found: $EnvFile" -ForegroundColor Red
    exit 1
}

# 先 validate
Write-Host "[1/4] Validating compose file..." -ForegroundColor Yellow
docker compose -f $ComposeFile --env-file $EnvFile config --quiet
if ($LASTEXITCODE -ne 0) {
    Write-Host "[ERR] compose file invalid" -ForegroundColor Red
    exit 1
}

if ($Pull) {
    Write-Host "[2/4] Pulling latest images..." -ForegroundColor Yellow
    docker compose -f $ComposeFile --env-file $EnvFile pull
    if ($LASTEXITCODE -ne 0) {
        Write-Host "[ERR] docker pull failed" -ForegroundColor Red
        exit 1
    }
} else {
    Write-Host "[2/4] Skipping pull (use -Pull to refresh)" -ForegroundColor Yellow
}

Write-Host "[3/4] Starting containers..." -ForegroundColor Yellow
docker compose -f $ComposeFile --env-file $EnvFile up -d
if ($LASTEXITCODE -ne 0) {
    Write-Host "[ERR] compose up failed" -ForegroundColor Red
    exit 1
}

Write-Host "[4/4] Waiting for healthy..." -ForegroundColor Yellow
$timeout = 120  # 秒
$elapsed = 0
$step = 5
function Get-ComposePsJson {
    $lines = @(docker compose -f $ComposeFile --env-file $EnvFile ps --format json)
    if ($LASTEXITCODE -ne 0) {
        Write-Host "[ERR] docker compose ps failed" -ForegroundColor Red
        exit 1
    }

    if ($lines.Count -eq 0) { return @() }

    $text = ($lines -join "`n").Trim()
    if ($text.StartsWith("[")) {
        return @($text | ConvertFrom-Json)
    }

    $items = @()
    foreach ($line in $lines) {
        if ($line -and $line.Trim() -ne "") {
            $items += ConvertFrom-Json -InputObject $line
        }
    }
    return $items
}

while ($elapsed -lt $timeout) {
    $unhealthy = Get-ComposePsJson |
        Where-Object {
            ($_.Health -and $_.Health -ne "healthy") -or
            (-not $_.Health -and $_.State -ne "running")
        } |
        Select-Object -ExpandProperty Name
    if ($null -eq $unhealthy -or $unhealthy.Count -eq 0) { break }
    Start-Sleep -Seconds $step
    $elapsed += $step
    Write-Host "  ${elapsed}s waiting: $($unhealthy -join ', ')"
}

if ($elapsed -ge $timeout) {
    Write-Host "[ERR] containers not ready after ${timeout}s" -ForegroundColor Red
    docker compose -f $ComposeFile --env-file $EnvFile ps
    exit 1
}

Write-Host ""
Write-Host "===== 服务连接信息 =====" -ForegroundColor Green
Write-Host "MySQL       localhost:3307   user=pandora pass=pandora_dev_pwd"
Write-Host "Redis       localhost:6380"
Write-Host "Kafka       localhost:9093   (host网络可达)"
Write-Host "etcd        localhost:2380"
Write-Host "Prometheus  http://localhost:9091"
Write-Host "Grafana     http://localhost:3001  user=admin pass=pandora_dev_admin"
Write-Host ""
Write-Host "===== 状态 =====" -ForegroundColor Green
docker compose -f $ComposeFile --env-file $EnvFile ps
