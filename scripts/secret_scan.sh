#!/usr/bin/env bash
#
# 脱敏闸门：扫描「已跟踪文件」里是否混进了内网地址或凭据。
#
#   bash scripts/secret_scan.sh       # GitHub CI 与本地通用
#
# 退出码 0 = 干净；1 = 有命中（逐条打印）。
#
# 只放「强特征」：真令牌格式 / 私网 IPv4 / 私钥块。
# 刻意不按文件名匹配 —— 那类规则会命中 .gitignore 自己（它的职责就是按文件名
# 列出待忽略项），把闸门训练成「红了就改注释」的摆设，比没有闸门更糟。

set -uo pipefail

PATTERNS=(
    'dckr_pat_[A-Za-z0-9_-]{12,}'
    'gh[pousr]_[A-Za-z0-9]{20,}'
    'glpat-[A-Za-z0-9_-]{15,}'
    '-----BEGIN [A-Z ]*PRIVATE KEY-----'
    '\b192\.168\.[0-9]{1,3}\.[0-9]{1,3}\b'
    '\b10\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}\b'
    '\b172\.(1[6-9]|2[0-9]|3[01])\.[0-9]{1,3}\.[0-9]{1,3}\b'
)

# 注意：必须用 < <(...) 而非管道，否则 while 跑在子 shell 里，bad 传不出来。
mapfile -d '' files < <(git ls-files -z)
if [ "${#files[@]}" -eq 0 ]; then
    echo "::warning title=闸门未生效::git ls-files 未返回任何文件，请确认工作区已 checkout"
    exit 1
fi

bad=0
for pat in "${PATTERNS[@]}"; do
    hits="$(grep -nIE -- "$pat" "${files[@]}" 2>/dev/null)"
    if [ -n "$hits" ]; then
        printf '[命中] 模式 %s\n%s\n\n' "$pat" "$hits"
        bad=1
    fi
done

if [ "$bad" -ne 0 ]; then
    echo "::error title=脱敏闸门未通过::已跟踪文件里存在内网地址或凭据，公开仓库不可推送"
    exit 1
fi

printf '脱敏闸门通过：已检查 %d 个跟踪文件，未发现内网地址或凭据\n' "${#files[@]}"
