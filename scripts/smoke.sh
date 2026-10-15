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
SRC1_PORT="${SRC1_PORT:-11510}"
SRC2_PORT="${SRC2_PORT:-8787}"
SRC1_BASE="http://${UPSTREAM_HOST}:${SRC1_PORT}"
SRC2_BASE="http://${UPSTREAM_HOST}:${SRC2_PORT}"
PANEL="http://127.0.0.1:${PANEL_PORT}"

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
if port_busy "$PANEL_PORT" || port_busy "$PROXY_PORT_BASE"; then
    echo "::error title=端口被占用::127.0.0.1:${PANEL_PORT} 或 :${PROXY_PORT_BASE} 上已有进程在监听，请先停掉再跑冒烟"
    exit 1
fi

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

note "启动被测服务（ENGINE=$ENGINE）"
if [ "$ENGINE" = "docker" ]; then
    docker run -d --name "$CNAME" \
        --add-host=host.docker.internal:host-gateway \
        -e SRC1_BASE="$SRC1_BASE" \
        -e SRC2_BASE="$SRC2_BASE" \
        -p "127.0.0.1:${PANEL_PORT}:${PANEL_PORT}" \
        -p "127.0.0.1:${PROXY_PORT_BASE}:${PROXY_PORT_BASE}" \
        "$IMAGE" >/dev/null 2>&1
else
    SRC1_BASE="$SRC1_BASE" \
        SRC2_BASE="$SRC2_BASE" \
        LISTEN_HOST=127.0.0.1 \
        PAGE_PORT="$PANEL_PORT" \
        PROXY_PORT_BASE="$PROXY_PORT_BASE" \
        COOKIE_FILE="$COOKIE_PATH" \
        CONFIG_FILE="$CONFIG_PATH" \
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
    if printf '%s' "$HEALTH" | grep -q '"status":"ok"'; then
        pass "/healthz 报告 status=ok"
    else
        fail "/healthz 未报告 status=ok（实际：$(head -c 200 "$WORKDIR/health.json")）"
    fi
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
if read_config_file | grep -q '"engines"'; then
    pass "首次启动即自动生成了配置文件（含 engines）"
else
    fail "配置文件未自动生成（实际：$(read_config_file | head -c 200)）"
fi

# ---------------------------------------------------------------------------
# 面板页
# ---------------------------------------------------------------------------
note "面板页"
curl -s --max-time 5 "$PANEL/" >"$WORKDIR/page.html"
PAGE="$(cat "$WORKDIR/page.html")"

for id in kwInput searchBtn settingsBtn pager pagerInfo pagerKw prevBtn nextBtn \
    clearBtn listBox codesToggle splitter settingsDialog engineList addEngineBtn resultsCol; do
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
if [ -n "$E1_NAME" ]; then
    case "$PAGE" in
        *"$E1_NAME"*) pass "结果面板标题使用引擎名（$E1_NAME）" ;;
        *) fail "结果面板标题未使用引擎名" ;;
    esac
fi

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
    curl -s --max-time 5 -D "$WORKDIR/h.txt" "http://127.0.0.1:${port}${path}?q=abc" >"$WORKDIR/b.html"
    local body
    body="$(cat "$WORKDIR/b.html")"

    grep -qi '^x-frame-options:' "$WORKDIR/h.txt" \
        && fail "$label 响应仍带 X-Frame-Options" \
        || pass "$label 已剥离 X-Frame-Options"
    grep -qi '^content-security-policy:' "$WORKDIR/h.txt" \
        && fail "$label 响应仍带 Content-Security-Policy" \
        || pass "$label 已剥离 Content-Security-Policy"
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
# URL 直达检索
# ---------------------------------------------------------------------------
note "URL 直达检索"

SEARCH="$(curl -s --max-time 5 "$PANEL/api/search?q=abc")"
if printf '%s' "$SEARCH" | grep -q '"keyword":"abc"'; then
    pass "GET /api/search 返回关键字"
else
    fail "GET /api/search 响应不正确（实际：$(head -c 200 <<<"$SEARCH")）"
fi
if [ "$(json_field "$SEARCH" "len(d['engines'])")" = "2" ]; then
    pass "GET /api/search 返回每个引擎的检索地址"
else
    fail "GET /api/search 引擎地址数量不正确"
fi
if [ "$(json_field "$SEARCH" "d['engines'][0]['url']")" = "http://127.0.0.1:${E1_PORT}/files?q=abc" ]; then
    pass "检索地址用请求主机名 + 引擎端口拼装"
else
    fail "检索地址不正确（实际：$(json_field "$SEARCH" "d['engines'][0]['url']")）"
fi
CODE="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "$PANEL/api/search")"
if [ "$CODE" = "400" ]; then
    pass "GET /api/search 缺少 q 时返回 400"
else
    fail "GET /api/search 缺少 q 应返回 400（实际 $CODE）"
fi
if curl -s --max-time 5 "$PANEL/?q=abc" | grep -q '"q":"abc"'; then
    pass "面板页 /?q=abc 把关键字带进了引导数据"
else
    fail "面板页未把 URL 里的关键字带进引导数据"
fi

# ---------------------------------------------------------------------------
# 配置接口：读取 / 跨站拒绝 / 校验 / 增删引擎 / 热切换 / 持久化
# 注意放在代理断言之后 —— 它会改掉引擎地址。
# ---------------------------------------------------------------------------
note "配置接口"

CFG="$(curl -s --max-time 5 "$PANEL/api/config")"
if [ "$(json_field "$CFG" "len(d['engines'])")" = "2" ]; then
    pass "GET /api/config 返回引擎列表"
else
    fail "GET /api/config 响应不正确（实际：$(head -c 200 <<<"$CFG")）"
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

curl -s --max-time 5 "http://127.0.0.1:${E1_PORT}/files?q=abc" >"$WORKDIR/b1b.html"
case "$(cat "$WORKDIR/b1b.html")" in
    *"MOCK-UPSTREAM-$SRC2_PORT"*) pass "改地址后引擎1 的代理已热切换" ;;
    *) fail "引擎1 代理未热切换（实际：$(head -c 200 "$WORKDIR/b1b.html")）" ;;
esac

if read_config_file | grep -q "$SRC2_BASE"; then
    pass "配置已落盘，重启后仍生效（持久化）"
else
    fail "配置文件未落盘或内容不正确（实际：$(read_config_file | head -c 200)）"
fi

# ---------------------------------------------------------------------------
# 汇总
# ---------------------------------------------------------------------------
# 断言计数下限：不使用 set -e 的脚本，「半途没跑起来」与「全部通过」退出码都是 0，
# 这条锁专堵那种空转的绿。取实际条数的一半左右并留富余，别贴着实际值。
MIN_CHECKS=24
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
