"""WorkBuddy Manager 入口：管理 API + 对外反代网关 + 静态前端托管。"""
from __future__ import annotations

from contextlib import asynccontextmanager

from fastapi import FastAPI, Request
from fastapi.middleware.cors import CORSMiddleware
from fastapi.responses import FileResponse, JSONResponse
from fastapi.staticfiles import StaticFiles

from . import config, db, security
from .iputil import client_ip
from .routers import accounts, auth, gateway, keys, logs, security as security_router, settings, stats


@asynccontextmanager
async def lifespan(app: FastAPI):
    config.ensure_dirs()
    db.connect()
    security.load_users()  # 首次启动会自动生成管理员并打印一次密码
    yield


app = FastAPI(title='WorkBuddy Manager', version='1.0.0', lifespan=lifespan)

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
app.include_router(gateway.router)


@app.middleware('http')
async def cache_headers(request: Request, call_next):
    """按内容性质设置缓存策略。

    - /_next/static/**：文件名含内容哈希，可长期强缓存（immutable）
    - /api/**、/v1/**：动态数据，禁止任何缓存（含浏览器与中间代理）
    - 其余（HTML 文档）：no-cache，即每次回源校验 ETag，避免拿到旧页面
    """
    response = await call_next(request)
    path = request.url.path
    if path.startswith('/_next/static/'):
        response.headers['Cache-Control'] = 'public, max-age=31536000, immutable'
    elif path.startswith(('/api/', '/v1/', '/v2/', '/healthz')):
        response.headers['Cache-Control'] = 'no-store, no-cache, must-revalidate'
        response.headers['Pragma'] = 'no-cache'
    else:
        response.headers['Cache-Control'] = 'no-cache'
    return response


@app.get('/api/sysinfo')
def sysinfo() -> dict:
    return {
        'service': 'workbuddy-manager',
        'version': app.version,
        'upstream_base': config.WB2API_BASE,
        'auth_dir': str(config.AUTH_DIR),
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
