# Pandora 客户端 SVN 服务端 pre-commit 钩子主体(Windows / VisualSVN)。
# 由 svn-pre-commit.bat 调用;规则与 svn-pre-commit.sh 保持一致,改一处必须同步另一处。
param(
    [Parameter(Mandatory)][string]$Repos,
    [Parameter(Mandatory)][string]$Txn
)
$ErrorActionPreference = 'Stop'
$svnlook = if ($env:SVNLOOK) { $env:SVNLOOK } else { 'svnlook.exe' }

# 日志带 [hook-override] 则放行(仅限管理员救急)
$log = & $svnlook log -t $Txn $Repos 2>$null
if ($log -match '\[hook-override\]') { exit 0 }

$changed = & $svnlook changed -t $Txn $Repos
if ($LASTEXITCODE -ne 0) { [Console]::Error.WriteLine('pre-commit 钩子无法读取事务变更。'); exit 1 }

# 本仓库有意纳管 Pandora/Binaries(美术/策划靠 svn 同步编辑器 DLL),Binaries 不拉黑。
$pattern = '(^Packages/|^Packages$|(^|/)(Saved|Intermediate|DerivedDataCache)(/|$)|\.(tar|pak|ucas|utoc)$)'
$bad = @()
foreach ($line in $changed) {
    # svnlook changed 输出格式: "A   path/to/file" (前 4 列是动作标记)
    $path = ($line -replace '^.{4}', '').Trim()
    if ($path -match $pattern) { $bad += $path }
}

if ($bad.Count -gt 0) {
    [Console]::Error.WriteLine('提交被拒绝:以下路径是构建产物,不允许进版本库。')
    [Console]::Error.WriteLine('打包产物请走制品目录发布线(见后端仓 docs/design/release-pipeline.md)。')
    [Console]::Error.WriteLine('')
    $bad | Select-Object -First 20 | ForEach-Object { [Console]::Error.WriteLine($_) }
    if ($bad.Count -gt 20) { [Console]::Error.WriteLine("...共 $($bad.Count) 条,仅显示前 20 条") }
    exit 1
}
exit 0
