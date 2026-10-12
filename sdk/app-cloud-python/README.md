# Euler Application Cloud Python SDK / CLI

面向当前独立应用云的项目服务账号。历史 `sdk/python` 的 CPS1/IaaS 客户端不是本 SDK。Python 3.10+，无运行时依赖。

```sh
python -m pip install ./sdk/app-cloud-python
export EULER_URL=https://console.example
export EULER_TENANT_ID=ten_example
export EULER_PROJECT_ID=prj_example
export EULER_SERVICE_TOKEN_FILE=/run/secrets/euler-service-token
euler-cloud databases
euler-cloud quota storage
euler-cloud request statistics GET /sites
```

先在欧拉“服务账号”中创建指定应用和操作权限的账号，将一次性秘密保存在仅自己可读的文件中（0600），通过 `EULER_SERVICE_TOKEN_FILE` 引用。也支持 `EULER_SERVICE_TOKEN` 环境变量；不提供命令行 token 参数，不在日志或 repr 输出秘密。默认只接受 HTTPS，本机开发可显式使用 `--allow-insecure-loopback`。

```python
import os
from pathlib import Path
from euler_cloud import Client, APIError

# 一个客户端绑定一个团队和项目；实际权限由服务端逐请求检查。
client = Client(
    os.environ['EULER_URL'],
    os.environ['EULER_TENANT_ID'],
    os.environ['EULER_PROJECT_ID'],
    Path(os.environ['EULER_SERVICE_TOKEN_FILE']).read_text().strip(),
)
try:
    print(client.databases())
except APIError as error:
    # 将 request_id 交给运营者排查，勿打印服务账号秘密。
    print(error.status, error.code, error.request_id)
```

`request(application, method, path, data=..., query=...)` 返回 `Response`（status、data、request_id、content_type）。JSON 解码为对象，非 JSON 返回 bytes，204 返回 None。单次响应最大 16 MiB，文件大对象使用应用提供的签名下载或分页接口。CLI `--output` 以 0600 创建新文件且拒绝覆盖，JSON body 从 `--data-file` 读取。

机器接口固定在 `/api/v1/machine/tenants/{tenant}/projects/{project}/apps/{app}`，使用 `Authorization: Bearer euler_sa_...`。它不接受浏览器 Cookie，不代替人员认证审核、WitShield 人工修复审批或平台管理员。服务器的路径白名单和权限检查是最终规则；SDK 不尝试绕过 403。

不自动跟随重定向，不自动重试写入。网络超时不能证明写入未发生；先查询结果，再按各业务接口支持的幂等语义处理。服务账号到期、撤销、所属项目归档或应用停用后拒绝新请求。已经发出的数据库凭据或短期文件签名仍受数据面自身生命周期约束。

测试：`PYTHONPATH=sdk/app-cloud-python/src python -m unittest discover -s sdk/app-cloud-python/tests -v`。
