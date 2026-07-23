#!/bin/sh
# Pandora 客户端 SVN 仓库服务端 pre-commit 钩子:构建产物路径黑名单
#
# 作用:在服务器端直接拒绝把打包产物/引擎中间产物提交进版本库
# (本地 svn:ignore 只是君子协定,挡不住 svn add --force / 新检出的机器)。
#
# 部署(Linux svnserve / Apache mod_dav_svn):
#   1. 拷到 <仓库路径>/hooks/pre-commit  (无扩展名)
#   2. chmod +x pre-commit
#   3. 如已有 pre-commit,把本文件改名为 pre-commit-blacklist.sh 并在原钩子里调用:
#        "$REPOS/hooks/pre-commit-blacklist.sh" "$REPOS" "$TXN" || exit 1
#
# 部署(VisualSVN Server / Windows):用同目录的 svn-pre-commit.bat。
#
# 紧急放行:管理员在提交日志里带 [hook-override] 字样可跳过本钩子
# (仅限管理员救急,例如迁移历史;日常提交不得使用)。

REPOS="$1"
TXN="$2"
SVNLOOK=${SVNLOOK:-/usr/bin/svnlook}

# 日志带 [hook-override] 则放行
LOG=$("$SVNLOOK" log -t "$TXN" "$REPOS")
case "$LOG" in
  *"[hook-override]"*) exit 0 ;;
esac

# 路径黑名单:
#   - Packages/            打包输出(BuildCookRun 产物)
#   - Saved/ Intermediate/ DerivedDataCache/  引擎中间产物(任意层级)
#   - *.tar *.pak *.ucas *.utoc  制品文件(镜像包 / cook 产物,不允许出现在源码库任何位置)
# 注意:本仓库有意纳管 Pandora/Binaries(美术/策划不编译,靠 svn 同步编辑器 DLL),
# 因此 Binaries 不在黑名单内,不要照抄通用 UE 模板把它加回来。
BAD=$("$SVNLOOK" changed -t "$TXN" "$REPOS" | \
  awk '{ $1=""; sub(/^ +/,""); print }' | \
  grep -E '(^Packages/|^Packages$|(^|/)(Saved|Intermediate|DerivedDataCache)(/|$)|\.(tar|pak|ucas|utoc)$)')

if [ -n "$BAD" ]; then
  echo "提交被拒绝:以下路径是构建产物,不允许进版本库。" 1>&2
  echo "打包产物请走制品目录发布线(见后端仓 docs/design/release-pipeline.md)。" 1>&2
  echo "" 1>&2
  echo "$BAD" | head -20 1>&2
  N=$(echo "$BAD" | wc -l)
  if [ "$N" -gt 20 ]; then echo "...共 $N 条,仅显示前 20 条" 1>&2; fi
  exit 1
fi

exit 0
