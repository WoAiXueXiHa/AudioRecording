#!/usr/bin/env python3
"""P0 自动验收：隔离数据库、全套 Go 测试、真实 HTTP 进程；不调用付费 API。"""
import json
import os
from pathlib import Path
import secrets
import shutil
import signal
import socket
import subprocess
import sys
import tempfile
import threading
import time
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

ROOT = Path(__file__).resolve().parents[1]
HTTP = urllib.request.build_opener(urllib.request.ProxyHandler({}))


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def main():
    for tool in ('go', 'docker'):
        require(shutil.which(tool), f'缺少命令：{tool}')
    report = Path(tempfile.mkdtemp(prefix='audio-check-report-'))
    print(f'日志目录：{report}', flush=True)
    name = 'audio-check-' + secrets.token_hex(6)
    password = secrets.token_hex(24)
    process = None
    stub = None
    started = False
    results = []
    env = dict(os.environ)
    # 不继承业务数据库、真实模型配置或网络代理。
    for key in list(env):
        if key.lower().endswith('_proxy') or key.startswith(('MYSQL_', 'TEST_MYSQL_', 'DEEPSEEK_')):
            env.pop(key)
    env['GIN_MODE'] = 'release'

    def run(args, label, timeout=180, input_text=None):
        with (report / (label + '.log')).open('a') as log:
            result = subprocess.run(args, cwd=ROOT, env=env, input=input_text,
                                    text=True, stdout=log, stderr=subprocess.STDOUT, timeout=timeout)
        require(result.returncode == 0, f'{label} 失败，见 {report / (label + ".log")}')

    def passed(label):
        results.append(label)
        print('PASS ' + label, flush=True)

    def sql(statement):
        return subprocess.check_output(
            ['docker', 'exec', '-i', '-e', 'MYSQL_PWD=' + password, name,
             'mysql', '-uroot', '-N', '-B'], input=statement, text=True, timeout=15).strip()

    def request(method, path, data=None, content_type=None):
        headers = {'Content-Type': content_type} if content_type else {}
        req = urllib.request.Request(base + path, data=data, headers=headers, method=method)
        try:
            response = HTTP.open(req, timeout=10)
        except urllib.error.HTTPError as exc:
            response = exc
        with response:
            raw = response.read()
            return response.code, json.loads(raw) if raw else None

    def expect(method, path, status, data=None, content_type=None):
        code, body = request(method, path, data, content_type)
        require(code == status, f'{method} {path}: expected {status}, got {code}: {body}')
        return body

    def upload():
        boundary = 'AudioCheckBoundary'
        data = (f'--{boundary}\r\nContent-Disposition: form-data; name="file"; '
                'filename="check.mp3"\r\nContent-Type: audio/mpeg\r\n\r\n').encode()
        data += b'mock-audio-content\r\n' + f'--{boundary}--\r\n'.encode()
        before = time.monotonic()
        body = expect('POST', '/v1/recordings', 201, data,
                      'multipart/form-data; boundary=' + boundary)
        require(body['status'] == 'pending', '上传未返回 pending')
        require(time.monotonic() - before < 5, '上传未及时返回（>=5秒）')
        return body['recording_id'], body['task_id']

    def wait_task(tid, target, timeout=25):
        end = time.monotonic() + timeout
        while time.monotonic() < end:
            task = expect('GET', f'/v1/tasks/{tid}', 200)
            if task['status'] in target:
                return task
            time.sleep(.1)
        raise RuntimeError(f'task {tid} 等待 {target} 超时')

    def start_server():
        nonlocal process
        with (report / 'server.log').open('a') as log:
            process = subprocess.Popen([str(binary)], cwd=ROOT, env=env, stdout=log, stderr=log)
        end = time.monotonic() + 15
        while time.monotonic() < end:
            require(process.poll() is None, '服务提前退出，见 server.log')
            try:
                if request('GET', '/health')[0] == 200:
                    return
            except (OSError, urllib.error.URLError):
                pass
            time.sleep(.1)
        raise RuntimeError('服务启动超时')

    def stop_server(sig=signal.SIGTERM):
        process.send_signal(sig)
        code = process.wait(timeout=20)
        if sig == signal.SIGTERM:
            require(code == 0, f'SIGTERM 退出码错误：{code}')

    class SummaryStub(BaseHTTPRequestHandler):
        def do_POST(self):
            body = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
            require(self.path == '/chat/completions', '摘要请求路径错误')
            require(body['messages'][-1]['content'], '未向摘要客户端传递转写文本')
            content = {'summary': '测试摘要', 'key_points': ['测试要点'], 'todos': []}
            payload = json.dumps({'choices': [{'finish_reason': 'stop', 'message': {
                'content': json.dumps(content, ensure_ascii=False)}}]}).encode()
            self.send_response(200)
            self.send_header('Content-Type', 'application/json')
            self.send_header('Content-Length', str(len(payload)))
            self.end_headers()
            self.wfile.write(payload)

        def log_message(self, *args):
            pass

    try:
        run(['docker', 'info'], 'docker-info', 30)
        run(['docker', 'image', 'inspect', 'mysql:8.4.10'], 'mysql-image', 30)
        # 镜像不存在时显式失败，用户可先 docker pull mysql:8.4.10。
        run(['docker', 'run', '--pull=never', '-d', '--rm', '--name', name,
             '-e', 'MYSQL_ROOT_PASSWORD=' + password, '-e', 'MYSQL_ROOT_HOST=%',
             '-p', '127.0.0.1::3306', 'mysql:8.4.10'], 'mysql-start', 60)
        started = True
        for _ in range(90):
            check = subprocess.run(['docker', 'exec', '-e', 'MYSQL_PWD=' + password, name,
                                    'mysql', '-h127.0.0.1', '--protocol=TCP', '-uroot', '-e', 'SELECT 1'],
                                   stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=5)
            if check.returncode == 0:
                break
            time.sleep(1)
        else:
            raise RuntimeError('隔离 MySQL 未就绪')
        info = json.loads(subprocess.check_output(['docker', 'inspect', name], text=True))[0]
        port = info['NetworkSettings']['Ports']['3306/tcp'][0]['HostPort']
        dsn = f'root:{password}@tcp(127.0.0.1:{port})/'
        env['TEST_MYSQL_DSN'] = dsn
        run(['go', 'vet', './...'], 'vet')
        passed('go vet')
        run(['go', 'test', '-race', '-count=1', '-v', './...'], 'go-tests', 300)
        require('--- SKIP:' not in (report / 'go-tests.log').read_text(), '有测试被跳过，请检查日志')
        passed('全部 Go 测试 + race + 隔离 MySQL（不允许 Skip）')
        run(['go', 'build', '-o', str(report / 'server'), './cmd/server'], 'build')
        binary = report / 'server'
        sql('CREATE DATABASE audio_e2e;\nUSE audio_e2e;\n' + (ROOT / 'migrations/001_init.sql').read_text())
        with socket.socket() as sock:
            sock.bind(('127.0.0.1', 0))
            api_port = sock.getsockname()[1]
        base = f'http://127.0.0.1:{api_port}'
        stub = ThreadingHTTPServer(('127.0.0.1', 0), SummaryStub)
        threading.Thread(target=stub.serve_forever, daemon=True).start()
        with tempfile.TemporaryDirectory(prefix='audio-check-uploads-') as uploads:
            env.update(HTTP_ADDR=f'127.0.0.1:{api_port}', MYSQL_DSN=dsn + 'audio_e2e',
                       UPLOAD_DIR=uploads, DEEPSEEK_API_KEY='test-only',
                       DEEPSEEK_BASE_URL=f'http://127.0.0.1:{stub.server_port}', DEEPSEEK_MODEL='test-model')
            start_server()
            expect('GET', '/health', 200)
            expect('GET', '/', 404)
            for method, path in [('GET', '/v1/tasks/0'), ('GET', '/v1/recordings/invalid'),
                                 ('GET', '/v1/recordings?page=0'), ('POST', '/v1/tasks/x/retry'),
                                 ('DELETE', '/v1/recordings/x')]:
                expect(method, path, 400)
            rid, tid = upload()
            expect('POST', f'/v1/tasks/{tid}/retry', 409)
            expect('DELETE', f'/v1/recordings/{rid}', 409)
            # 生产 Mock 有20%失败率；只对该明确错误做有限重试，不掩盖其他失败。
            for attempt in range(10):
                task = wait_task(tid, {'done', 'failed'})
                if task['status'] == 'done':
                    break
                require(task['error_code'] == 'transcription_failed', f'非预期失败：{task}')
                require(attempt < 9, 'Mock 连续失败10次，请重跑；不记为通过')
                result = expect('POST', f'/v1/tasks/{tid}/retry', 202)
                require(result['task_id'] == tid, '重试未复用任务ID')
            detail = expect('GET', f'/v1/recordings/{rid}', 200)
            require(detail['transcript'] and detail['summary'] == '测试摘要', '结果链路不完整')
            require(detail['key_points'] == ['测试要点'] and detail['todos'] == [], '摘要字段不正确')
            require('storage_path' not in detail, '泄漏内部存储路径')
            page = expect('GET', '/v1/recordings?page=1&page_size=20', 200)
            require(any(item['id'] == rid for item in page['items']), '分页未返回录音')
            passed('真实进程：上传→Mock转写→摘要HTTP替身→结果查询')
            stop_server()
            start_server()
            require(expect('GET', f'/v1/recordings/{rid}', 200) == detail, '重启后结果变化')
            require(len(list(Path(uploads).iterdir())) == 1, '重启后文件缺失')
            expect('POST', f'/v1/tasks/{tid}/retry', 409)
            expect('DELETE', f'/v1/recordings/{rid}', 204)
            expect('GET', f'/v1/tasks/{tid}', 404)
            expect('GET', f'/v1/recordings/{rid}', 404)
            require(not list(Path(uploads).iterdir()), '删除后文件残留')
            passed('重启保留结果、终态重试拒绝、删除及文件清理')
            rid, tid = upload()
            wait_task(tid, {'transcribing'}, 4)
            stop_server(signal.SIGKILL)
            start_server()
            task = expect('GET', f'/v1/tasks/{tid}', 200)
            require(task['status'] == 'failed' and task['error_code'] == 'service_interrupted', '重启未标记中断')
            expect('POST', f'/v1/tasks/{tid}/retry', 202)
            require(expect('GET', f'/v1/tasks/{tid}', 200)['retry_count'] == 1, '重试计数错误')
            wait_task(tid, {'transcribing'}, 4)
            stop_server()
            require(sql(f"USE audio_e2e; SELECT status FROM tasks WHERE id={tid};") == 'failed', 'SIGTERM 未落库失败状态')
            passed('SIGKILL重启收尾、手动重试、SIGTERM取消任务及退出0')
    finally:
        if process is not None and process.poll() is None:
            process.kill()
            process.wait(timeout=10)
        if stub is not None:
            stub.shutdown()
            stub.server_close()
        if started:
            cleanup = subprocess.run(['docker', 'rm', '-f', '-v', name],
                                     stdout=subprocess.DEVNULL, timeout=30)
            require(cleanup.returncode == 0, f'清理失败，请手动执行 docker rm -f -v {name}')
        (report / 'passed.json').write_text(json.dumps(results, ensure_ascii=False, indent=2))
    print(f'ALL PASS（{len(results)}组）。完整用例见 {report / "go-tests.log"}', flush=True)
    print('范围：不验证真实ASR、真实DeepSeek账号/网络、Compose镜像构建、断电及COMMIT响应丢失。')


if __name__ == '__main__':
    try:
        main()
    except KeyboardInterrupt:
        print('测试被中断。', file=sys.stderr)
        sys.exit(130)
    except Exception as exc:
        print(f'FAIL: {exc}', file=sys.stderr)
        sys.exit(1)
