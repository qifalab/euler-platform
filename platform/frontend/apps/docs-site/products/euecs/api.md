# EUECS API 参考

本接口参考由 OpenAPI 规范自动生成(02§8.2),与后端 API 规范同源。

## 实例管理

### DescribeInstances — 查询实例列表

查询指定地域下的云服务器实例。

```http
GET /api/v1/euecs/DescribeInstances?RegionId=cn-north-1
Authorization: Bearer <access_token>
```

**响应**

```json
{
  "RequestId": "9f8a...",
  "Code": "OK",
  "Data": [
    {
      "InstanceId": "euecs-cn-north-1-01-a1b2c3d4",
      "InstanceName": "web-prod-1",
      "Status": "Running",
      "SpecCode": "euecs.s2.large",
      "ZoneId": "cn-north-1a"
    }
  ]
}
```

### RunInstances — 创建实例

```http
POST /api/v1/euecs/RunInstances
```

### StartInstance / StopInstance / ReleaseInstance — 实例生命周期

```http
POST /api/v1/euecs/StartInstance?InstanceId=euecs-...
```

## 鉴权

所有请求经 APISIX 网关,携带 `Authorization: Bearer <access_token>`。签名算法参见 CPS1-HMAC-SHA256(07§4.1)。
