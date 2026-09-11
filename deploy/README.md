# 单实例服务器部署（SSH 隧道）

本地执行 `bash scripts/package-deploy.sh` 生成 `/tmp/audio-deploy-XXXXXXXX.tar.gz`。包含 API 与 MySQL 镜像、迁移、启动脚本，无密钥或音频。需要本地 Docker 和 mysql:8.4.10 镜像。

从本地 WSL 终端执行，替换真实包名，密码仅在终端输入：

```bash
scp /tmp/audio-deploy-XXXXXXXX.tar.gz vect@192.144.168.226:~/
ssh vect@192.144.168.226
```

服务器中先确认无同名部署、8080未占用：

```bash
docker ps -a --filter label=com.docker.compose.project=audio-recording
ss -lnt | grep ':8080 ' || true
```

首次部署执行；`mkdir` 若提示目录已存在则停止，不覆盖已有部署：

```bash
mkdir ~/audio-recording
tar -xzf ~/audio-deploy-XXXXXXXX.tar.gz -C ~/audio-recording
cd ~/audio-recording
bash start.sh
```

脚本询问真实 DeepSeek 密钥，生成随机数据库密码并保存到权限600的 `.env`；不要发送或提交此文件。重复启动复用密码，不重新初始化已有卷。

在 Windows CMD 建立隧道并保持窗口开启：

```bash
ssh -N -L 18080:127.0.0.1:8080 vect@192.144.168.226
```

Apifox 地址设为 `http://127.0.0.1:18080`。先检查健康，再上传、查询至done、测试删除；Mock可能失败，可手动重试。默认最多3个任务同时转写/摘要。真实语音识别仍未接入。

服务器验证：

```bash
cd ~/audio-recording
docker compose -p audio-recording logs --tail=100 api
docker stats --no-stream
docker compose -p audio-recording restart
```

等待健康恢复，再查询之前保留的录音，确认结果和文件持久化。上传有摘要会产生真实API费用。若失败，保留日志和数据卷排查；停止用 `docker compose -p audio-recording stop`，不要执行 `down -v`。首次部署尚无旧镜像可回滚；后续更新应先备份数据、保留旧镜像再替换。
