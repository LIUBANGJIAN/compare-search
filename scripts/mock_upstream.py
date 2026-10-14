#!/usr/bin/env python3
"""冒烟测试用的假上游。

同时监听源1 / 源2 端口，返回带「禁止 iframe 内嵌」响应头、且正文里含
上游绝对地址的 HTML —— 用来验证本地代理确实剥掉了安全头、注入了隐藏样式，
并把绝对地址改写成了相对路径。

上游主机名通过环境变量 MOCK_UPSTREAM_HOST 传入，必须与被测服务的
SRC1_BASE / SRC2_BASE 保持一致，否则「绝对地址改写」这条断言不成立。
"""
import os
import sys
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

BODY_TEMPLATE = (
    "<!DOCTYPE html><html><head><title>mock-upstream</title></head><body>"
    "<p>MOCK-UPSTREAM-{port}</p>"
    '<a href="http://{host}:{port}/files?q=x">absolute-link</a>'
    '<script>var api = "http://{host}:{port}/api";</script>'
    "</body></html>"
)


class Handler(BaseHTTPRequestHandler):
    server_version = "mock-upstream"

    def do_GET(self):  # noqa: N802 (BaseHTTPRequestHandler 的命名约定)
        port = self.server.server_address[1]
        body = BODY_TEMPLATE.format(port=port, host=self.server.upstream_host).encode("utf-8")
        self.send_response(200)
        self.send_header("Content-Type", "text/html; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        # 这两个头是「能不能被 iframe 内嵌」的关键，代理必须剥掉
        self.send_header("X-Frame-Options", "DENY")
        self.send_header("Content-Security-Policy", "frame-ancestors 'none'")
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, fmt, *args):
        pass


def serve(port, upstream_host):
    httpd = ThreadingHTTPServer(("0.0.0.0", port), Handler)
    httpd.upstream_host = upstream_host
    httpd.serve_forever()


def main():
    ports = [int(p) for p in sys.argv[1:]] or [11510, 8787]
    upstream_host = os.environ.get("MOCK_UPSTREAM_HOST", "host.docker.internal")
    for p in ports[1:]:
        threading.Thread(target=serve, args=(p, upstream_host), daemon=True).start()
    sys.stderr.write("mock upstream listening on %s (host=%s)\n" % (ports, upstream_host))
    serve(ports[0], upstream_host)


if __name__ == "__main__":
    main()
