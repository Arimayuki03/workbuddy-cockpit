"""模型中心：展示账号实际可用的模型及其细节。

与「上游配置 → 可用模型」的区别：那里只是上游 /v1/models 的简表；这里直连
腾讯模型接口，能给出**显示名、真实上下文、最大输出、推理档位**，并支持搜索、
按能力筛选与按系列分组。

只读接口。来源与缓存状态如实返回，前端据此标注（不把回退数据说成实时数据）。
"""
from __future__ import annotations

from fastapi import APIRouter, Depends

from .. import security
from ..services import modelcatalog

router = APIRouter(prefix='/api', tags=['models'])


@router.get('/model-catalog')
async def model_catalog(
    force: bool = False,
    user: dict = Depends(security.current_user),
) -> dict:
    """模型清单 + 统计。force=true 绕过 5 分钟缓存（对应界面「重新拉取」）。"""
    data = await modelcatalog.catalog(force=force)
    models = data.get('models') or []
    return {
        **data,
        'summary': modelcatalog.summarize(models),
    }
