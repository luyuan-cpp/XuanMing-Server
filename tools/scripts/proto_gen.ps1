# Pandora Proto 代码生成
#
# 用法:
#   pwsh tools/scripts/proto_gen.ps1            # buf lint + 生成 go pb
#   pwsh tools/scripts/proto_gen.ps1 -Lint      # 只 lint
#   pwsh tools/scripts/proto_gen.ps1 -Cpp       # 同时生成 cpp pb(给 UE 仓库用)
#   pwsh tools/scripts/proto_gen.ps1 -Breaking  # 检测 breaking change(对比 main 分支)
#
# 前置:必须装 buf。安装方式:
#   winget install bufbuild.buf
#   或 scoop install buf
#   或下载 https://github.com/bufbuild/buf/releases
#
# 第一次 buf generate 会从 buf.build 拉远程插件(protoc-gen-go / protoc-gen-go-grpc 等),
# 需要外网。

param(
    [switch]$Lint,
    [switch]$Cpp,
    [switch]$Breaking
)

$ErrorActionPreference = "Stop"

$ProjectRoot = Resolve-Path "$PSScriptRoot/../.."
$ProtoDir    = "$ProjectRoot/proto"

# 检查 buf
$bufCmd = Get-Command buf -ErrorAction SilentlyContinue
if ($null -eq $bufCmd) {
    Write-Host "[ERR] buf 未安装" -ForegroundColor Red
    Write-Host "  请运行下面任一命令安装:"
    Write-Host "    winget install bufbuild.buf"
    Write-Host "    scoop install buf"
    Write-Host "  或访问 https://github.com/bufbuild/buf/releases"
    exit 1
}

Write-Host "===== Pandora proto gen =====" -ForegroundColor Cyan
Write-Host "buf:   $($bufCmd.Source)"
Write-Host "proto: $ProtoDir"

Push-Location $ProtoDir
try {
    # 1. lint
    Write-Host ""
    Write-Host "[1] buf lint" -ForegroundColor Yellow
    & buf lint
    if ($LASTEXITCODE -ne 0) {
        Write-Host "[ERR] buf lint failed" -ForegroundColor Red
        exit 1
    }
    Write-Host "  OK" -ForegroundColor Green

    if ($Lint) {
        Write-Host ""
        Write-Host "(仅 lint 模式,跳过 generate)" -ForegroundColor DarkGray
        return
    }

    # 2. breaking(可选)
    if ($Breaking) {
        Write-Host ""
        Write-Host "[2] buf breaking against main" -ForegroundColor Yellow
        & buf breaking --against "$ProjectRoot/.git#branch=main"
        if ($LASTEXITCODE -ne 0) {
            Write-Host "[ERR] breaking change detected!" -ForegroundColor Red
            exit 1
        }
        Write-Host "  OK" -ForegroundColor Green
    }

    # 3. generate go
    Write-Host ""
    Write-Host "[3] buf generate go" -ForegroundColor Yellow
    & buf generate --template buf.gen.go.yaml
    if ($LASTEXITCODE -ne 0) {
        Write-Host "[ERR] go generate failed" -ForegroundColor Red
        exit 1
    }
    Write-Host "  OK → $ProtoDir/gen/go/" -ForegroundColor Green

    # 4. generate cpp(可选)
    if ($Cpp) {
        Write-Host ""
        Write-Host "[4] buf generate cpp" -ForegroundColor Yellow
        & buf generate --template buf.gen.cpp.yaml
        if ($LASTEXITCODE -ne 0) {
            Write-Host "[ERR] cpp generate failed" -ForegroundColor Red
            exit 1
        }
        Write-Host "  OK → $ProtoDir/gen/cpp/" -ForegroundColor Green
    }

    # 5. 统计
    Write-Host ""
    Write-Host "===== 产物 =====" -ForegroundColor Green
    $goFiles = Get-ChildItem -Path "$ProtoDir/gen/go" -Filter *.go -Recurse -ErrorAction SilentlyContinue
    Write-Host "go pb:  $($goFiles.Count) files"
    if ($Cpp) {
        $cppFiles = Get-ChildItem -Path "$ProtoDir/gen/cpp" -Filter *.cc -Recurse -ErrorAction SilentlyContinue
        Write-Host "cpp pb: $($cppFiles.Count) files"
    }
} finally {
    Pop-Location
}
