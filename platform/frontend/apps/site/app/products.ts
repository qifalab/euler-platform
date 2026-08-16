interface Product {
  code: string;
  name: string;
  desc: string;
  link: string;
}

// Product catalog condensed from docs/architecture/01-product-catalog.md §2.
// In production this is fetched from the catalog service at SSR time (02§8.1).
export const products: Product[] = [
  { code: "SCVPC", name: "欧拉专有网络", desc: "租户逻辑隔离网络,一切资源的网络边界。", link: "https://docs.eulercloud.cn/scvpc" },
  { code: "SCECS", name: "欧拉服务器", desc: "云上虚拟服务器,一切资源的基础算力载体。", link: "https://docs.eulercloud.cn/scecs" },
  { code: "SCBS", name: "欧拉块存储", desc: "挂载云服务器的高性能云盘,快照与回滚。", link: "https://docs.eulercloud.cn/scbs" },
  { code: "SCOSS", name: "欧拉对象存储", desc: "RESTful 海量非结构化存储,S3 兼容 API。", link: "https://docs.eulercloud.cn/scoss" },
  { code: "SCRDS", name: "欧拉数据库 MySQL 版", desc: "托管 MySQL 关系型数据库,一主一备高可用。", link: "https://docs.eulercloud.cn/scrds" },
  { code: "SCMON", name: "欧拉监控", desc: "资源与自定义指标监控、告警与通知。", link: "https://docs.eulercloud.cn/scmon" },
  { code: "SCEIP", name: "弹性公网 IP", desc: "可独立购买与动态绑定的公网地址。", link: "https://docs.eulercloud.cn/sceip" },
];
