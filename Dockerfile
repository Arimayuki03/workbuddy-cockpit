# syntax=docker/dockerfile:1

# 三段构建（v1.2.0，设计文档 §7）：
#   1) node:22       前端面板静态导出（npm ci && npm run build:export → web/out）
#   2) golang:1.26   go:embed 内嵌前端产物（-tags embed_panel）+ 全部 CLI 二进制
#   3) alpine:3.20   运行时（python3/bash/时区，与原镜像一致）

# ---------- 1. 前端静态导出 ----------
FROM node:22-alpine AS frontend
WORKDIR /web
# 先拷 manifest 单独 npm ci：依赖未变时利用层缓存
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
# NEXT_OUTPUT_EXPORT=1 → output:'export' + trailingSlash，产物落 /web/out
RUN npm run build:export

# ---------- 2. Go 编译（内嵌面板） ----------
FROM golang:1.26-alpine AS build
# 版本注入：CI 打 tag 构建时传 --build-arg VERSION=vX.Y.Z；本地/无 tag 构建回落 dev。
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# 静态产物就位（embed 目录：internal/panel/dist）后再编译，-tags embed_panel 启用内嵌。
COPY --from=frontend /web/out/ ./internal/panel/dist/
RUN CGO_ENABLED=0 go build -trimpath -tags embed_panel \
      -ldflags="-s -w -X workbuddy2api/internal/server.appVersion=${VERSION}" \
      -o /out/wb2api ./cmd/server \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/signin_bin ./cmd/signin \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/login ./cmd/login \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/credit ./cmd/credit \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/trial_bin ./cmd/trial \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/activity_bin ./cmd/activity

# ---------- 3. 运行时 ----------
FROM alpine:3.20
# python3：login.sh 的 JSON 解析 / 签到 / 落盘；bash：shell 脚本。
RUN apk add --no-cache wget ca-certificates tzdata python3 bash \
 && adduser -D -u 10001 app \
 && mkdir -p /app/auths /app/data \
 && chown -R app:app /app
WORKDIR /app
# 脚本置入 + 去 CRLF（Windows 检出可能性）在切到 app 之前以 root 完成——
# app 对 root 所有文件无写权限，sed -i 需要写权限。
COPY --from=build /out/wb2api /app/wb2api
COPY --from=build /out/signin_bin /app/signin_bin
COPY --from=build /out/login /app/login
COPY --from=build /out/credit /app/credit
COPY --from=build /out/trial_bin /app/trial_bin
COPY --from=build /out/activity_bin /app/activity_bin
COPY login.sh signin.sh credit.sh trial.sh /app/
# 国际版注册地区自动完善模块（login.sh global 分支 import；scripts/ 无测试/缓存）
COPY scripts/global_region.py /app/scripts/global_region.py
COPY scripts/task_common.py /app/scripts/task_common.py
COPY scripts/task_runner.py /app/scripts/task_runner.py
COPY scripts/school_open_day_2026.py /app/scripts/school_open_day_2026.py
RUN sed -i 's/\r$//' /app/*.sh && chmod 755 /app/*.sh
RUN sed -i 's/\r$//' /app/scripts/*.py && chmod 755 /app/scripts/*.py
# 镜像不带真实配置：落 example 作为默认（生产由挂载卷 /app/config.json 覆盖）
COPY config.example.json /app/config.json
USER app
EXPOSE 7863
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s \
  CMD wget -qO- http://127.0.0.1:7863/healthz || exit 1
ENTRYPOINT ["/app/wb2api", "-config", "/app/config.json"]
