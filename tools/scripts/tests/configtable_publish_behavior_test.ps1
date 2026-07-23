# configtable_publish_behavior_test.ps1 — 发布脚本行为回归(R4 复审 P1-6/P1-8;R5 复审 P1-9)。
#
# 覆盖:
#  ① 正常首发成功,active 根目录有 manifest;
#  ② 残缺 active(目录在、manifest 缺失)必须非 0 退出,且不得生成 active\staging 嵌套
#     (P1-6:旧实现把该形态当"active 缺失"续跑,Move-Item 进已存在目录后退出 0 误报成功);
#  ③ 同版本、表文件字节相同、manifest 语义(rows/proto)不同 → 必须拒绝(P1-8:旧实现
#     只比表文件 hash,语义漂移静默放行);
#  ④ 同版本同批次幂等 no-op 仍成功;
#  ⑤ R5 P1-9:version=1.5(非整数)必须拒绝(旧实现 [uint64] 强转当 2 放行,Go 运行时拒载);
#  ⑥ R5 P1-9:大写 checksum 必须拒绝(旧实现 -ne 大小写不敏感放行,Go 运行时精确比较拒载)。
# P1-7(回滚精确恢复服务端 activeVersion)依赖 grpcurl 交互,不在本测试内,列入人工验收;
# P1-10(与 generator 的快照边界)与 P2-9(回滚槽位复验)为时序/交互路径,同列人工验收。
#
# 约定:非 0 退出 = 测试失败;成功输出 PASS 行。
$ErrorActionPreference = "Stop"

$script = Join-Path $PSScriptRoot "..\configtable_publish.ps1"
$work = Join-Path ([System.IO.Path]::GetTempPath()) ("ctpub-test-" + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Force $work | Out-Null

function Invoke-Publish([string]$root, [string]$dist) {
    & pwsh -NoProfile -File $script -DeployRoot $root -DistDir $dist *>&1 | Out-Null
    return $LASTEXITCODE
}

try {
    # 构造最小合法 dist:1 张表 + manifest(真实 sha256)。
    $dist = Join-Path $work "dist"
    New-Item -ItemType Directory -Force $dist | Out-Null
    Set-Content -NoNewline -Path (Join-Path $dist "level.json") -Value ('{"rows":[{"id":1}]}' + "`n") -Encoding UTF8
    $hash = "sha256:" + (Get-FileHash (Join-Path $dist "level.json") -Algorithm SHA256).Hash.ToLower()
    @{
        version = 101; generated_at_ms = 1; generator = "test"; source_rev = "svn-r1"
        tables  = @(@{ name = "level"; file = "level.json"; proto = "pandora.config.v1.LevelTableData"; checksum = $hash; rows = 1 })
    } | ConvertTo-Json -Depth 5 | Set-Content -Path (Join-Path $dist "manifest.json") -Encoding UTF8

    # ① 正常首发。
    $root1 = Join-Path $work "deploy1"
    if ((Invoke-Publish $root1 $dist) -ne 0) { Write-Host "[FAIL] 正常首发应成功" -ForegroundColor Red; exit 1 }
    if (-not (Test-Path (Join-Path $root1 "configtable\active\manifest.json"))) {
        Write-Host "[FAIL] 首发后 active 根目录缺 manifest" -ForegroundColor Red; exit 1
    }

    # ② 残缺 active(P1-6):删 manifest 再发布 → 必须拒绝,且无嵌套目录。
    Remove-Item (Join-Path $root1 "configtable\active\manifest.json")
    if ((Invoke-Publish $root1 $dist) -eq 0) {
        Write-Host "[FAIL] 残缺 active 必须拒绝发布(P1-6)" -ForegroundColor Red; exit 1
    }
    if (Test-Path (Join-Path $root1 "configtable\active\staging")) {
        Write-Host "[FAIL] 残缺 active 续跑生成了 active\staging 嵌套目录(P1-6)" -ForegroundColor Red; exit 1
    }

    # ③ 同版本语义漂移(P1-8):新根首发后篡改 active manifest 的 rows(表文件字节不动)。
    $root2 = Join-Path $work "deploy2"
    if ((Invoke-Publish $root2 $dist) -ne 0) { Write-Host "[FAIL] deploy2 首发应成功" -ForegroundColor Red; exit 1 }
    $activeManifestPath = Join-Path $root2 "configtable\active\manifest.json"
    $am = Get-Content $activeManifestPath -Raw | ConvertFrom-Json
    $am.tables[0].rows = 42
    $am | ConvertTo-Json -Depth 5 | Set-Content -Path $activeManifestPath -Encoding UTF8
    if ((Invoke-Publish $root2 $dist) -eq 0) {
        Write-Host "[FAIL] 同版本同字节但 rows 语义不同必须拒绝(P1-8)" -ForegroundColor Red; exit 1
    }

    # ④ 恢复语义一致后,同版本同批次幂等 no-op 仍成功。
    $am.tables[0].rows = 1
    $am | ConvertTo-Json -Depth 5 | Set-Content -Path $activeManifestPath -Encoding UTF8
    if ((Invoke-Publish $root2 $dist) -ne 0) {
        Write-Host "[FAIL] 同版本同批次幂等重发应成功" -ForegroundColor Red; exit 1
    }

    # ⑤ R5 P1-9:非整数 version 必须拒绝(运行时 json.Unmarshal 到 uint64 拒载 1.5)。
    $dist3 = Join-Path $work "dist3"
    New-Item -ItemType Directory -Force $dist3 | Out-Null
    Copy-Item (Join-Path $dist "level.json") (Join-Path $dist3 "level.json")
    (Get-Content (Join-Path $dist "manifest.json") -Raw).Replace('"version": 101', '"version": 1.5') |
        Set-Content -Path (Join-Path $dist3 "manifest.json") -Encoding UTF8
    $root3 = Join-Path $work "deploy3"
    if ((Invoke-Publish $root3 $dist3) -eq 0) {
        Write-Host "[FAIL] version=1.5 必须拒绝(P1-9:运行时拒载的形态发布器不得放行)" -ForegroundColor Red; exit 1
    }

    # ⑥ R5 P1-9:大写 checksum 必须拒绝(运行时精确比较,发布器不得靠 -ne 大小写不敏感放行)。
    $dist4 = Join-Path $work "dist4"
    New-Item -ItemType Directory -Force $dist4 | Out-Null
    Copy-Item (Join-Path $dist "level.json") (Join-Path $dist4 "level.json")
    $upperHash = "sha256:" + (Get-FileHash (Join-Path $dist4 "level.json") -Algorithm SHA256).Hash.ToUpper()
    @{
        version = 101; generated_at_ms = 1; generator = "test"; source_rev = "svn-r1"
        tables  = @(@{ name = "level"; file = "level.json"; proto = "pandora.config.v1.LevelTableData"; checksum = $upperHash; rows = 1 })
    } | ConvertTo-Json -Depth 5 | Set-Content -Path (Join-Path $dist4 "manifest.json") -Encoding UTF8
    $root4 = Join-Path $work "deploy4"
    if ((Invoke-Publish $root4 $dist4) -eq 0) {
        Write-Host "[FAIL] 大写 checksum 必须拒绝(P1-9:Go 运行时精确比较会拒载)" -ForegroundColor Red; exit 1
    }

    Write-Host "PASS configtable_publish_behavior_test(P1-6 残缺 active 拒绝/无嵌套;P1-8 语义漂移拒绝;幂等 no-op 保持;P1-9 非整数版本/大写 checksum 拒绝)" -ForegroundColor Green
    exit 0
} finally {
    Remove-Item -Recurse -Force $work -ErrorAction SilentlyContinue
}
