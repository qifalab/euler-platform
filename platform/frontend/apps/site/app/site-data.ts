/**
 * Homepage content data — structured to mirror Google Cloud's homepage layout.
 * Product categories reuse the catalog from products.ts but regroup into
 * Google Cloud-style categories with bullet-point descriptions.
 */

export interface UpdateItem {
  category: string;
  title: string;
  desc: string;
  image: string;
}

export const updates: UpdateItem[] = [
  {
    category: "产品动态",
    title: "欧拉 AI 威胁防御正式发布",
    desc: "帮助企业以 AI 驱动的安全分析,更快识别和应对威胁。",
    image: "/images/update-1.jpg",
  },
  {
    category: "AI 基础设施",
    title: "新一代 AI 基础设施:面向 Agent 时代的扩展",
    desc: "支持更大规模模型推理与 Agent 编排的算力架构升级。",
    image: "/images/update-2.jpg",
  },
  {
    category: "数据云",
    title: "Agentic 数据云的新能力",
    desc: "驱动「行动系统」——从数据洞察到自动化决策的全链路。",
    image: "/images/update-3.jpg",
  },
  {
    category: "产品动态",
    title: "Gemini Enterprise:一个平台搞定 Agent 开发",
    desc: "统一的 Agent 开发、编排与治理平台,加速企业 AI 落地。",
    image: "/images/update-4.jpg",
  },
];

export interface ProgramItem {
  badge: string;
  title: string;
  desc: string;
}

export const tabs = ["开发者", "企业领袖", "特别计划"];

export const tabContent: ProgramItem[][] = [
  // 开发者
  [
    { badge: "产品动态", title: "Gemini 3.6 Flash 模型上线", desc: "更快的推理速度,更低的调用成本,适合高频 Agent 场景。" },
    { badge: "指南", title: "用 Agent Platform 构建多 Agent 系统", desc: "10 分钟内构建一个可工作的 AI 应用——从零到部署。" },
    { badge: "指南", title: "远程 MCP Server 实战", desc: "用全托管远程 MCP Server 快速接入企业工具链。" },
  ],
  // 企业领袖
  [
    { badge: "报告", title: "2026 AI ROI 报告:从 Token 到回报", desc: "量化企业 AI 投资回报,找到最优的 Agent 落地路径。" },
    { badge: "活动", title: "Build with Gemini 城市巡展", desc: "在你所在的城市获得 Gemini 实操经验,立即报名。" },
    { badge: "指南", title: "企业级 Agentic 工作指南", desc: "用 Gemini Enterprise 重塑企业工作流的实践手册。" },
  ],
  // 特别计划
  [
    { badge: "新用户", title: "¥300 免费额度 + 20+ 免费层产品", desc: "注册即享免费试用额度,覆盖计算、存储、数据库等核心产品。" },
    { badge: "AI 构建者", title: "GEAR 计划:每月 35 额度学 Agent", desc: "加入 Gemini Enterprise Agent Ready,学习构建企业级 Agent。" },
    { badge: "创业公司", title: "最高 ¥350 万云资源补贴", desc: "早期融资初创企业可通过欧拉创业计划获取云资源补贴。" },
  ],
];

export interface ProductCategory {
  title: string;
  items: string[];
  link: string;
}

export const categories: ProductCategory[] = [
  {
    title: "计算",
    items: [
      "用欧拉服务器创建可定制的虚拟机,支持 GPU 加速",
      "自动部署、弹性伸缩容器化应用",
      "按需迁移——无需重写代码即可容器化",
    ],
    link: "https://docs.eulercloud.cn/scecs",
  },
  {
    title: "存储",
    items: [
      "对象存储:任意类型、任意规模的数据存取",
      "块存储:与云服务器深度集成的高性能云盘",
      "在线/离线数据迁移工具,平滑迁移存量数据",
    ],
    link: "https://docs.eulercloud.cn/scoss",
  },
  {
    title: "数据库",
    items: [
      "托管 MySQL,一主一备高可用,降低运维成本",
      "99.99% 可用性 SLA,支撑核心业务系统",
      "与 AI 应用深度集成,支持向量检索",
    ],
    link: "https://docs.eulercloud.cn/scrds",
  },
  {
    title: "网络",
    items: [
      "专有网络(VPC):租户逻辑隔离,安全可控",
      "弹性公网 IP:独立购买、动态绑定",
      "混合连接:VPN、对等连接等企业级方案",
    ],
    link: "https://docs.eulercloud.cn/scvpc",
  },
  {
    title: "监控与运维",
    items: [
      "资源与自定义指标监控、告警与通知",
      "全链路可观测,从基础设施到应用层",
      "日志分析与审计,满足合规要求",
    ],
    link: "https://docs.eulercloud.cn/scmon",
  },
  {
    title: "安全",
    items: [
      "DDoS 防护与 Web 应用防火墙",
      "身份与访问管理(IAM),精细化权限控制",
      "数据加密与密钥管理,满足数据主权要求",
    ],
    link: "#",
  },
];
