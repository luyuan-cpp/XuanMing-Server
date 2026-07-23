#!/bin/sh
# Pandora 后端 git 仓库服务端 pre-receive 钩子:拒收构建产物与超大文件
#
# 部署(自建 git 裸仓库):拷到 <裸仓库>/hooks/pre-receive 并 chmod +x。
# 托管平台(GitHub / GitLab)用平台自带能力替代:
#   - GitHub: Settings -> Rules(push rulesets: 限制路径 + 文件大小上限)
#   - GitLab: Settings -> Repository -> Push rules(file size limit + 文件名黑名单)
#
# 规则:
#   1. 路径黑名单:deploy/offline-images/*.tar 及任何 *.tar(镜像包走制品目录,不进 git)
#   2. 单文件超过 MAX_MB(默认 50MB)拒收
# 紧急放行:push 侧无法带日志,如确需放行由管理员临时移除钩子后恢复。

MAX_MB=${MAX_MB:-50}
MAX_BYTES=$((MAX_MB * 1024 * 1024))
ZERO=0000000000000000000000000000000000000000

fail=0
while read oldrev newrev refname; do
  [ "$newrev" = "$ZERO" ] && continue   # 删除分支
  if [ "$oldrev" = "$ZERO" ]; then
    range="$newrev"                     # 新分支:检查该分支全部可达提交里的新对象
  else
    range="$oldrev..$newrev"
  fi

  # 1) 路径黑名单
  bad_paths=$(git rev-list --objects "$range" 2>/dev/null | \
    awk '{ $1=""; sub(/^ +/,""); print }' | \
    grep -E '\.tar$' | sort -u)
  if [ -n "$bad_paths" ]; then
    echo "拒收:以下 tar 制品不允许进 git(走制品目录发布线,见 docs/design/release-pipeline.md):" 1>&2
    echo "$bad_paths" | head -10 1>&2
    fail=1
  fi

  # 2) 大文件
  big=$(git rev-list --objects "$range" 2>/dev/null | \
    git cat-file --batch-check='%(objecttype) %(objectsize) %(rest)' | \
    awk -v max="$MAX_BYTES" '$1 == "blob" && $2 > max { printf "%.1fMB  %s\n", $2/1048576, $3 }' | sort -u)
  if [ -n "$big" ]; then
    echo "拒收:以下文件超过 ${MAX_MB}MB 单文件上限:" 1>&2
    echo "$big" | head -10 1>&2
    fail=1
  fi
done

exit $fail
