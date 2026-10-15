#!/usr/bin/env bash
#
# 冒烟测试：真的把服务跑起来，验证面板页、两条代理链路、以及配置接口与持久化。
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
PROXY_PORT="${PROXY_PORT:-18898}"
SRC1_PORT="${SRC1_PORT:-11510}"
SRC2_PORT="${SRC2_PORT:-8787}"

S1_BASE="http://${UPSTREAM_HOST}:${SRC1_PORT}"
S2_BASE="http://${UPSTREAM_HOST}:${SRC2_PORT}"
PANEL="http://127.0.0.1:${PANEL_PORT}"
PROXY1="http://127.0.0.1:${PROXY_PORT}"

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
    echo "::error title=缺少 python::冒烟脚本需要 python3 来启动假上游"
    exit 1
fi

# 预检端口：端口被别的进程占用时，被测服务会「安静地」退化到随机端口，
# 于是 curl 打到的是那个进程、断言结果全是误导 —— 实测踩过一次。
port_busy() {
    (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null || return 1
    return 0
}
if port_busy "$PANEL_PORT" || port_busy "$PROXY_PORT"; then
    echo "::error title=端口被占用::127.0.0.1:${PANEL_PORT} 或 :${PROXY_PORT} 上已有进程在监听，请先停掉再跑冒烟"
    exit 1
fi

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
        -e SRC1_BASE="$S1_BASE" \
        -e SRC2_BASE="$S2_BASE" \
        -p "127.0.0.1:${PANEL_PORT}:${PANEL_PORT}" \
        -p "127.0.0.1:${PROXY_PORT}:${PROXY_PORT}" \
        "$IMAGE" >/dev/null 2>&1
else
    SRC1_BASE="$S1_BASE" \
        SRC2_BASE="$S2_BASE" \
        LISTEN_HOST=127.0.0.1 \
        PAGE_PORT="$PANEL_PORT" \
        PROXY1_PORT="$PROXY_PORT" \
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
# 就绪探测与健康检查
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
if [ "$READY" = "1" ]; then
    pass "服务已就绪，/healthz 返回 200"
    if grep -q '"status":"ok"' "$WORKDIR/health.json"; then
        pass "/healthz 报告 status=ok"
    else
        fail "/healthz 未报告 status=ok（实际：$(head -c 200 "$WORKDIR/health.json")）"
    fi
    VER="$(sed -n 's/.*"version":"\([^"]*\)".*/\1/p' "$WORKDIR/health.json")"
    if [ -n "$VER" ]; then
        pass "服务自报版本：$VER"
    else
        fail "服务未自报版本（实际：$(head -c 200 "$WORKDIR/health.json")）"
    fi
    if [ -n "${EXPECT_VERSION:-}" ]; then
        if [ "$VER" = "$EXPECT_VERSION" ]; then
            pass "自报版本与构建参数一致：$VER"
        else
            fail "自报版本 ${VER:-空} 与期望 ${EXPECT_VERSION} 不一致"
        fi
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
# 面板页
# ---------------------------------------------------------------------------
note "面板页"
curl -s --max-time 5 "$PANEL/" >"$WORKDIR/page.html"
PAGE="$(cat "$WORKDIR/page.html")"

for id in kwInput searchBtn settingsBtn pager pagerInfo pagerKw prevBtn nextBtn \
    frame1 frame2 clearBtn listBox codesToggle splitter settingsDialog; do
    case "$PAGE" in
        *"id=\"$id\""*) pass "面板页含 #$id" ;;
        *) fail "面板页缺少 #$id" ;;
    esac
done

case "$PAGE" in
    *"data-p1-port=\"$PROXY_PORT\""*) pass "面板页注入了源1 端口 $PROXY_PORT" ;;
    *) fail "面板页未注入源1 端口" ;;
esac

case "$PAGE" in
    *'{P1PORT}'* | *'{BOOTSTRAP}'* | *'{SCRIPT}'*) fail "面板页残留未替换的占位符" ;;
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
    *'"source1"'*'"source2"'*) pass "面板页下发了当前上游地址" ;;
    *) fail "面板页未下发当前上游地址" ;;
esac

# ---------------------------------------------------------------------------
# 代理链路（源2 走面板页同源路径）
# ---------------------------------------------------------------------------
note "源2 代理（同源路径 /s）"
curl -s --max-time 5 -D "$WORKDIR/h2.txt" "$PANEL/s?q=abc" >"$WORKDIR/b2.html"
B2="$(cat "$WORKDIR/b2.html")"

grep -qi '^x-frame-options:' "$WORKDIR/h2.txt" \
    && fail "源2 响应仍带 X-Frame-Options" \
    || pass "源2 已剥离 X-Frame-Options"
grep -qi '^content-security-policy:' "$WORKDIR/h2.txt" \
    && fail "源2 响应仍带 Content-Security-Policy" \
    || pass "源2 已剥离 Content-Security-Policy"
case "$B2" in
    *data-local-inject*) pass "源2 已注入隐藏外壳样式" ;;
    *) fail "源2 未注入隐藏外壳样式" ;;
esac
case "$B2" in
    *"MOCK-UPSTREAM-$SRC2_PORT"*) pass "源2 代理链路可达上游" ;;
    *) fail "源2 代理未取到上游内容（实际：$(head -c 200 "$WORKDIR/b2.html")）" ;;
esac
case "$B2" in
    *"$UPSTREAM_HOST"*) fail "源2 正文仍含上游绝对地址" ;;
    *) pass "源2 绝对地址已改写为相对" ;;
esac

# ---------------------------------------------------------------------------
# 代理链路（源1 独立端口）
# ---------------------------------------------------------------------------
note "源1 代理（独立端口 /files）"
curl -s --max-time 5 -D "$WORKDIR/h1.txt" "$PROXY1/files?q=abc" >"$WORKDIR/b1.html"
B1="$(cat "$WORKDIR/b1.html")"

grep -qi '^x-frame-options:' "$WORKDIR/h1.txt" \
    && fail "源1 响应仍带 X-Frame-Options" \
    || pass "源1 已剥离 X-Frame-Options"
grep -qi '^content-security-policy:' "$WORKDIR/h1.txt" \
    && fail "源1 响应仍带 Content-Security-Policy" \
    || pass "源1 已剥离 Content-Security-Policy"
case "$B1" in
    *data-local-inject*) pass "源1 已注入隐藏外壳样式" ;;
    *) fail "源1 未注入隐藏外壳样式" ;;
esac
case "$B1" in
    *"MOCK-UPSTREAM-$SRC1_PORT"*) pass "源1 代理链路可达上游" ;;
    *) fail "源1 代理未取到上游内容（实际：$(head -c 200 "$WORKDIR/b1.html")）" ;;
esac
case "$B1" in
    *"$UPSTREAM_HOST"*) fail "源1 正文仍含上游绝对地址" ;;
    *) pass "源1 绝对地址已改写为相对" ;;
esac

# ---------------------------------------------------------------------------
# 配置接口：读取 / 跨站拒绝 / 校验 / 保存 / 热切换 / 持久化
# 注意放在代理断言之后 —— 它会改掉上游地址。
# ---------------------------------------------------------------------------
note "配置接口"

CFG="$(curl -s --max-time 5 "$PANEL/api/config")"
case "$CFG" in
    *'"source1"'*'"source2"'*) pass "GET /api/config 返回两个上游地址" ;;
    *) fail "GET /api/config 响应不正确（实际：$(head -c 200 <<<"$CFG")）" ;;
esac
case "$CFG" in
    *"$S1_BASE"*) pass "GET /api/config 与启动时的源1 地址一致" ;;
    *) fail "GET /api/config 源1 地址与启动参数不一致（实际：$(head -c 200 <<<"$CFG")）" ;;
esac

CODE="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 -X POST \
    -H 'Content-Type: application/json' -H 'Origin: http://evil.example' \
    -d '{"source1":"http://192.0.2.99:1","source2":"http://192.0.2.99:2"}' \
    "$PANEL/api/config")"
if [ "$CODE" = "403" ]; then
    pass "跨站 Origin 的写入被拒绝（403）"
else
    fail "跨站 Origin 的写入未被拒绝（实际 HTTP $CODE）"
fi

CODE="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 -X POST \
    -H 'Content-Type: application/json' \
    -d '{"source1":"ftp://bad","source2":"http://192.0.2.20:8787"}' \
    "$PANEL/api/config")"
if [ "$CODE" = "400" ]; then
    pass "非法地址被拒绝（400）"
else
    fail "非法地址未被拒绝（实际 HTTP $CODE）"
fi

# 真实保存：把源1 指向另一个假上游端口，验证热切换
CODE="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 -X POST \
    -H 'Content-Type: application/json' \
    -d "{\"source1\":\"$S2_BASE\",\"source2\":\"$S2_BASE\"}" \
    "$PANEL/api/config")"
if [ "$CODE" = "200" ]; then
    pass "合法配置保存成功（200）"
else
    fail "合法配置保存失败（实际 HTTP $CODE）"
fi

curl -s --max-time 5 "$PANEL/healthz" >"$WORKDIR/health2.json"
case "$(cat "$WORKDIR/health2.json")" in
    *"$S2_BASE"*) pass "/healthz 反映出新地址（配置已生效）" ;;
    *) fail "/healthz 未反映新地址（实际：$(head -c 200 "$WORKDIR/health2.json")）" ;;
esac

curl -s --max-time 5 "$PROXY1/files?q=abc" >"$WORKDIR/b1b.html"
case "$(cat "$WORKDIR/b1b.html")" in
    *"MOCK-UPSTREAM-$SRC2_PORT"*) pass "源1 代理已热切换到新上游（无需重启）" ;;
    *) fail "源1 代理未热切换（实际：$(head -c 200 "$WORKDIR/b1b.html")）" ;;
esac

if read_config_file | grep -q "$S2_BASE"; then
    pass "配置已落盘，重启后仍生效（持久化）"
else
    fail "配置文件未落盘或内容不正确"
fi

# ---------------------------------------------------------------------------
# 汇总
# ---------------------------------------------------------------------------
# 断言计数下限：不使用 set -e 的脚本，「半途没跑起来」与「全部通过」退出码都是 0，
# 这条锁专堵那种空转的绿。取实际条数的一半左右并留富余，别贴着实际值。
MIN_CHECKS=20
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
