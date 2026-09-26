<script setup lang="ts">
import { computed } from "vue";

const config = useRuntimeConfig();
const repositoryUrl = "https://github.com/qifalab/euler-platform";
const deploymentUrl = `${repositoryUrl}/blob/master/deploy/app-cloud/README.md`;
const capabilitiesUrl = `${repositoryUrl}/blob/master/docs/unified-app-cloud/NATIVE-APPS.md`;

// A console is an operator-configured deployment, never an invented hosted offer.
const consoleUrl = computed(() => {
  const value = String(config.public.consoleUrl || "").trim();
  if (value.startsWith("/") && !value.startsWith("//") && !value.includes("\\"))
    return value;
  try {
    const url = new URL(value);
    const local = ["localhost", "127.0.0.1", "[::1]"].includes(url.hostname);
    if (url.username || url.password) return "";
    return url.protocol === "https:" || (url.protocol === "http:" && local)
      ? url.href
      : "";
  } catch {
    return "";
  }
});
const primaryUrl = computed(() => consoleUrl.value || deploymentUrl);
const primaryLabel = computed(() =>
  consoleUrl.value ? "进入应用控制台" : "部署应用云",
);

// Capability descriptions follow docs/unified-app-cloud/NATIVE-APPS.md. These cards
// describe the product catalogue, not provisioned resources or live telemetry.
const products = [
  {
    id: "eid",
    name: "EID",
    label: "成员资格",
    category: "身份与信任",
    color: "#6157cb",
    symbol: "ID",
    description: "申请成员资格、查看身份卡，完成社团报名与录取。",
    capability: "资格申请 · 审核 · 社团报名",
    repository: "https://github.com/miaojilab/emoera-eid",
  },
  {
    id: "trust",
    name: "Trust",
    label: "信任中心",
    category: "身份与信任",
    color: "#16776a",
    symbol: "✓",
    description: "创建认证方案、提交材料、审核申请，与成员资格联动。",
    capability: "动态方案 · 材料 · 审核",
    repository: "https://github.com/miaojilab/trust-center",
  },
  {
    id: "weauth",
    name: "WeAuth",
    label: "人机验证",
    category: "开发服务",
    color: "#864cb5",
    symbol: "W",
    description: "配置验证站点、域名与风险策略，将人机验证嵌入业务。",
    capability: "工作量证明 · 域名 · IP 风控",
    repository: "https://github.com/ctipscn/weauth",
  },
  {
    id: "database",
    name: "ECloud Database",
    label: "云数据库",
    category: "开发服务",
    color: "#4266b8",
    symbol: "DB",
    description: "创建 MySQL 与 PostgreSQL 数据库，管理连接、用量和配额。",
    capability: "数据库创建 · 凭据 · 资源包",
    repository: "https://github.com/ctipscn/ecloud-database",
  },
  {
    id: "storage",
    name: "ECloud Storage",
    label: "对象存储",
    category: "开发服务",
    color: "#137f98",
    symbol: "S3",
    description: "管理存储桶与文件，通过签名地址上传下载并分配访问密钥。",
    capability: "文件管理 · 访问策略 · 配额",
    repository: "https://github.com/ctipscn/ecloud-storage",
  },
  {
    id: "statistics",
    name: "Statistics",
    label: "网站统计",
    category: "应用工具",
    color: "#aa7025",
    symbol: "ST",
    description: "采集网站访问，查看页面 PV、UV 与排行，嵌入访问计数。",
    capability: "采集脚本 · PV / UV · 页面排行",
    repository: "https://github.com/ctipscn/ecloud-statistics",
  },
  {
    id: "lottery",
    name: "Lottery",
    label: "活动抽奖",
    category: "应用工具",
    color: "#bb536a",
    symbol: "LT",
    description: "创建活动、扫码报名、现场抽奖，保存参与者和开奖历史。",
    capability: "活动房间 · 报名 · 抽奖",
    repository: "https://github.com/miaojilab/emoera-lottery-system",
  },
  {
    id: "witshield",
    name: "WitShield",
    label: "服务器安全",
    category: "安全运维",
    color: "#40875c",
    symbol: "WS",
    description: "接入服务器、扫描风险、调查事件，审批修复并跟踪回执。",
    capability: "设备 · 调查 · 修复审批",
    repository: "https://github.com/witkitlab/witshield",
  },
];
const steps = [
  {
    number: "01",
    title: "建立团队和项目",
    description:
      "邀请成员，明确团队角色与项目权限。始终知道自己正在操作哪个项目。",
  },
  {
    number: "02",
    title: "启用需要的应用",
    description: "在项目中启用应用，业务数据与成员权限有明确归属。",
  },
  {
    number: "03",
    title: "协作管理，保留记录",
    description: "查看真实状态、执行已支持的操作，并通过审计记录追溯项目变更。",
  },
];
</script>

<template>
  <div class="euler-site">
    <a class="skip-link" href="#main">跳转到正文</a>
    <header class="site-header">
      <div class="container header-inner">
        <a class="brand" href="/" aria-label="欧拉应用云首页">
          <svg class="brand-symbol" viewBox="0 0 40 40" aria-hidden="true">
            <path
              d="M8 8h25v7H15v5h15v7H15v5h18v7H8z"
              transform="translate(0 -4)"
              fill="currentColor"
            />
            <circle cx="34" cy="8" r="4" fill="#e5a47b" />
          </svg>
          <span>欧拉<span class="brand-detail">应用云</span></span>
        </a>
        <nav class="main-nav" aria-label="主导航">
          <a href="#workflow">团队协作</a><a href="#applications">应用目录</a
          ><a :href="deploymentUrl">部署文档</a>
        </nav>
        <a v-if="consoleUrl" class="header-cta" :href="consoleUrl"
          >进入控制台 <span aria-hidden="true">↗</span></a
        >
        <a v-else class="header-cta" :href="repositoryUrl"
          >GitHub <span aria-hidden="true">↗</span></a
        >
      </div>
    </header>

    <main id="main">
      <section class="hero container" aria-labelledby="hero-title">
        <div class="hero-copy">
          <p class="eyebrow">
            <span class="small-dot" /> EULER APPLICATION CLOUD
          </p>
          <h1 id="hero-title">
            团队、项目、应用。<br /><span>放在一起管理。</span>
          </h1>
          <p class="hero-description">
            在一个平台中使用 E时代生态的应用。<br
              class="desktop-break"
            />从成员身份到数据服务，让团队的日常协作更清楚。
          </p>
          <div class="hero-actions">
            <a class="button button-primary" :href="primaryUrl"
              >{{ primaryLabel }} <span aria-hidden="true">↗</span></a
            ><a class="button button-secondary" href="#applications"
              >浏览应用 <span aria-hidden="true">↓</span></a
            >
          </div>
          <p class="hero-note">自主部署 · 统一登录 · 项目权限</p>
        </div>
        <div
          class="workspace-visual"
          role="img"
          aria-label="结构示意：团队成员在项目空间中连接身份、开发、应用与安全服务"
        >
          <div class="visual-halo" />
          <div class="workspace-topline">
            <span class="visual-label">围绕项目组织工作</span
            ><span class="visual-caption">结构示意</span>
          </div>
          <div class="team-strip">
            <div class="team-glyph" aria-hidden="true">
              <span /><span /><span />
            </div>
            <div><strong>团队成员</strong><span>角色与项目权限</span></div>
            <svg viewBox="0 0 28 20" aria-hidden="true">
              <path d="M2 10h22m-7-7 7 7-7 7" />
            </svg>
            <div class="project-label">
              <span class="project-icon" aria-hidden="true">⌘</span> 项目空间
            </div>
          </div>
          <div class="project-panel">
            <div class="project-panel-head">
              <span class="project-indicator" /><strong
                >这个项目需要的应用</strong
              ><span class="panel-dots" aria-hidden="true">•••</span>
            </div>
            <div class="visual-app-grid">
              <div
                v-for="product in products"
                :key="product.id"
                class="visual-app"
                :style="{ '--product-color': product.color }"
              >
                <span class="visual-app-icon">{{ product.symbol }}</span
                ><span>{{ product.label }}</span>
              </div>
            </div>
            <div class="project-panel-foot">
              <span>连接与资源</span><span>成员与授权</span
              ><span>操作记录</span>
            </div>
          </div>
          <div class="visual-bottom">
            <span class="connector-line" /><span
              >一个工作空间，清楚的项目归属。</span
            >
          </div>
        </div>
      </section>

      <section
        id="workflow"
        class="workflow-section"
        aria-labelledby="workflow-title"
      >
        <div class="container">
          <div class="section-heading">
            <p class="eyebrow">WORK TOGETHER</p>
            <h2 id="workflow-title">从一个项目开始。</h2>
            <p>把成员、应用和资源放进同一个工作上下文。</p>
          </div>
          <div class="workflow-grid">
            <article
              v-for="step in steps"
              :key="step.number"
              class="workflow-item"
            >
              <span class="step-number">{{ step.number }}</span>
              <h3>{{ step.title }}</h3>
              <p>{{ step.description }}</p>
            </article>
          </div>
        </div>
      </section>

      <section
        id="applications"
        class="applications-section container"
        aria-labelledby="applications-title"
      >
        <div class="section-heading applications-heading">
          <div>
            <p class="eyebrow">E时代 ECOSYSTEM</p>
            <h2 id="applications-title">熟悉的产品，统一的入口。</h2>
            <p>按需要组合应用，在欧拉内完成完整业务流程。</p>
          </div>
          <a class="text-link" :href="capabilitiesUrl"
            >查看应用说明 <span aria-hidden="true">↗</span></a
          >
        </div>
        <div class="application-grid">
          <article
            v-for="product in products"
            :id="`app-${product.id}`"
            :key="product.id"
            class="application-card"
            :style="{ '--product-color': product.color }"
          >
            <div class="application-card-top">
              <span class="app-symbol" aria-hidden="true">{{
                product.symbol
              }}</span
              ><span class="app-category">{{ product.category }}</span>
            </div>
            <h3>{{ product.name }}</h3>
            <p class="app-label">{{ product.label }}</p>
            <p class="app-description">{{ product.description }}</p>
            <div class="app-capability">{{ product.capability }}</div>
            <a
              class="app-source"
              :href="product.repository"
              :aria-label="`查看 ${product.name} 源码`"
              >查看项目 <span aria-hidden="true">↗</span></a
            >
          </article>
        </div>
        <p class="applications-note">
          欧拉独立部署和保存应用数据。数据库、存储、邮件与 AI
          服务按部署环境配置，应用中的人员权限统一管理。
        </p>
      </section>

      <section
        class="principles-section container"
        aria-labelledby="principles-title"
      >
        <div class="principles-intro">
          <p class="eyebrow">CLEAR BY DESIGN</p>
          <h2 id="principles-title">统一工作入口，<br />保留产品边界。</h2>
          <p>
            欧拉独立承载应用功能与数据，统一工作空间、身份和权限。原有开源产品继续独立发展。
          </p>
          <a class="text-link" :href="capabilitiesUrl"
            >了解平台设计 <span aria-hidden="true">↗</span></a
          >
        </div>
        <div class="principles-list">
          <article>
            <span class="principle-mark identity-mark" aria-hidden="true"
              >01</span
            >
            <div>
              <h3>账号、资格、认证、权限各有职责</h3>
              <p>
                通行证负责登录，EID 提供成员资格，Trust
                提供认证状态；团队与项目权限由欧拉管理。
              </p>
            </div>
          </article>
          <article>
            <span class="principle-mark connection-mark" aria-hidden="true"
              >02</span
            >
            <div>
              <h3>每项应用都有完整工作台</h3>
              <p>
                从申请与审核到数据管理和安全运维，在项目内完成操作，并保留业务过程与审计记录。
              </p>
            </div>
          </article>
          <article>
            <span class="principle-mark privacy-mark" aria-hidden="true"
              >03</span
            >
            <div>
              <h3>业务数据有明确的归属</h3>
              <p>
                认证材料由欧拉加密保存，文件直接通过存储服务传输。项目之间独立授权，敏感操作单独校验权限。
              </p>
            </div>
          </article>
        </div>
      </section>

      <section class="start-section container" aria-labelledby="start-title">
        <div>
          <p class="eyebrow">MAKE IT YOUR WORKSPACE</p>
          <h2 id="start-title">为你的团队，建立应用工作空间。</h2>
          <p>从部署指南开始，建立团队的统一应用平台。</p>
        </div>
        <a class="button button-primary" :href="primaryUrl"
          >{{ primaryLabel }} <span aria-hidden="true">↗</span></a
        >
      </section>
    </main>
    <footer class="site-footer container">
      <a class="footer-brand" href="/">欧拉应用云 <span>EULER</span></a>
      <p>面向团队与项目的 E时代应用入口。</p>
      <nav aria-label="页脚导航">
        <a :href="deploymentUrl">部署指南</a
        ><a :href="capabilitiesUrl">接入能力</a
        ><a :href="repositoryUrl">GitHub ↗</a>
      </nav>
    </footer>
  </div>
</template>

<style scoped>
:global(html) {
  scroll-behavior: smooth;
  scroll-padding-top: 96px;
}
:global(body) {
  margin: 0;
}
:global(*) {
  box-sizing: border-box;
}
.euler-site {
  --ink: #222837;
  --muted: #6b7280;
  --line: #e7e8ee;
  --accent: #5c52b9;
  color: var(--ink);
  background: #fcfcfa;
  font-family:
    Inter,
    -apple-system,
    BlinkMacSystemFont,
    "Segoe UI",
    "PingFang SC",
    "Microsoft YaHei",
    sans-serif;
  font-size: 15px;
  line-height: 1.7;
  overflow: clip;
}
a {
  color: inherit;
  text-decoration: none;
}
a:focus-visible {
  outline: 3px solid #8c7ee2;
  outline-offset: 5px;
  border-radius: 5px;
}
h1,
h2,
h3,
p {
  margin: 0;
}
.container {
  width: min(1180px, calc(100% - 80px));
  margin-inline: auto;
}
.skip-link {
  position: fixed;
  top: -100px;
  left: 20px;
  z-index: 100;
  padding: 10px 20px;
  background: #fff;
}
.skip-link:focus {
  top: 10px;
}
.site-header {
  position: sticky;
  top: 0;
  z-index: 20;
  background: rgb(252 252 250 / 92%);
  border-bottom: 1px solid rgb(231 232 238 / 75%);
  backdrop-filter: blur(18px);
}
.header-inner {
  min-height: 80px;
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 28px;
}
.brand {
  display: flex;
  align-items: center;
  gap: 10px;
  font-size: 22px;
  font-weight: 750;
  letter-spacing: -0.05em;
}
.brand-symbol {
  width: 33px;
  height: 33px;
  color: var(--accent);
}
.brand-detail {
  margin-left: 9px;
  color: #777682;
  font-size: 16px;
  font-weight: 450;
  letter-spacing: 0.02em;
}
.main-nav {
  display: flex;
  gap: 32px;
  color: #535866;
  font-size: 14px;
}
.main-nav a:hover,
.text-link:hover,
.site-footer a:hover {
  color: var(--accent);
}
.header-cta {
  padding: 9px 16px;
  border: 1px solid #dcdde5;
  border-radius: 9px;
  font-size: 13px;
  font-weight: 600;
  white-space: nowrap;
  background: #fff;
}
.header-cta span {
  margin-left: 10px;
}
.hero {
  padding-block: 106px 112px;
  display: grid;
  grid-template-columns: 1.08fr 1fr;
  gap: 48px;
  align-items: center;
}
.eyebrow {
  font-size: 11px;
  font-weight: 650;
  letter-spacing: 0.13em;
  color: #77728c;
  line-height: 1.5;
}
.hero .eyebrow {
  display: flex;
  align-items: center;
  gap: 9px;
}
.small-dot {
  width: 6px;
  height: 6px;
  background: #ad997f;
  border-radius: 50%;
}
h1 {
  margin-top: 26px;
  font-size: clamp(34px, 3.8vw, 50px);
  line-height: 1.42;
  font-weight: 690;
  letter-spacing: -0.045em;
  white-space: nowrap;
}
h1 span {
  color: var(--accent);
}
.hero-description {
  margin-top: 23px;
  color: #6d7380;
  font-size: 16px;
  line-height: 1.95;
}
.hero-actions {
  display: flex;
  flex-wrap: wrap;
  gap: 12px;
  margin-top: 32px;
}
.button {
  display: inline-flex;
  justify-content: center;
  align-items: center;
  gap: 24px;
  min-height: 47px;
  padding: 11px 23px;
  border-radius: 10px;
  font-size: 14px;
  font-weight: 550;
  transition:
    transform 0.18s,
    background 0.18s;
}
.button:hover {
  transform: translateY(-2px);
}
.button-primary {
  background: var(--accent);
  color: #fff;
  box-shadow: 0 5px 14px #5c52b918;
}
.button-primary:hover {
  background: #4c429f;
}
.button-secondary {
  border: 1px solid #dedfe6;
  background: #fff;
  color: #5d6170;
}
.hero-note {
  margin-top: 20px;
  color: #8a8f9b;
  font-size: 11px;
  letter-spacing: 0.035em;
}
.workspace-visual {
  position: relative;
  isolation: isolate;
  padding: 27px 25px 22px;
  border: 1px solid #e4e4ec;
  border-radius: 20px;
  background: linear-gradient(135deg, #ffffffd9, #faf8ffbd);
  box-shadow:
    0 20px 70px -30px #59538126,
    0 3px 12px #40396503;
}
.visual-halo {
  position: absolute;
  width: 580px;
  height: 580px;
  max-width: 140%;
  inset: -85px auto auto -30px;
  z-index: -1;
  background: radial-gradient(
    ellipse,
    #e4def980,
    #f8e9dd36 52%,
    transparent 72%
  );
  pointer-events: none;
}
.workspace-topline {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 10px;
  margin-bottom: 24px;
}
.visual-label {
  font-size: 12px;
  letter-spacing: 0.025em;
  color: #65657a;
}
.visual-caption {
  font-size: 10px;
  color: #9b9aab;
}
.team-strip {
  display: flex;
  align-items: center;
  gap: 13px;
  margin-bottom: 22px;
}
.team-glyph {
  display: flex;
  padding-left: 3px;
}
.team-glyph span {
  width: 24px;
  height: 24px;
  border-radius: 50%;
  background: #c8bedf;
  border: 3px solid #fefeff;
  margin-left: -5px;
}
.team-glyph span:nth-child(2) {
  background: #d8beaa;
}
.team-glyph span:nth-child(3) {
  background: #a6c6c5;
}
.team-strip strong {
  display: block;
  font-size: 12px;
  font-weight: 600;
}
.team-strip div > span:not(.project-icon) {
  font-size: 9px;
  color: #9997a8;
}
.team-strip svg {
  width: 22px;
  height: 20px;
  margin-left: auto;
  fill: none;
  stroke: #bbb8cc;
  stroke-width: 1.5;
}
.project-label {
  display: flex;
  gap: 7px;
  align-items: center;
  font-size: 12px;
  color: #615976;
  font-weight: 600;
}
.project-icon {
  font-size: 23px;
  font-weight: 400;
  color: #8a7cb6;
}
.project-panel {
  background: #fff;
  border: 1px solid #e9e7f0;
  border-radius: 13px;
  box-shadow: 0 6px 20px #79709007;
  overflow: hidden;
}
.project-panel-head {
  display: flex;
  align-items: center;
  gap: 7px;
  padding: 16px;
  border-bottom: 1px solid #f0eef5;
}
.project-panel-head strong {
  font-size: 11px;
  font-weight: 550;
  color: #686278;
}
.project-indicator {
  height: 7px;
  width: 7px;
  border-radius: 2px;
  background: #9a8ecd;
}
.panel-dots {
  margin-left: auto;
  font-size: 9px;
  letter-spacing: 2px;
  color: #c3bfce;
}
.visual-app-grid {
  display: grid;
  grid-template-columns: repeat(4, 1fr);
  gap: 23px 10px;
  padding: 23px 10px;
}
.visual-app {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 7px;
  color: #7a7589;
  font-size: 9px;
}
.visual-app-icon {
  width: 33px;
  height: 33px;
  display: grid;
  place-items: center;
  border-radius: 9px;
  font-size: 10px;
  font-weight: 650;
  color: var(--product-color);
  background: color-mix(in srgb, var(--product-color) 9%, #fff);
  border: 1px solid color-mix(in srgb, var(--product-color) 12%, #fff);
}
.project-panel-foot {
  display: flex;
  justify-content: center;
  gap: 25px;
  padding: 11px 12px;
  background: #fcfbfe;
  border-top: 1px solid #f0eef5;
  font-size: 9px;
  color: #9a94a8;
}
.visual-bottom {
  display: flex;
  justify-content: center;
  align-items: center;
  gap: 10px;
  margin-top: 19px;
  font-size: 10px;
  color: #918c9f;
}
.connector-line {
  width: 19px;
  height: 1px;
  background: #c9c0d9;
}
.workflow-section {
  border-block: 1px solid var(--line);
  background: #f6f6f3;
  padding-block: 68px 71px;
}
.section-heading h2 {
  margin-top: 12px;
  font-size: clamp(25px, 2.6vw, 31px);
  line-height: 1.45;
  letter-spacing: -0.025em;
  font-weight: 630;
}
.section-heading > p:last-child,
.section-heading > div > p:last-child {
  margin-top: 12px;
  color: var(--muted);
  font-size: 14px;
}
.workflow-grid {
  display: grid;
  grid-template-columns: repeat(3, 1fr);
  gap: 50px;
  margin-top: 44px;
}
.step-number {
  display: inline-block;
  color: #9690a4;
  font-size: 12px;
  font-variant-numeric: tabular-nums;
  font-family: ui-monospace, monospace;
  margin-bottom: 15px;
}
.workflow-item h3 {
  font-size: 17px;
  font-weight: 580;
  margin-bottom: 9px;
}
.workflow-item p {
  font-size: 13px;
  line-height: 1.85;
  color: #767b85;
  max-width: 310px;
}
.applications-section {
  padding-block: 86px 77px;
}
.applications-heading {
  display: flex;
  justify-content: space-between;
  gap: 24px;
  align-items: end;
  margin-bottom: 35px;
}
.text-link {
  display: inline-flex;
  gap: 15px;
  font-size: 12px;
  color: #646073;
  white-space: nowrap;
}
.application-grid {
  display: grid;
  grid-template-columns: repeat(4, 1fr);
  gap: 15px;
}
.application-card {
  position: relative;
  display: flex;
  flex-direction: column;
  padding: 23px 21px 18px;
  border: 1px solid var(--line);
  background: #fff;
  border-radius: 13px;
  transition:
    border-color 0.18s,
    box-shadow 0.18s;
}
.application-card:hover {
  border-color: color-mix(in srgb, var(--product-color) 35%, #eee);
  box-shadow: 0 8px 20px #29253805;
}
.application-card-top {
  display: flex;
  justify-content: space-between;
  align-items: center;
  gap: 10px;
  margin-bottom: 23px;
}
.app-symbol {
  display: grid;
  place-items: center;
  width: 39px;
  height: 39px;
  border-radius: 11px;
  color: var(--product-color);
  background: color-mix(in srgb, var(--product-color) 9%, #fff);
  font-size: 12px;
  font-weight: 700;
}
.app-category {
  font-size: 10px;
  color: #98949f;
}
.application-card h3 {
  font-size: 16px;
  line-height: 1.45;
  font-weight: 610;
  letter-spacing: -0.025em;
}
.app-label {
  margin-top: 3px;
  color: #9c98a4;
  font-size: 11px;
}
.app-description {
  margin-top: 17px;
  margin-bottom: 23px;
  color: #767880;
  font-size: 12px;
  line-height: 1.85;
}
.app-capability {
  color: var(--product-color);
  font-size: 10px;
  margin-top: auto;
  margin-bottom: 15px;
}
.app-source {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding-top: 13px;
  border-top: 1px solid #f0eff3;
  color: #83808e;
  font-size: 11px;
}
.app-source:hover {
  color: var(--product-color);
}
.applications-note {
  margin-top: 19px;
  font-size: 11px;
  color: #92939a;
}
.principles-section {
  display: grid;
  grid-template-columns: 0.95fr 1.1fr;
  gap: 90px;
  border-top: 1px solid var(--line);
  padding-block: 75px 86px;
}
.principles-intro h2 {
  margin-top: 14px;
  font-size: 30px;
  line-height: 1.5;
  letter-spacing: -0.02em;
  font-weight: 610;
}
.principles-intro > p:last-of-type {
  color: #7f818c;
  font-size: 14px;
  line-height: 1.9;
  max-width: 370px;
  margin-top: 18px;
  margin-bottom: 22px;
}
.principles-list {
  display: grid;
  gap: 25px;
  padding-top: 3px;
}
.principles-list article {
  display: flex;
  gap: 20px;
}
.principle-mark {
  width: 30px;
  height: 30px;
  flex-shrink: 0;
  display: grid;
  place-items: center;
  border-radius: 8px;
  font-family: ui-monospace, monospace;
  font-size: 10px;
}
.identity-mark {
  color: #80729f;
  background: #f0ecf7;
}
.connection-mark {
  color: #567f76;
  background: #e9f1ed;
}
.privacy-mark {
  color: #a28463;
  background: #f5efe4;
}
.principles-list h3 {
  font-size: 15px;
  font-weight: 580;
  margin-bottom: 6px;
}
.principles-list p {
  color: #83838d;
  font-size: 12px;
  line-height: 1.9;
}
.start-section {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 28px;
  border: 1px solid #e8e5ef;
  border-radius: 18px;
  background: linear-gradient(110deg, #f2eff9, #f6f2ec);
  padding: 42px 45px;
  margin-bottom: 68px;
}
.start-section h2 {
  margin-top: 13px;
  font-size: 25px;
  line-height: 1.5;
  font-weight: 600;
  letter-spacing: -0.025em;
}
.start-section p:last-child {
  margin-top: 9px;
  font-size: 13px;
  color: #8c8497;
}
.start-section .button {
  flex-shrink: 0;
}
.site-footer {
  display: flex;
  align-items: center;
  gap: 22px;
  border-top: 1px solid var(--line);
  padding-block: 27px 35px;
  font-size: 11px;
  color: #92919a;
}
.footer-brand {
  font-size: 12px;
  font-weight: 600;
  color: #646074;
  white-space: nowrap;
}
.footer-brand span {
  font-size: 8px;
  margin-left: 6px;
  letter-spacing: 0.06em;
  color: #a8a2b5;
}
.site-footer nav {
  margin-left: auto;
  display: flex;
  gap: 23px;
}
@media (max-width: 1040px) {
  .container {
    width: calc(100% - 56px);
  }
  .hero {
    gap: 28px;
    padding-block: 76px 85px;
  }
  h1 {
    font-size: clamp(29px, 3.6vw, 37px);
  }
  .hero-description {
    font-size: 14px;
  }
  .workspace-visual {
    padding: 23px 18px 20px;
  }
  .application-grid {
    grid-template-columns: repeat(2, 1fr);
  }
  .application-card {
    padding: 24px;
  }
  .principles-section {
    gap: 50px;
  }
  .desktop-break {
    display: none;
  }
  .site-footer p {
    display: none;
  }
}
@media (max-width: 760px) {
  .container {
    width: calc(100% - 40px);
  }
  .header-inner {
    min-height: 70px;
    flex-wrap: wrap;
    gap: 0;
    padding-top: 14px;
  }
  .brand {
    font-size: 20px;
  }
  .brand-detail {
    font-size: 14px;
  }
  .header-cta {
    margin-left: auto;
    font-size: 12px;
  }
  .main-nav {
    order: 3;
    width: 100%;
    gap: 25px;
    padding: 13px 0 12px;
    font-size: 12px;
  }
  .hero {
    grid-template-columns: 1fr;
    gap: 48px;
    padding-block: 51px 57px;
  }
  .hero-copy {
    text-align: center;
  }
  .hero .eyebrow,
  .hero-actions {
    justify-content: center;
  }
  h1 {
    font-size: clamp(29px, 6.7vw, 45px);
    margin-top: 20px;
  }
  .hero-description {
    margin-top: 19px;
    max-width: 410px;
    margin-inline: auto;
  }
  .hero-actions {
    margin-top: 25px;
  }
  .hero-note {
    margin-top: 16px;
  }
  .workspace-visual {
    width: min(470px, 100%);
    margin-inline: auto;
  }
  .workflow-section {
    padding-block: 47px 48px;
  }
  .workflow-grid {
    grid-template-columns: 1fr;
    gap: 25px;
    margin-top: 32px;
  }
  .workflow-item {
    position: relative;
    padding-left: 40px;
  }
  .step-number {
    position: absolute;
    left: 0;
    top: 3px;
  }
  .workflow-item p {
    max-width: none;
  }
  .applications-section {
    padding-block: 55px 49px;
  }
  .applications-heading {
    align-items: start;
    flex-direction: column;
    gap: 18px;
  }
  .application-grid {
    gap: 12px;
  }
  .application-card {
    padding: 19px 16px 15px;
  }
  .app-category {
    display: none;
  }
  .application-card h3 {
    font-size: 14px;
  }
  .app-description {
    font-size: 11px;
  }
  .app-capability {
    font-size: 9px;
  }
  .principles-section {
    grid-template-columns: 1fr;
    gap: 34px;
    padding-block: 44px 51px;
  }
  .principles-intro h2 {
    font-size: 27px;
  }
  .principles-intro > p:last-of-type {
    max-width: none;
  }
  .start-section {
    align-items: start;
    flex-direction: column;
    padding: 28px 24px;
    margin-bottom: 40px;
  }
  .start-section h2 {
    font-size: 23px;
  }
  .site-footer {
    flex-wrap: wrap;
    row-gap: 14px;
  }
  .site-footer nav {
    margin-left: 0;
    width: 100%;
    gap: 24px;
  }
}
@media (max-width: 380px) {
  .container {
    width: calc(100% - 32px);
  }
  .application-grid {
    grid-template-columns: 1fr;
  }
  .app-category {
    display: block;
  }
  .app-description {
    font-size: 12px;
  }
  .hero-actions {
    gap: 8px;
  }
  .button {
    padding-inline: 18px;
    gap: 16px;
  }
  .project-panel-foot {
    gap: 16px;
  }
  .brand-detail {
    margin-left: 6px;
  }
  .header-cta {
    padding-inline: 12px;
  }
}
@media (prefers-reduced-motion: reduce) {
  :global(html) {
    scroll-behavior: auto;
  }
  * {
    transition: none !important;
  }
}
</style>
