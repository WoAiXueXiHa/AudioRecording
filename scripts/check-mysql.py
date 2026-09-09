#!/usr/bin/env python3
"""使用一次性 MySQL 容器运行映射、上传与查询测试；不连接本机业务数据库。"""
import json
import os
from pathlib import Path
import secrets
import subprocess
import time

root = Path(__file__).resolve().parents[1]
name = "audio-mapping-" + secrets.token_hex(6)
password = secrets.token_hex(24)
started = False
try:
    # 使用本地已有镜像；缺少镜像时明确失败，不隐式下载。
    subprocess.run([
        "docker", "run", "--pull=never", "--detach", "--rm", "--name", name,
        "-e", "MYSQL_ROOT_PASSWORD=" + password, "-e", "MYSQL_ROOT_HOST=%",
        "-p", "127.0.0.1::3306", "mysql:8.4.10",
    ], check=True, stdout=subprocess.DEVNULL)
    started = True
    print("等待隔离 MySQL 就绪……", flush=True)
    for _ in range(60):
        result = subprocess.run([
            "docker", "exec", "-e", "MYSQL_PWD=" + password, name,
            "mysql", "-uroot", "-N", "-e", "SELECT 1",
        ], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=5)
        if result.returncode == 0:
            break
        time.sleep(1)
    else:
        raise RuntimeError("MySQL 未在预期时间内就绪")
    info = json.loads(subprocess.check_output(["docker", "inspect", name], text=True))[0]
    port = info["NetworkSettings"]["Ports"]["3306/tcp"][0]["HostPort"]
    env = dict(os.environ, TEST_MYSQL_DSN=f"root:{password}@tcp(127.0.0.1:{port})/")
    result = subprocess.run([
        "go", "test", "-v", "./internal/database", "-run", "TestMySQLMapping", "-count=1",
    ], cwd=root, env=env, timeout=120)
    raise SystemExit(result.returncode)
finally:
    if started:
        subprocess.run(["docker", "rm", "-f", "-v", name], check=True, stdout=subprocess.DEVNULL)
        print("已清理本次测试容器及其匿名数据卷。", flush=True)
