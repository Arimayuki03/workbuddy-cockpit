"""WorkBuddy Manager 入口：管理 API + 对外反代网关 + 静态前端托管。"""
from __future__ import annotations

from contextlib import asynccontextmanager

from fastapi import Depends, FastAPI, Request
from fastapi.middleware.cors import CORSMiddleware
from fastapi.responses import FileResponse, JSONResponse
from fastapi.staticfiles import StaticFiles

from . import config, db, security
from .iputil import client_ip
from .routers import (
    accounts, auth, gateway, keys, logs,
    security as security_router, settings, stats, system,
)
from .services import tasklog


@asynccontextmanager
async def lifespan(app: FastAPI):
    config.ensure_dirs()
    db.connect()
    security.load_users()  # 首次启动会自动生成管理员并打印一次密码
    # 后台采集上游自动任务日志（旅行/活跃/签到/保活），容器日志会被重建清掉，
    # 这里解析后落库长期保留，界面才能看到「这趟旅行领了多少积分」
    tasklog.start_collector()
    try:
        yield
    finally:
        tasklog.stop_collector()


app = FastAPI(
    title='WorkBuddy Manager',
    version='1.0.18',
    lifespan=lifespan,
    # 生产环境默认关闭交互式文档与 OpenAPI 描述：
    # 它们会把管理接口全貌（路径、参数、结构）暴露给任何未认证访问者，
    # 便于攻击者摸清面。需要时设 WB_ENABLE_DOCS=1 打开。
    docs_url='/docs' if config.ENABLE_DOCS else None,
    redoc_url='/redoc' if config.ENABLE_DOCS else None,
    openapi_url='/openapi.json' if config.ENABLE_DOCS else None,
)

if config.CORS_ORIGINS:
    app.add_middleware(
        CORSMiddleware,
        allow_origins=config.CORS_ORIGINS,
        allow_credentials=True,
        allow_methods=['*'],
        allow_headers=['*'],
    )

# ── 路由注册顺序很重要：先 API / 网关，最后挂静态文件 ──
app.include_router(auth.router)
app.include_router(accounts.router)
app.include_router(keys.router)
app.include_router(logs.router)
app.include_router(stats.router)
app.include_router(security_router.router)
app.include_router(settings.router)
app.include_router(system.router)
app.include_router(gateway.router)


@app.middleware('http')
async def cache_headers(request: Request, call_next):
    """按内容性质设置缓存策略与安全响应头。

    - /_next/static/**：文件名含内容哈希，可长期强缓存（immutable）
    - /api/**、/v1/**：动态数据，禁止任何缓存（含浏览器与中间代理）
    - 其余（HTML 文档）：no-cache，即每次回源校验 ETag，避免拿到旧页面

    安全头说明：
    - X-Content-Type-Options: 阻止浏览器嗅探类型（防内容被当作脚本执行）
    - X-Frame-Options / frame-ancestors: 禁止被其他站点内嵌（防点击劫持）
    - Referrer-Policy: 跨站请求不带完整 URL（避免泄露路径）
    - CSP: 只允许同源资源与内联样式（前端使用内联样式属性）；
      限制外联目标，降低 XSS 得手后的影响面
    """
    response = await call_next(request)
    path = request.url.path
    if path.startswith('/_next/static/'):
        # 文件名含内容哈希，内容变了文件名就变，可长期强缓存
        response.headers['Cache-Control'] = 'public, max-age=31536000, immutable'
    elif path.startswith(('/api/', '/v1/', '/v2/', '/healthz')):
        response.headers['Cache-Control'] = 'no-store, no-cache, must-revalidate'
        response.headers['Pragma'] = 'no-cache'
    elif path.startswith('/favicon/') or path.endswith(('.png', '.ico', '.svg', '.woff2', '.webmanifest')):
        # 图标 / 字体等静态资源，内容基本不变，缓存一天
        response.headers['Cache-Control'] = 'public, max-age=86400'
    else:
        response.headers['Cache-Control'] = 'no-cache'

    response.headers.setdefault('X-Content-Type-Options', 'nosniff')
    response.headers.setdefault('X-Frame-Options', 'DENY')
    response.headers.setdefault('Referrer-Policy', 'no-referrer')
    response.headers.setdefault('Permissions-Policy', 'geolocation=(), microphone=(), camera=()')
    response.headers.setdefault(
        'Content-Security-Policy',
        "default-src 'self'; "
        "img-src 'self' data: blob:; "
        "font-src 'self' data:; "
        "style-src 'self' 'unsafe-inline'; "
        "script-src 'self' 'unsafe-inline'; "
        "connect-src 'self'; "
        "frame-ancestors 'none'; "
        "base-uri 'self'; "
        "form-action 'self'",
    )
    return response


@app.get('/api/sysinfo')
def sysinfo(user: dict = Depends(security.current_user)) -> dict:
    """服务信息。需要登录 —— 路径类信息不应对未认证访问者暴露。"""
    return {
        'service': 'workbuddy-manager',
        'version': app.version,
    }


# ── 静态前端（Next.js 静态导出）────────────────────────
if config.STATIC_DIR.is_dir():
    app.mount('/_next', StaticFiles(directory=str(config.STATIC_DIR / '_next')), name='next-assets')
    if (config.STATIC_DIR / 'favicon.ico').exists():
        @app.get('/favicon.ico', include_in_schema=False)
        def favicon() -> FileResponse:
            return FileResponse(config.STATIC_DIR / 'favicon.ico')

    @app.get('/', include_in_schema=False)
    def index() -> FileResponse:
        return FileResponse(config.STATIC_DIR / 'index.html')

    @app.get('/{full_path:path}', include_in_schema=False)
    def spa(full_path: str):
        # 优先命中导出的静态页面 / 资源，否则回退到 404 页面
        candidate = config.STATIC_DIR / full_path
        if candidate.is_file():
            return FileResponse(candidate)
        index_candidate = config.STATIC_DIR / full_path / 'index.html'
        if index_candidate.is_file():
            return FileResponse(index_candidate)
        not_found = config.STATIC_DIR / '404.html'
        if not_found.is_file():
            return FileResponse(not_found, status_code=404)
        return JSONResponse({'error': 'not found'}, status_code=404)
