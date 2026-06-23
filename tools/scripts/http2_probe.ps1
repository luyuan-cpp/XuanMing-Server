# Pandora 客户端连接②(gRPC-Web)HTTP/2 盯死脚本
#
# 用途:把"现在到底是不是真 HTTP/2"从假设变成可证实、可持续监控。
#   连接② = UE FHttpModule grpc-web(login / team / match / push 等),经 Envoy :8443。
#   连接①(UE NetDriver / UDP,移动/技能)与本脚本无关。
#
# 两个独立证据:
#   A. ALPN 主动握手:用容器 curl(libcurl 带 nghttp2)对 Envoy :8443 发一次 h2 请求,
#      看 "server accepted h2"。证明服务端面具备 HTTP/2 能力。
#   B. Envoy stats 计数器:http.pandora_hcm.downstream_cx_http2_total / _http1_total。
#      这是"盯死"真客户端(UE)的指针——UE 连上来后 http2 计数涨、http1 保持 0 即达标。
#
# 用法:
#   pwsh tools/scripts/http2_probe.ps1            # 跑一次 A+B
#   pwsh tools/scripts/http2_probe.ps1 -Watch     # 每 2s 刷新 B(盯 UE 实连),Ctrl+C 退出
#   pwsh tools/scripts/http2_probe.ps1 -SkipAlpn  # 只看 stats,不发 curl 探测

[CmdletBinding()]
param(
    [string]$EnvoyHost = "127.0.0.1",
    [int]$ClientPort   = 8443,    # 客户端面 listener(pandora_hcm)
    [int]$AdminPort    = 9901,    # Envoy admin
    [switch]$Watch,
    [switch]$SkipAlpn,
    [int]$IntervalSec  = 2
)

$ErrorActionPreference = "Stop"

function Test-AlpnH2 {
    param([string]$VHost, [int]$Port)
    Write-Host "===== A. ALPN 握手探测(容器 curl --http2)=====" -ForegroundColor Cyan
    $docker = Get-Command docker -ErrorAction SilentlyContinue
    if (-not $docker) {
        Write-Host "  [SKIP] 未找到 docker,无法跑容器 curl(系统 Schannel curl 不支持 --http2)" -ForegroundColor Yellow
        return
    }
    $lines = docker run --rm --add-host=host.docker.internal:host-gateway curlimages/curl:latest `
        -k -sS -v --http2 "https://host.docker.internal:$Port/" -o /dev/null 2>&1
    $accepted = $lines | Select-String -Pattern "server accepted h2|using HTTP/2"
    $alpn     = $lines | Select-String -Pattern "ALPN"
    $alpn | ForEach-Object { Write-Host "  $_" }
    if ($accepted) {
        Write-Host "  [PASS] 服务端接受 h2,连接走 HTTP/2" -ForegroundColor Green
    } else {
        Write-Host "  [FAIL] 未协商到 h2(可能回落 HTTP/1.1)。原始输出:" -ForegroundColor Red
        $lines | Select-String -Pattern "HTTP/1|HTTP/2|refused|error" | ForEach-Object { Write-Host "  $_" }
    }
    Write-Host ""
}

function Show-Stats {
    param([string]$VHost, [int]$Port)
    $url = "http://${VHost}:$Port/stats"
    try {
        $raw = (Invoke-WebRequest -UseBasicParsing -Uri $url -TimeoutSec 5).Content
    } catch {
        Write-Host "  [ERR] 读不到 Envoy admin stats($url):$($_.Exception.Message)" -ForegroundColor Red
        return
    }
    function Get-Stat([string]$name) {
        $m = [regex]::Match($raw, [regex]::Escape($name) + ":\s*(\d+)")
        if ($m.Success) { [int]$m.Groups[1].Value } else { -1 }
    }
    # 客户端面 listener = pandora_hcm
    $h1cx = Get-Stat "http.pandora_hcm.downstream_cx_http1_total"
    $h2cx = Get-Stat "http.pandora_hcm.downstream_cx_http2_total"
    $h1rq = Get-Stat "http.pandora_hcm.downstream_rq_http1_total"
    $h2rq = Get-Stat "http.pandora_hcm.downstream_rq_http2_total"
    $h2act = Get-Stat "http.pandora_hcm.downstream_cx_http2_active"
    $h1act = Get-Stat "http.pandora_hcm.downstream_cx_http1_active"

    $stamp = Get-Date -Format "HH:mm:ss"
    Write-Host "[$stamp] 客户端面 pandora_hcm(连接②)" -ForegroundColor Cyan
    Write-Host ("  连接累计  h2={0,-6} h1={1,-6}" -f $h2cx, $h1cx) -ForegroundColor White
    Write-Host ("  当前活跃  h2={0,-6} h1={1,-6}" -f $h2act, $h1act) -ForegroundColor White
    Write-Host ("  请求累计  h2={0,-6} h1={1,-6}" -f $h2rq, $h1rq) -ForegroundColor White

    if ($h1cx -gt 0) {
        Write-Host "  [WARN] 出现 HTTP/1.1 连接!有客户端未协商到 h2(回落了)。" -ForegroundColor Yellow
    } elseif ($h2cx -gt 0) {
        Write-Host "  [PASS] 全部走 HTTP/2,无 h1 回落。" -ForegroundColor Green
    } else {
        Write-Host "  [IDLE] 还没有客户端连接②上来(UE 未启动或未登录)。" -ForegroundColor DarkGray
    }
    Write-Host ""
}

if (-not $SkipAlpn -and -not $Watch) {
    Test-AlpnH2 -VHost $EnvoyHost -Port $ClientPort
}

Write-Host "===== B. Envoy stats(盯 UE 真实连接)=====" -ForegroundColor Cyan
if ($Watch) {
    Write-Host "(每 ${IntervalSec}s 刷新,Ctrl+C 退出)" -ForegroundColor DarkGray
    while ($true) {
        Show-Stats -VHost $EnvoyHost -Port $AdminPort
        Start-Sleep -Seconds $IntervalSec
    }
} else {
    Show-Stats -VHost $EnvoyHost -Port $AdminPort
}
