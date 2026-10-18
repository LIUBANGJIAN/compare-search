#!/usr/bin/env bash
#
# 冒烟测试：真的把服务跑起来，验证面板页、每个引擎的代理链路、配置接口与 URL 直达检索。
#
#   ENGINE=docker IMAGE=<镜像> bash scripts/smoke.sh   # CI 默认（真跑镜像）
#   ENGINE=local  BIN=./compare-search.exe bash scripts/smoke.sh
#
# 两种引擎共用同一套断言，保证 CI 里验的就是本机验过的同一件事。
#
# 刻意不使用 set -e：需要跑完全部断言后一次性汇总（告诉你到底坏了几处），
# 而不是第一条失败就退出。
#
# ⚠️ 不要写 `cmd | grep -q ...`：在 pipefail 下这是**会随机假失败**的写法 ——
# grep -q 一命中就退出，上游进程收到 SIGPIPE 返回非零，整条管道被判为失败。
# 实测本机侥幸通过、容器里失败。所有内容匹配都先取到变量（或落文件）再比。
set -uo pipefail

ENGINE="${ENGINE:-docker}"
IMAGE="${IMAGE:-compare-search:smoke}"
BIN="${BIN:-./compare-search}"
if [ "$ENGINE" = "local" ]; then
    : "${UPSTREAM_HOST:=127.0.0.1}"
else
    : "${UPSTREAM_HOST:=host.docker.internal}"
fi

PANEL_PORT="${PANEL_PORT:-18899}"
PROXY_PORT_BASE="${PROXY_PORT_BASE:-18898}"
MAX_ENGINES="${MAX_ENGINES:-8}"
SRC1_PORT="${SRC1_PORT:-11510}"
SRC2_PORT="${SRC2_PORT:-8787}"
SRC1_BASE="http://${UPSTREAM_HOST}:${SRC1_PORT}"
SRC2_BASE="http://${UPSTREAM_HOST}:${SRC2_PORT}"
PANEL="http://127.0.0.1:${PANEL_PORT}"

# 需要暴露的端口区间：面板端口 + 全部引擎代理端口
PORT_LO=$((PROXY_PORT_BASE - MAX_ENGINES + 1))
PORT_HI="$PANEL_PORT"
[ "$PORT_LO" -gt "$PANEL_PORT" ] && PORT_LO="$PANEL_PORT"
[ "$PORT_HI" -lt "$PROXY_PORT_BASE" ] && PORT_HI="$PROXY_PORT_BASE"

WORKDIR="$(mktemp -d)"
CNAME="t2h-smoke-$$"
APP_PID=""
MOCK_PID=""

CHECKS=0
FAILS=0
pass() { CHECKS=$((CHECKS + 1)); printf '  [OK]   %s\n' "$1"; }
fail() { CHECKS=$((CHECKS + 1)); FAILS=$((FAILS + 1)); printf '  [FAIL] %s\n' "$1"; }
note() { printf '\n== %s\n' "$1"; }

cleanup() {
    if [ "$ENGINE" = "docker" ]; then
        docker rm -f "$CNAME" >/dev/null 2>&1
    else
        [ -n "$APP_PID" ] && kill "$APP_PID" >/dev/null 2>&1
    fi
    [ -n "$MOCK_PID" ] && kill "$MOCK_PID" >/dev/null 2>&1
    return 0
}
trap cleanup EXIT

# Windows 上 Git Bash 的 /tmp 与原生程序看到的路径不是同一个位置，
# 交给被测程序的文件路径要转成 Windows 形式（mock 脚本不需要）。
to_native() {
    if command -v cygpath >/dev/null 2>&1; then cygpath -w "$1"; else printf '%s' "$1"; fi
}
COOKIE_PATH="$(to_native "$WORKDIR/cookie_115.txt")"
CONFIG_PATH="$(to_native "$WORKDIR/settings.json")"
DRAFT_PATH="$(to_native "$WORKDIR/draft.json")"

PY="$(command -v python3 || command -v python)"
if [ -z "$PY" ]; then
    echo "::error title=缺少 python::冒烟脚本需要 python3 来启动假上游与解析 JSON"
    exit 1
fi

# 预检端口：端口被别的进程占用时，被测服务会「安静地」退化到随机端口，
# 于是 curl 打到的是那个进程、断言结果全是误导 —— 实测踩过一次。
port_busy() {
    (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null || return 1
    return 0
}
for p in $(seq "$PORT_LO" "$PORT_HI"); do
    if port_busy "$p"; then
        echo "::error title=端口被占用::127.0.0.1:${p} 上已有进程在监听，请先停掉再跑冒烟"
        exit 1
    fi
done

# json_field <json 文本> <python 表达式，d 为解析后的对象>
json_field() {
    printf '%s' "$1" | "$PY" -c "import json,sys;d=json.load(sys.stdin);print($2)" 2>/dev/null
}

note "启动假上游 (:$SRC1_PORT / :$SRC2_PORT)"
MOCK_UPSTREAM_HOST="$UPSTREAM_HOST" "$PY" scripts/mock_upstream.py "$SRC1_PORT" "$SRC2_PORT" \
    >"$WORKDIR/mock.log" 2>&1 &
MOCK_PID=$!
for _ in $(seq 1 40); do
    curl -s -o /dev/null --max-time 2 "http://127.0.0.1:$SRC1_PORT/" && break
    sleep 0.25
done

note "启动被测服务（ENGINE=$ENGINE，端口区间 ${PORT_LO}-${PORT_HI}）"
if [ "$ENGINE" = "docker" ]; then
    # 必须把「面板端口 + 全部引擎代理端口」都映射出来：
    # 每个搜索引擎都有自己的代理端口，只映射前两个会让第 3 个引擎起不来。
    docker run -d --name "$CNAME" \
        --add-host=host.docker.internal:host-gateway \
        -e SRC1_BASE="$SRC1_BASE" \
        -e SRC2_BASE="$SRC2_BASE" \
        -p "127.0.0.1:${PORT_LO}-${PORT_HI}:${PORT_LO}-${PORT_HI}" \
        "$IMAGE" >/dev/null 2>&1
else
    SRC1_BASE="$SRC1_BASE" \
        SRC2_BASE="$SRC2_BASE" \
        LISTEN_HOST=127.0.0.1 \
        PAGE_PORT="$PANEL_PORT" \
        PROXY_PORT_BASE="$PROXY_PORT_BASE" \
        COOKIE_FILE="$COOKIE_PATH" \
        CONFIG_FILE="$CONFIG_PATH" \
        DRAFT_FILE="$DRAFT_PATH" \
        NO_BROWSER=1 \
        "$BIN" >"$WORKDIR/app.log" 2>&1 &
    APP_PID=$!
fi

read_config_file() {
    if [ "$ENGINE" = "docker" ]; then
        docker exec "$CNAME" cat /data/settings.json 2>/dev/null
    else
        cat "$WORKDIR/settings.json" 2>/dev/null
    fi
}

# ---------------------------------------------------------------------------
# 健康检查（引擎列表与端口分配都在这里）
# ---------------------------------------------------------------------------
note "健康检查"
READY=0
for _ in $(seq 1 60); do
    if curl -fsS --max-time 2 "$PANEL/healthz" >"$WORKDIR/health.json" 2>/dev/null; then
        READY=1
        break
    fi
    sleep 0.5
done

VER=""
E1_PORT=""
E2_PORT=""
E1_NAME=""
E2_NAME=""
if [ "$READY" = "1" ]; then
    HEALTH="$(cat "$WORKDIR/health.json")"
    pass "服务已就绪，/healthz 返回 200"
    case "$HEALTH" in
        *'"status":"ok"'*) pass "/healthz 报告 status=ok" ;;
        *) fail "/healthz 未报告 status=ok（实际：$(head -c 200 "$WORKDIR/health.json")）" ;;
    esac
    VER="$(json_field "$HEALTH" "d['version']")"
    if [ -n "$VER" ]; then
        pass "服务自报版本：$VER"
    else
        fail "服务未自报版本"
    fi
    if [ -n "${EXPECT_VERSION:-}" ]; then
        if [ "$VER" = "$EXPECT_VERSION" ]; then
            pass "自报版本与构建参数一致：$VER"
        else
            fail "自报版本 ${VER:-空} 与期望 ${EXPECT_VERSION} 不一致"
        fi
    fi

    NENG="$(json_field "$HEALTH" "len(d['engines'])")"
    if [ "$NENG" = "2" ]; then
        pass "/healthz 报告 2 个搜索引擎"
    else
        fail "/healthz 里的引擎数不正确（实际 ${NENG:-空}）"
    fi
    E1_NAME="$(json_field "$HEALTH" "d['engines'][0]['name']")"
    E2_NAME="$(json_field "$HEALTH" "d['engines'][1]['name']")"
    E1_PORT="$(json_field "$HEALTH" "d['engines'][0]['proxyPort']")"
    E2_PORT="$(json_field "$HEALTH" "d['engines'][1]['proxyPort']")"
    if [ -n "$E1_NAME" ] && [ -n "$E2_NAME" ]; then
        pass "引擎名已下发：$E1_NAME / $E2_NAME"
    else
        fail "引擎名缺失"
    fi
    if [ -n "$E1_PORT" ] && [ "$E1_PORT" != "0" ] && [ -n "$E2_PORT" ] && [ "$E2_PORT" != "0" ]; then
        pass "每个引擎都分到了代理端口：$E1_PORT / $E2_PORT"
    else
        fail "引擎代理端口分配失败（$E1_PORT / $E2_PORT）"
    fi
    # 端口必须落在被映射出来的区间里，否则宿主机根本连不上
    if [ -n "$E1_PORT" ] && [ "$E1_PORT" -ge "$PORT_LO" ] && [ "$E1_PORT" -le "$PORT_HI" ]; then
        pass "引擎端口落在已映射区间内"
    else
        fail "引擎端口 $E1_PORT 不在映射区间 ${PORT_LO}-${PORT_HI} 内"
    fi
else
    fail "服务未在超时内就绪"
    if [ "$ENGINE" = "docker" ]; then
        docker logs "$CNAME" 2>&1 | tail -30
    else
        tail -30 "$WORKDIR/app.log"
    fi
fi

# ---------------------------------------------------------------------------
# 配置文件自动生成（首次启动就该有，不用等用户点保存）
# ---------------------------------------------------------------------------
note "配置文件自动生成"
CFG_TEXT="$(read_config_file)"
case "$CFG_TEXT" in
    *'"engines"'*) pass "首次启动即自动生成了配置文件（含 engines）" ;;
    *) fail "配置文件未自动生成（实际：$(printf '%s' "$CFG_TEXT" | head -c 200)）" ;;
esac

# ---------------------------------------------------------------------------
# 面板页
# ---------------------------------------------------------------------------
note "面板页"
curl -s --max-time 5 "$PANEL/" >"$WORKDIR/page.html"
PAGE="$(cat "$WORKDIR/page.html")"

for id in kwInput searchBtn settingsBtn pager pagerInfo prevBtn nextBtn \
    miniPrev miniNext clearBtn listBox codesToggle splitter settingsDialog \
    appVersion directLink engineList addEngineBtn enginesSummary resultsCol; do
    case "$PAGE" in
        *"id=\"$id\""*) pass "面板页含 #$id" ;;
        *) fail "面板页缺少 #$id" ;;
    esac
done

case "$PAGE" in
    *'{SCRIPT}'* | *'{BOOTSTRAP}'* | *'{P1PORT}'*) fail "面板页残留未替换的占位符" ;;
    *) pass "面板页占位符已全部替换" ;;
esac
case "$PAGE" in
    *splitKeywords*) pass "面板页已内联前端脚本" ;;
    *) fail "面板页未内联前端脚本" ;;
esac
case "$PAGE" in
    *'id="bootstrap"'*'"items"'*) pass "面板页下发了番号引导数据" ;;
    *) fail "面板页未下发番号引导数据" ;;
esac
case "$PAGE" in
    *'"proxyPort"'*) pass "面板页下发了各引擎的代理端口" ;;
    *) fail "面板页未下发引擎代理端口" ;;
esac
# 「隐藏网站外壳」要给现成预设，用户不该被迫自己写 CSS 选择器
case "$PAGE" in
    *'"hidePresets"'*) pass "面板页下发了隐藏外壳的预设" ;;
    *) fail "面板页未下发隐藏外壳预设" ;;
esac
case "$PAGE" in
    *'"key":"common"'*) pass "预设立即包含「通用」方案" ;;
    *) fail "预设里缺少通用方案" ;;
esac
# 「清空」要在「检索」旁边（搜索栏里），而不是番号栏里
if printf '%s' "$PAGE" | "$PY" -c "
import sys
h = sys.stdin.read()
i = h.find('id=\"clearBtn\"')
j = h.find('class=\"codes-col\"')
sys.exit(0 if (i >= 0 and j >= 0 and i < j) else 1)
"; then
    pass "「清空」按钮在搜索栏里（检索旁边）"
else
    fail "「清空」按钮不在搜索栏里"
fi
case "$PAGE" in
    *'btn-clear'*) pass "「清空」带两段式确认样式" ;;
    *) fail "「清空」缺少确认态样式" ;;
esac
case "$PAGE" in
    *'"version":"'"$VER"'"'*) pass "面板页下发了版本号 $VER（设置里显示）" ;;
    *) fail "面板页未下发版本号" ;;
esac
if [ -n "$E1_NAME" ]; then
    case "$PAGE" in
        *"$E1_NAME"*) pass "结果面板标题使用引擎名（$E1_NAME）" ;;
        *) fail "结果面板标题未使用引擎名" ;;
    esac
fi
# 番号栏能滚动的前提是外层高度固定：容器用固定 height，flex 子项 min-height:0，
# 否则番号一多就把整页撑长（用户反馈过的问题）。这两条是防回退的护栏。
case "$PAGE" in
    *'height: calc(100vh'*) pass "外层容器高度固定（番号栏才能内部滚动）" ;;
    *) fail "外层容器未固定高度，番号多时会把页面撑长" ;;
esac
case "$PAGE" in
    *'min-height: 0'*) pass "flex 子项允许收缩（min-height: 0）" ;;
    *) fail "flex 子项缺少 min-height: 0，番号栏无法滚动" ;;
esac

# ---------------------------------------------------------------------------
# 每个引擎各自的代理链路
# ---------------------------------------------------------------------------
check_engine_proxy() {
    local label="$1" port="$2" path="$3" marker="$4"
    note "$label（端口 $port，路径 $path）"
    if [ -z "$port" ] || [ "$port" = "0" ]; then
        fail "$label 没有可用端口"
        return
    fi
    local code
    code="$(curl -s --max-time 5 -o "$WORKDIR/b.html" -D "$WORKDIR/h.txt" \
        -w '%{http_code}' "http://127.0.0.1:${port}${path}?q=abc")"
    # 先断言 HTTP 状态：否则请求失败时「头里没有 X-Frame-Options」会**假通过**
    if [ "$code" = "200" ]; then
        pass "$label 返回 200"
    else
        fail "$label 返回 HTTP $code（端口 $port 可能没被映射出来）"
    fi
    local body
    body="$(cat "$WORKDIR/b.html")"

    if grep -qi '^x-frame-options:' "$WORKDIR/h.txt"; then
        fail "$label 响应仍带 X-Frame-Options"
    elif [ "$code" = "200" ]; then
        pass "$label 已剥离 X-Frame-Options"
    else
        fail "$label 无法判断是否剥离了 X-Frame-Options（请求未成功）"
    fi
    if grep -qi '^content-security-policy:' "$WORKDIR/h.txt"; then
        fail "$label 响应仍带 Content-Security-Policy"
    elif [ "$code" = "200" ]; then
        pass "$label 已剥离 Content-Security-Policy"
    else
        fail "$label 无法判断是否剥离了 CSP（请求未成功）"
    fi
    case "$body" in
        *data-local-inject*) pass "$label 已注入隐藏外壳样式" ;;
        *) fail "$label 未注入隐藏外壳样式" ;;
    esac
    case "$body" in
        *"$marker"*) pass "$label 代理链路可达上游" ;;
        *) fail "$label 代理未取到上游内容（实际：$(head -c 200 "$WORKDIR/b.html")）" ;;
    esac
    case "$body" in
        *"$UPSTREAM_HOST"*) fail "$label 正文仍含上游绝对地址" ;;
        *) pass "$label 绝对地址已改写为相对" ;;
    esac
}

check_engine_proxy "引擎1 代理" "$E1_PORT" "/files" "MOCK-UPSTREAM-$SRC1_PORT"
check_engine_proxy "引擎2 代理" "$E2_PORT" "/s" "MOCK-UPSTREAM-$SRC2_PORT"

# ---------------------------------------------------------------------------
# 隐藏站点外壳：光有标记不算数，要看注入的到底是不是真规则、有没有兜底
# ---------------------------------------------------------------------------
note "隐藏站点外壳"
HIDE1="$(curl -s --max-time 5 "http://127.0.0.1:${E1_PORT}/files?q=abc")"
case "$HIDE1" in
    *'<style data-local-inject>'*'display:none!important'*)
        pass "注入了真实的隐藏规则（不是空壳样式）" ;;
    *) fail "注入的隐藏规则内容不对" ;;
esac
case "$HIDE1" in
    *'[data-sidebar'*)
        pass "注入的是该引擎自己的 hideCSS" ;;
    *) fail "注入的 hideCSS 不是该引擎配置的那份" ;;
esac
case "$HIDE1" in
    *'<script data-local-inject>'*'important'*)
        pass "带行内 !important 兜底脚本（压得住站点自己的 !important）" ;;
    *) fail "缺少行内 !important 兜底脚本" ;;
esac
# 注入块必须落在 </body> 之前：源序靠后才能在同样 !important 时胜出
if printf '%s' "$HIDE1" | "$PY" -c "
import sys
h = sys.stdin.read()
i = h.find('data-local-inject')
j = h.find('</body>')
sys.exit(0 if (i >= 0 and j >= 0 and i < j) else 1)
"; then
    pass "注入块位于文档末尾（</body> 之前），源序靠后"
else
    fail "注入块不在 </body> 之前，站点自己的 !important 会压掉它"
fi

# ---------------------------------------------------------------------------
# 这个网站「带关键字直接检索」—— 对接方只需要这一个 URL
# ---------------------------------------------------------------------------
note "带关键字直接检索"

SEARCH="$(curl -s --max-time 5 "$PANEL/api/search?q=abc")"
case "$SEARCH" in
    *'"keyword":"abc"'*) pass "GET /api/search 返回关键字" ;;
    *) fail "GET /api/search 响应不正确（实际：$(printf '%s' "$SEARCH" | head -c 200)）" ;;
esac
DIRECT="$(json_field "$SEARCH" "d['url']")"
if [ "$DIRECT" = "${PANEL}/?q=abc" ]; then
    pass "主字段就是这个网站的直达地址：$DIRECT"
else
    fail "网站直达地址不正确（实际：${DIRECT:-空}）"
fi
if [ "$(json_field "$SEARCH" "len(d['engines'])")" = "2" ]; then
    pass "附带返回各引擎地址（仅供需要直抓某引擎 HTML 的场景）"
else
    fail "附带的引擎地址数量不正确"
fi

# 这个网站带关键字的几种写法都要能用
for target in "/?q=abc" "/search?q=abc" "/?keyword=abc" "/?kw=abc"; do
    BODY="$(curl -s --max-time 5 "${PANEL}${target}")"
    case "$BODY" in
        *'"q":"abc"'*) pass "这个网站 ${target} 能带关键字检索" ;;
        *) fail "${target} 未把关键字带进面板页" ;;
    esac
done

CODE="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "$PANEL/api/search")"
if [ "$CODE" = "400" ]; then
    pass "GET /api/search 缺少关键字时返回 400"
else
    fail "GET /api/search 缺少关键字应返回 400（实际 $CODE）"
fi

PAGE404="$(curl -s --max-time 5 "$PANEL/some-unknown-path")"
case "$PAGE404" in
    *'/?q='*) pass "404 页面把「带关键字检索」的用法写出来了" ;;
    *) fail "404 页面未提示带关键字检索的用法" ;;
esac

# ---------------------------------------------------------------------------
# 配置接口：读取 / 跨站拒绝 / 校验 / 增删引擎 / 热切换 / 持久化
# 注意放在代理断言之后 —— 它会改掉引擎地址。
# ---------------------------------------------------------------------------
note "配置接口"

CFG="$(curl -s --max-time 5 "$PANEL/api/config")"
if [ "$(json_field "$CFG" "len(d['engines'])")" = "2" ]; then
    pass "GET /api/config 返回引擎列表"
else
    fail "GET /api/config 响应不正确（实际：$(printf '%s' "$CFG" | head -c 200)）"
fi

CODE="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 -X POST \
    -H 'Content-Type: application/json' -H 'Origin: http://evil.example' \
    -d '{"engines":[{"name":"x","base":"http://192.0.2.99:1","path":"/s"}]}' \
    "$PANEL/api/config")"
if [ "$CODE" = "403" ]; then
    pass "跨站 Origin 的写入被拒绝（403）"
else
    fail "跨站 Origin 的写入未被拒绝（实际 HTTP $CODE）"
fi

CODE="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 -X POST \
    -H 'Content-Type: application/json' \
    -d '{"engines":[{"name":"x","base":"ftp://bad","path":"/s"}]}' \
    "$PANEL/api/config")"
if [ "$CODE" = "400" ]; then
    pass "非法地址被拒绝（400）"
else
    fail "非法地址未被拒绝（实际 HTTP $CODE）"
fi

CODE="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 -X POST \
    -H 'Content-Type: application/json' \
    -d '{"engines":[]}' \
    "$PANEL/api/config")"
if [ "$CODE" = "400" ]; then
    pass "空引擎列表被拒绝（400）"
else
    fail "空引擎列表未被拒绝（实际 HTTP $CODE）"
fi

# 真正保存：把引擎1 改到另一个假上游端口，并新增第 3 个引擎
CODE="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 -X POST \
    -H 'Content-Type: application/json' \
    -d "{\"engines\":[{\"name\":\"$E1_NAME\",\"base\":\"$SRC2_BASE\",\"path\":\"/files\"},{\"name\":\"$E2_NAME\",\"base\":\"$SRC2_BASE\",\"path\":\"/s\"},{\"name\":\"新增引擎\",\"base\":\"$SRC1_BASE\",\"path\":\"/s\"}]}" \
    "$PANEL/api/config")"
if [ "$CODE" = "200" ]; then
    pass "保存 3 个引擎成功（200）"
else
    fail "保存引擎配置失败（实际 HTTP $CODE）"
fi

HEALTH2="$(curl -s --max-time 5 "$PANEL/healthz")"
if [ "$(json_field "$HEALTH2" "len(d['engines'])")" = "3" ]; then
    pass "新增引擎后 /healthz 立刻反映（无需重启）"
else
    fail "新增引擎未生效（实际 $(json_field "$HEALTH2" "len(d['engines'])") 个）"
fi
E3_PORT="$(json_field "$HEALTH2" "d['engines'][2]['proxyPort']")"
if [ -n "$E3_PORT" ] && [ "$E3_PORT" != "0" ]; then
    pass "新增引擎拿到了自己的代理端口：$E3_PORT"
else
    fail "新增引擎没有拿到代理端口（实际 ${E3_PORT:-空}）"
fi
# 新增的那个引擎必须真的能通过它的端口取到内容（这是端口池 + 映射都对的最终判据）
if [ -n "$E3_PORT" ] && [ "$E3_PORT" != "0" ]; then
    E3_BODY="$(curl -s --max-time 5 "http://127.0.0.1:${E3_PORT}/s?q=abc")"
    case "$E3_BODY" in
        *"MOCK-UPSTREAM-$SRC1_PORT"*) pass "新增引擎的代理端口真的可用" ;;
        *) fail "新增引擎的代理端口取不到内容（实际：$(printf '%s' "$E3_BODY" | head -c 200)）" ;;
    esac
fi

curl -s --max-time 5 "http://127.0.0.1:${E1_PORT}/files?q=abc" >"$WORKDIR/b1b.html"
case "$(cat "$WORKDIR/b1b.html")" in
    *"MOCK-UPSTREAM-$SRC2_PORT"*) pass "改地址后引擎1 的代理已热切换" ;;
    *) fail "引擎1 代理未热切换（实际：$(head -c 200 "$WORKDIR/b1b.html")）" ;;
esac

CFG_TEXT="$(read_config_file)"
case "$CFG_TEXT" in
    *"$SRC2_BASE"*) pass "配置已落盘，重启后仍生效（持久化）" ;;
    *) fail "配置文件未落盘或内容不正确（实际：$(printf '%s' "$CFG_TEXT" | head -c 200)）" ;;
esac

# ---------------------------------------------------------------------------
# 引擎的启用 / 停用开关
# ---------------------------------------------------------------------------
note "启用 / 停用开关"

case "$PAGE" in
    *'id="enginesSummary"'*) pass "设置里有「已启用 N 个」计数" ;;
    *) fail "设置里缺少启用计数" ;;
esac
case "$PAGE" in
    *'switch-track'*) pass "每个引擎都带启用开关" ;;
    *) fail "设置里没有启用开关" ;;
esac
case "$PAGE" in
    *'"enabled"'*) pass "下发的引擎数据里带启用状态" ;;
    *) fail "引擎数据里没有启用状态字段" ;;
esac

# 停用中间那个（第 2 个），验证：运行时少一个、后面的引擎接上前面的端口
CODE="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 -X POST \
    -H 'Content-Type: application/json' \
    -d "{\"engines\":[{\"name\":\"$E1_NAME\",\"base\":\"$SRC2_BASE\",\"path\":\"/files\",\"enabled\":true},{\"name\":\"$E2_NAME\",\"base\":\"$SRC2_BASE\",\"path\":\"/s\",\"enabled\":false},{\"name\":\"新增引擎\",\"base\":\"$SRC1_BASE\",\"path\":\"/s\",\"enabled\":true}]}" \
    "$PANEL/api/config")"
if [ "$CODE" = "200" ]; then
    pass "可以通过开关停用某个引擎（200）"
else
    fail "停用引擎失败（实际 HTTP $CODE）"
fi

HEALTH4="$(curl -s --max-time 5 "$PANEL/healthz")"
if [ "$(json_field "$HEALTH4" "len(d['engines'])")" = "2" ]; then
    pass "停用后运行时只剩 2 个引擎"
else
    fail "停用后运行时引擎数不对（实际 $(json_field "$HEALTH4" "len(d['engines'])") 个）"
fi
if [ "$(json_field "$HEALTH4" "d['engines'][1]['proxyPort']")" = "$((PROXY_PORT_BASE - 1))" ]; then
    pass "停用中间那个后，后面的引擎接上了下一个端口"
else
    fail "停用后端口没有顺延（实际 $(json_field "$HEALTH4" "d['engines'][1]['proxyPort']")）"
fi
# 配置里仍保留 3 个（停用的那个要留着，方便以后打开）
CFG2="$(curl -s --max-time 5 "$PANEL/api/config")"
if [ "$(json_field "$CFG2" "len(d['engines'])")" = "3" ]; then
    pass "配置里仍保留 3 个引擎（停用的不删掉）"
else
    fail "停用变成了删除（实际配置里 $(json_field "$CFG2" "len(d['engines'])") 个）"
fi
case "$(read_config_file)" in
    *'"enabled": false'*) pass "停用状态已落盘" ;;
    *) fail "停用状态没有写进配置文件" ;;
esac

# ---------------------------------------------------------------------------
# 关键字草稿：存在服务端，关掉浏览器也不会丢
# ---------------------------------------------------------------------------
note "关键字草稿"

CODE="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 -X POST \
    -H 'Content-Type: application/json' \
    -d '{"text":"ABP-1\nSSIS-2","index":1}' \
    "$PANEL/api/draft")"
if [ "$CODE" = "200" ]; then
    pass "可以把关键字草稿存到服务端"
else
    fail "保存草稿失败（实际 HTTP $CODE）"
fi

DRAFT_PAGE="$(curl -s --max-time 5 "$PANEL/")"
case "$DRAFT_PAGE" in
    *'"text":"ABP-1\nSSIS-2"'*)
        pass "重新打开页面时草稿随页面下发（关掉浏览器也还在）" ;;
    *) fail "页面未带上服务端草稿" ;;
esac

CODE="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 -X POST \
    -H 'Content-Type: application/json' -H 'Origin: http://evil.example' \
    -d '{"text":"evil"}' "$PANEL/api/draft")"
if [ "$CODE" = "403" ]; then
    pass "跨站写草稿被拒绝（403）"
else
    fail "跨站写草稿未被拒绝（实际 HTTP $CODE）"
fi

CODE="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 -X POST \
    -H 'Content-Type: application/json' \
    -d '{"text":"","index":-1}' "$PANEL/api/draft")"
DRAFT_PAGE2="$(curl -s --max-time 5 "$PANEL/")"
if [ "$CODE" = "200" ] && printf '%s' "$DRAFT_PAGE2" | grep -q '"text":""'; then
    pass "清空后草稿真的空了"
else
    fail "清空草稿未生效（HTTP $CODE）"
fi
if [ "$ENGINE" = "docker" ]; then
    DRAFT_FILE_BODY="$(docker exec "$CNAME" cat /data/draft.json 2>/dev/null)"
else
    DRAFT_FILE_BODY="$(cat "$WORKDIR/draft.json" 2>/dev/null)"
fi
case "$DRAFT_FILE_BODY" in
    *'"text":""'*) pass "草稿已落盘（重启后仍生效）" ;;
    *) fail "草稿未落盘（实际：$(printf '%s' "$DRAFT_FILE_BODY" | head -c 120)）" ;;
esac

# ---------------------------------------------------------------------------
# 汇总
# ---------------------------------------------------------------------------
# 断言计数下限：不使用 set -e 的脚本，「半途没跑起来」与「全部通过」退出码都是 0，
# 这条锁专堵那种空转的绿。现在实际有 90 条左右，取下限 40 留足富余。
MIN_CHECKS=40
if [ "$CHECKS" -lt "$MIN_CHECKS" ]; then
    echo "::error title=断言数异常::只执行了 ${CHECKS} 条，低于下限 ${MIN_CHECKS}"
    exit 1
fi

printf '\n共 %d 条断言，失败 %d 条\n' "$CHECKS" "$FAILS"
if [ "$FAILS" -ne 0 ]; then
    echo "::error title=冒烟失败::${FAILS} / ${CHECKS} 条断言未通过"
    exit 1
fi

DIGEST=""
if [ "$ENGINE" = "docker" ]; then
    DIGEST="$(docker image inspect --format '{{index .RepoDigests 0}}' "$IMAGE" 2>/dev/null)"
fi
echo "::notice title=冒烟通过::断言=${CHECKS} 条全部通过 | 引擎=${ENGINE} | 镜像/程序=${IMAGE:-$BIN} | digest=${DIGEST:-无} | 自报版本=${VER:-未知}"
