# WorkBuddy Manager 容器镜像（管理端 + 反代网关）
#
# 设计取舍说明（值得先读，因为它解释了为什么容器版有些功能是"降级"的）：
#
# 1. **本镜像不含上游 workbuddy2api**。上游是独立的 Go 服务、自带
#    docker-compose.yml 与 auths/data 卷，硬塞进来会让两边的升级互相牵制。
#    推荐做法：两边分别用 compose 起，管理端通过 WB2API_BASE 连接上游。
#
# 2. **容器内的一键更新不能自我重启**。更新进程可以下载并替换代码，但容器里
#    没有 systemd、也不能重启自己所在的容器。所以容器形态下的「更新」是：
#    替换代码 → 结束容器 → 由 compose 的 restart 策略用新代码拉起。
#    这需要 compose 里配 `restart: unless-stopped`（本仓库的 compose 已配好）。
#
# 3. **不挂载 docker.sock**。挂了就能用一键更新去重建上游容器，但那等于把宿主
#    root 权限交给容器内进程（可挂载宿主根目录）——比"少一个功能"危险得多。
#    因此容器版不支持「更新上游」，界面会如实提示改用宿主机的 compose 命令。
#
# 4. **数据与凭据全部走卷**，不烘进镜像：data/（数据库、日志、更新状态）、
#    以及上游的 auths/ 与 config.json（只读挂载即可）。
FROM python:3.12-slim

# 环境变量：Python 不要写 pyc（容器是一次性的，写了也没用）、日志不缓冲
ENV PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    WB_RUN_MODE=docker \
    WB_MANAGER_HOST=0.0.0.0 \
    WB_MANAGER_PORT=7864 \
    WB_INSTALL_DIR=/app \
    WB_DATA_DIR=/app/data \
    WB_STATIC_DIR=/app/web/out \
    WB_AUTH_DIR=/opt/workbuddy2api/auths \
    WB_UPSTREAM_CONFIG=/opt/workbuddy2api/config.json \
    WB2API_BASE=http://127.0.0.1:7863

# git：一键更新要 git fetch；curl：健康检查
# openssh-client：发布包验签（ssh-keygen -Y verify 需要 OpenSSH 8.0+）
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        git curl ca-certificates openssh-client \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

# 先装依赖（利用层缓存：代码改动不必重装依赖）
COPY server/requirements.txt /app/server/requirements.txt
RUN pip install --no-cache-dir -r /app/server/requirements.txt

# 再拷代码与已构建的前端
COPY server /app/server
COPY web/out /app/web/out
COPY deploy /app/deploy
COPY CHANGELOG.md README.md /app/

# 非 root 运行。目录归属交给 app 用户，使容器内更新能写回代码目录。
# 注意与上游 auths/data 卷的 uid 对齐：上游容器以 uid 10001 运行，
# 这里用同一 uid 可避免跨容器写同一个卷时的权限问题。
RUN useradd -u 10001 -m -s /bin/bash app \
    && mkdir -p /app/data /opt/workbuddy2api \
    && chown -R 10001:10001 /app /opt/workbuddy2api

USER app

EXPOSE 7864

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD curl -fsS http://127.0.0.1:7864/api/healthz || exit 1

CMD ["python", "-m", "uvicorn", "server.main:app", "--host", "0.0.0.0", "--port", "7864"]
