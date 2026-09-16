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
# 3. **挂载 docker.sock 是可选的，默认提供**。挂上它，容器内的管理端就能像
#    宿主机部署那样操作上游容器（重载配置 / 读日志 / 一键更新上游）。
#
#    关于安全性的一次修正：初版这里写的是"挂了等于把宿主 root 交给容器，比
#    少一个功能危险得多"，**这个说法不准确**。事实是——宿主部署时本服务
#    **本来就是以 root 运行的**（systemd 单元无 User=，安装脚本要求 root），
#    而 root 的宿主进程本来就能 `docker run -v /:/host` 拿到宿主文件系统。
#    也就是说，宿主部署的权限**已经等价于**挂 docker.sock，两者并无本质高下。
#    所以挂上它只是让容器版与宿主版能力对齐，而不是引入一个新的风险等级。
#
#    若你的威胁模型要求最小权限，把 compose 里那行卷注掉即可：此时依赖 docker
#    的功能会自动降级为「请到宿主机操作」，界面会如实提示（不会静默失败）。
#
# 4. **数据与凭据全部走卷**，不烘进镜像：data/（数据库、日志、更新状态）、
#    以及上游的 auths/ 与 config.json。
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

# 可选：Debian 软件源镜像（留空 = 官方源 deb.debian.org）。
#
# 为什么需要这个开关：官方源走 Fastly CDN，中国大陆访问实测**频繁 502 且极慢**
# （约 367 kB/s，常在下载某个 .deb 时中断），表现为构建在第 2 步就失败、
# 管理端镜像根本建不出来。换国内镜像即可正常构建：
#
#   docker compose build --build-arg DEBIAN_MIRROR=mirrors.aliyun.com
#
# 也可以直接填进 docker-compose.yml 的 build.args。
#
# 为兼容「没显式指定镜像源」的场景，这里**带一次自动降级**：官方源失败时
# 自动改用阿里云镜像重试；两者都失败才报错，并把原因写清楚。
ARG DEBIAN_MIRROR=""

# git：一键更新要 git fetch；curl：健康检查与容器健康探针
# openssh-client：发布包验签（ssh-keygen -Y verify 需要 OpenSSH 8.0+）
# docker-cli：让「保存设置后重载上游」「上游日志」「更新上游」在本容器内可用
#   （需要挂 /var/run/docker.sock，见 docker-compose.yml；不挂则这几项自动降级
#    为"请到宿主机操作"，界面会如实提示，不会静默失败）
#   注意只装 CLI（~50MB），不装 dockerd —— 我们只要控制宿主上的 docker。
RUN set -eu; \
    # 把配置里所有「协议://主机」的主机部分统一换成目标镜像源。
    # 注意不能只 sed 掉 deb.debian.org：降级时源里已经没有那个字符串了，
    # 只认官方主机名会让第二次替换变成空操作（这一点是靠实际测试发现的）。
    # Debian 12+ 用 deb822 格式的 debian.sources，老版本用 sources.list，两处都要覆盖。
    set_mirror() { \
        for f in /etc/apt/sources.list /etc/apt/sources.list.d/debian.sources; do \
            if [ -f "$f" ]; then \
                sed -i -E "s#(https?://)[^/ ]+#\1$1#g" "$f"; \
            fi; \
        done; \
    }; \
    install_pkgs() { \
        # 调小重试与超时：官方源不通时 apt 默认会反复重试，实测要空等 ~200 秒
        # 才判定失败。调小可减少无谓等待，但**主要瓶颈是带宽**（官方源实测
        # 367 kB/s，光索引就有 13 MB），所以真想快还是要显式指定镜像源。 \
        apt-get -o Acquire::Retries=1 -o Acquire::http::Timeout=15 -o Acquire::https::Timeout=15 update \
        && apt-get install -y --no-install-recommends \
            git curl ca-certificates openssh-client; \
    }; \
    if [ -n "${DEBIAN_MIRROR}" ]; then \
        echo "apt: 使用指定软件源 ${DEBIAN_MIRROR}"; \
        set_mirror "${DEBIAN_MIRROR}"; \
    fi; \
    if install_pkgs; then \
        echo "apt: 依赖安装成功"; \
    else \
        echo "apt: 当前软件源失败，改用 mirrors.aliyun.com 重试…" >&2; \
        rm -rf /var/lib/apt/lists/*; \
        set_mirror mirrors.aliyun.com; \
        install_pkgs; \
        echo "apt: 改用国内镜像后安装成功（如需固定，构建时传 DEBIAN_MIRROR 或写入 compose build.args）"; \
    fi; \
    rm -rf /var/lib/apt/lists/*
# docker-cli 走官方静态包（Debian 仓库里的 docker.io 会拖进 dockerd，太重）
ARG DOCKER_CLI_VERSION=27.3.1
RUN curl -fsSL --retry 5 --retry-delay 2 --retry-all-errors \
        "https://download.docker.com/linux/static/stable/x86_64/docker-${DOCKER_CLI_VERSION}.tgz" \
        -o /tmp/docker.tgz \
    && tar -xzf /tmp/docker.tgz -C /tmp \
    && mv /tmp/docker/docker /usr/local/bin/docker \
    && chmod +x /usr/local/bin/docker \
    && rm -rf /tmp/docker /tmp/docker.tgz \
    && docker --version

WORKDIR /app

# 先装依赖（利用层缓存：代码改动不必重装依赖）
#
# 可选：PyPI 镜像。与上面的 DEBIAN_MIRROR 同一个成因——官方 PyPI 走国际 CDN，
# 中国大陆访问实测会出现 `SSL: UNEXPECTED_EOF_WHILE_READING` 并在重试几次后才通，
# 赶上抖动就直接构建失败。留空 = 官方源（失败时自动降级到清华镜像）。
#
#   docker compose build --build-arg PIP_INDEX_URL=https://pypi.tuna.tsinghua.edu.cn/simple
ARG PIP_INDEX_URL=""
COPY server/requirements.txt /app/server/requirements.txt
RUN set -eu; \
    pip_install() { \
        pip install --no-cache-dir --retries 5 --timeout 60 \
            --index-url "$1" -r /app/server/requirements.txt; \
    }; \
    if [ -n "${PIP_INDEX_URL}" ]; then \
        echo "pip: 使用指定镜像源 ${PIP_INDEX_URL}"; \
        pip_install "${PIP_INDEX_URL}"; \
    elif pip_install https://pypi.org/simple; then \
        echo "pip: 依赖安装成功（官方源）"; \
    else \
        echo "pip: 官方源失败，改用清华镜像重试…" >&2; \
        pip_install https://pypi.tuna.tsinghua.edu.cn/simple; \
        echo "pip: 改用镜像后安装成功（如需固定，构建时传 PIP_INDEX_URL 或写入 compose build.args）"; \
    fi

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
