<script setup lang="ts">
/** 开发者社区 (M-10.3, 09§5.2 M-10): 文档 / 示例 / 社区入口.
 *
 *  This view is a static surface — it links out to the SDKs and shows the one
 *  thing every integration needs (a signed CPS1 call), but it never computes
 *  anything. Real signing stays in pkg-go/cps1 (single implementation); the
 *  snippets below mirror the SDK entry points, not a second algorithm.
 */
import { PageHeader } from "@eu/ui";

const goSnippet = `// pkg-go/eusdk — 一次签名调用 (单一实现: cps1)
client := eusdk.New(eusdk.Config{
    AK:     os.Getenv("EULER_ACCESS_KEY"),
    SK:     os.Getenv("EULER_SECRET_KEY"),
    Region: "cn-north-1",
})
resp, err := client.Call(eusdk.ApiRequest{
    ProductCode: "euecs",
    Method:      "POST",
    Query:       url.Values{"Action": {"RunInstances"}, "Version": {"2026-08-01"}},
    Body:        []byte(\`{"ImageId":"img-001","InstanceType":"s2.large"}\`),
})
// resp.StatusCode / resp.Body — 签名由 SDK 用与网关同一份 cps1 计算`;

const pySnippet = `# sdk/python/cloudsdk — 独立实现, golden-vector 回归锁定一致
from cloudsdk import Client, Config, ApiRequest

client = Client(Config(
    ak="EU...", sk="...", region="cn-north-1",
))
resp = client.call(ApiRequest(
    product_code="euecs", method="POST",
    query={"Action": ["RunInstances"], "Version": ["2026-08-01"]},
    body=b'{"ImageId":"img-001","InstanceType":"s2.large"}',
))
# 签名与 Go 完全一致: sdk/python/tests/test_golden_vectors.py 断言 7 个向量`;

const tfSnippet = `# sdk/terraform — IaC 消费者 (M-10.2, source-only)
provider "euler" {
  region   = "cn-north-1"           # 凭证读 EULER_ACCESS_KEY / EULER_SECRET_KEY
}
resource "euler_euecs_instance" "web" {
  instance_type = "s2.large"
  image_id      = "img-001"
}`;

const docs = [
  { title: "Go SDK", path: "pkg-go/eusdk", note: "与网关共用 cps1 签名,零漂移" },
  { title: "Python SDK", path: "sdk/python/cloudsdk", note: "独立实现,golden-vector 锁定与 Go 一致" },
  { title: "Terraform Provider", path: "sdk/terraform", note: "覆盖 EUECS/EUOSS/EUVPC/EURDS,source-only" },
  { title: "API 版本化规范", path: "proto-hub/VERSIONING.md", note: "additive-only / 弃用窗口" },
  { title: "IDL 事实源", path: "proto-hub/proto/euler", note: "18 包,单一 IDL 源" },
  { title: "CPS1 签名说明", path: "docs/cps1-implementation-notes.md", note: "编码/校验顺序契约" },
];
</script>

<template>
  <div class="community">
    <PageHeader title="开发者社区" subtitle="接入平台的文档、示例与社区入口。示例只展示 SDK 入口,签名仍由 pkg-go/cps1 单一实现计算,杜绝算法漂移。" />

    <section class="community-card">
      <h3>文档入口</h3>
      <ul class="doc-list">
        <li v-for="d in docs" :key="d.title" class="doc-row">
          <span class="doc-title">{{ d.title }}</span>
          <code class="doc-path">{{ d.path }}</code>
          <span class="doc-note">{{ d.note }}</span>
        </li>
      </ul>
    </section>

    <section class="community-card">
      <h3>接入示例</h3>

      <div class="sample-block">
        <div class="sample-label">Go · eusdk 签名调用</div>
        <pre class="code-block">{{ goSnippet }}</pre>
      </div>

      <div class="sample-block">
        <div class="sample-label">Python · cloudsdk 签名调用</div>
        <pre class="code-block">{{ pySnippet }}</pre>
      </div>

      <div class="sample-block">
        <div class="sample-label">Terraform · 资源声明</div>
        <pre class="code-block">{{ tfSnippet }}</pre>
      </div>
    </section>

    <section class="community-card">
      <h3>社区入口</h3>
      <ul class="link-list">
        <li><a href="https://github.com/qifalab/euler-platform" target="_blank" rel="noopener">GitHub 仓库 (qifalab/euler-platform)</a> — 源码 / issues / PR</li>
        <li><span class="muted">论坛 / 开发者门户 — 待对客上线后接入 (09§5.1 目标 3 运营面)</span></li>
      </ul>
      <p class="community-note">上架第三方镜像/应用走云市场闭环 (svc-marketplace, M-10.1),非本入口。</p>
    </section>
  </div>
</template>

<style scoped>
.community { padding: var(--eu-spacing-6); max-width: 1100px; }
.community-card {
  background: var(--eu-glass-bg-soft);
  -webkit-backdrop-filter: var(--eu-glass-blur-soft);
  backdrop-filter: var(--eu-glass-blur-soft);
  border: 1px solid var(--eu-glass-border);
  border-radius: var(--eu-radius-lg);
  box-shadow: var(--eu-shadow-sm);
  padding: var(--eu-spacing-5);
  margin-bottom: var(--eu-spacing-4);
}
.community h3 { font-size: 15px; color: var(--eu-text-primary); margin: 0 0 var(--eu-spacing-3); }
.doc-list, .link-list { list-style: none; margin: 0; padding: 0; display: flex; flex-direction: column; gap: var(--eu-spacing-2); }
.doc-row { display: grid; grid-template-columns: 160px 1fr auto; gap: var(--eu-spacing-3); align-items: baseline; font-size: 13px; }
.doc-title { color: var(--eu-text-primary); font-weight: 500; }
.doc-path { font-family: monospace; font-size: 12px; color: var(--eu-color-brand); background: var(--eu-bg-container); border: 1px solid var(--eu-border); border-radius: var(--eu-radius-sm); padding: 2px 8px; }
.doc-note { color: var(--eu-text-secondary); font-size: 12px; }
.sample-block { margin-bottom: var(--eu-spacing-4); }
.sample-label { font-size: 12px; color: var(--eu-text-secondary); margin-bottom: 4px; }
.code-block { background: var(--eu-bg-container); border: 1px solid var(--eu-border); border-radius: var(--eu-radius-sm); padding: var(--eu-spacing-3); font-size: 12px; font-family: monospace; color: var(--eu-text-primary); white-space: pre-wrap; word-break: break-all; overflow-x: auto; }
.link-list a { color: var(--eu-color-brand); text-decoration: none; font-size: 13px; }
.link-list a:hover { color: var(--eu-color-brand-hover); }
.link-list .muted, .community-note { color: var(--eu-text-secondary); font-size: 12px; }
.community-note { margin: var(--eu-spacing-3) 0 0; }
</style>
