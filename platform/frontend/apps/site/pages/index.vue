<template>
  <div class="site">
    <!-- 1. Header: clean white, Google Cloud style nav -->
    <header class="site-header">
      <div class="container header-inner">
        <a class="logo" href="/">
          <svg class="logo-mark" viewBox="0 0 24 24" width="28" height="28" aria-hidden="true">
            <path fill="#4285F4" d="M22 12c0-.78-.07-1.53-.2-2.26H12v4.28h5.6a4.8 4.8 0 0 1-2.08 3.15v2.62h3.37C20.9 18.1 22 15.3 22 12Z"/>
            <path fill="#34A853" d="M12 23c2.7 0 4.97-.9 6.63-2.43l-3.37-2.62c-.9.6-2.05.96-3.26.96-2.5 0-4.63-1.7-5.39-3.97H3.14v2.71A10 10 0 0 0 12 23Z"/>
            <path fill="#FBBC04" d="M6.61 14.94a6 6 0 0 1 0-3.88V8.35H3.14a10 10 0 0 0 0 7.3l3.47-2.71Z"/>
            <path fill="#EA4335" d="M12 5.88c1.35 0 2.56.46 3.52 1.37l2.63-2.63A10 10 0 0 0 3.14 8.35l3.47 2.71C7.37 7.58 9.5 5.88 12 5.88Z"/>
          </svg>
          <span class="logo-text">欧拉算力平台</span>
        </a>
        <nav class="main-nav">
          <a href="#products">产品</a>
          <a href="#solutions">解决方案</a>
          <a href="#pricing">定价</a>
          <a href="https://docs.eulercloud.cn">文档</a>
        </nav>
        <div class="header-actions">
          <a class="header-link" href="https://console.eulercloud.cn">控制台</a>
          <a class="btn btn-primary btn-sm" href="https://console.eulercloud.cn">免费开始</a>
        </div>
      </div>
    </header>

    <!-- 2. Hero: large headline, credit emphasis, dual CTAs -->
    <section class="hero">
      <div class="container hero-inner">
        <h1>全新的上云方式</h1>
        <p class="hero-sub">
          用 AI 构建。快速部署和弹性扩展应用。分析并保护您的数据。<br />
          <strong>立即开始，获赠 ¥300 免费额度。</strong>
        </p>
        <div class="hero-actions">
          <a class="btn btn-primary" href="https://console.eulercloud.cn">免费开始</a>
          <a class="btn btn-secondary" href="#">联系销售</a>
        </div>
      </div>
    </section>

    <!-- 3. Latest updates carousel -->
    <section v-if="updates.length" class="updates">
      <div class="container">
        <h2 class="section-title">最新动态</h2>
        <div class="update-carousel">
          <article v-for="u in updates" :key="u.title" class="update-card">
            <div class="update-img">
              <img :src="u.image" :alt="u.title" loading="lazy" />
              <span class="update-cat-tag">{{ u.category }}</span>
            </div>
            <div class="update-body">
              <h3>{{ u.title }}</h3>
              <p>{{ u.desc }}</p>
            </div>
          </article>
        </div>
      </div>
    </section>

    <!-- 4. Tabbed programs section -->
    <section v-if="tabs.length" class="programs">
      <div class="container">
        <div class="tabs">
          <button
            v-for="(tab, i) in tabs"
            :key="tab"
            :class="{ active: activeTab === i }"
            @click="activeTab = i"
          >{{ tab }}</button>
        </div>
        <div class="tab-content">
          <div v-for="(items, i) in tabContent" :key="i" v-show="activeTab === i" class="tab-grid">
            <article v-for="item in items" :key="item.title" class="program-card">
              <span class="program-badge">{{ item.badge }}</span>
              <h3>{{ item.title }}</h3>
              <p>{{ item.desc }}</p>
              <a class="program-link" href="#">了解更多 →</a>
            </article>
          </div>
        </div>
      </div>
    </section>

    <!-- 5. Product categories (Google Cloud style) -->
    <section id="products" v-if="categories.length" class="products-section">
      <div class="container">
        <h2 class="section-title">专为开发者和 AI 设计的云平台</h2>
        <p class="section-sub">免费试用 20+ 产品,新客户注册即享 ¥300 免费额度。</p>
        <div class="cta-row">
          <a class="btn btn-primary" href="https://console.eulercloud.cn">免费开始</a>
          <a class="text-link" href="#all-products">查看全部 150+ 产品 →</a>
        </div>
        <div class="category-grid">
          <article v-for="cat in categories" :key="cat.title" class="category-card">
            <h3>{{ cat.title }}</h3>
            <ul>
              <li v-for="item in cat.items" :key="item">{{ item }}</li>
            </ul>
            <a class="category-link" :href="cat.link">了解更多 →</a>
          </article>
        </div>
      </div>
    </section>

    <!-- 6. Dark highlight section -->
    <section v-if="highlightVideos.length" class="highlight">
      <div class="container">
        <div class="highlight-head">
          <h2>用 AI 构建和扩展您的应用</h2>
          <p>欧拉 AI 平台提供全托管 AI 开发环境,200+ 基础模型,Agent Studio 可视化编排。</p>
          <a class="btn btn-primary" href="#">免费开始</a>
        </div>
        <div class="highlight-cards">
          <article v-for="v in highlightVideos" :key="v.title" class="highlight-card">
            <div class="highlight-thumb">
              <img :src="v.image" :alt="v.title" loading="lazy" />
              <span class="play-icon">▶</span>
            </div>
            <h3>{{ v.title }}</h3>
            <span class="highlight-duration">{{ v.duration }}</span>
          </article>
        </div>
      </div>
    </section>

    <!-- 7. CTA section -->
    <section class="cta-section">
      <div class="container cta-inner">
        <h2>准备好开始了吗?</h2>
        <p>新客户注册即享 ¥300 免费额度,免费试用 20+ 产品。</p>
        <a class="btn btn-primary btn-lg" href="https://console.eulercloud.cn">免费开始</a>
      </div>
    </section>

    <!-- 8. Footer -->
    <footer class="site-footer">
      <div class="container footer-grid">
        <div class="footer-col">
          <h4>产品</h4>
          <a href="https://docs.eulercloud.cn/scecs">云服务器</a>
          <a href="https://docs.eulercloud.cn/scoss">对象存储</a>
          <a href="https://docs.eulercloud.cn/scbs">块存储</a>
          <a href="https://docs.eulercloud.cn/scrds">云数据库</a>
        </div>
        <div class="footer-col">
          <h4>解决方案</h4>
          <a href="#">AI 训练</a>
          <a href="#">大数据分析</a>
          <a href="#">混合云</a>
          <a href="#">DevOps</a>
        </div>
        <div class="footer-col">
          <h4>资源</h4>
          <a href="https://docs.eulercloud.cn">文档</a>
          <a href="#">博客</a>
          <a href="#pricing">定价</a>
          <a href="#">案例</a>
        </div>
        <div class="footer-col">
          <h4>公司</h4>
          <a href="#">关于我们</a>
          <a href="#">联系方式</a>
          <a href="#">法律条款</a>
          <a href="#">隐私政策</a>
        </div>
      </div>
      <div class="container footer-bottom">
        <p>© 2026 欧拉算力平台 · eulercloud.cn</p>
      </div>
    </footer>
  </div>
</template>

<script setup lang="ts">
/**
 * Homepage — all sections are backend-driven (no client-side content data):
 *  - GET /api/v1/announcements (svc-notify) → 最新动态 news, 栏目 tab cards,
 *    AI highlight videos — one board, filtered by `type`.
 *  - GET /api/v1/catalog/products + /categories (svc-catalog) → 产品分类 grid.
 * Sections whose source is unavailable simply don't render (honest degradation).
 */
import { ref, computed } from "vue";

interface Announcement {
  id: string;
  type: "news" | "program" | "video";
  tab?: string;
  category?: string;
  badge?: string;
  title: string;
  description?: string;
  image?: string;
  link?: string;
  duration?: string;
  publishedAt: string;
}
interface CatalogProduct {
  productCode: string;
  productName: string;
  category: string;
  status: number;
}
interface CatalogCategory {
  code: string;
  name: string;
  description: string;
  link: string;
}
interface Envelope<T> {
  Code?: string;
  Message?: string;
  Data?: T;
}

const { data: annData } = await useFetch<Envelope<{ items: Announcement[] }>>("/api/v1/announcements");
const { data: prodData } = await useFetch<Envelope<CatalogProduct[]>>("/api/v1/catalog/products");
const { data: catData } = await useFetch<Envelope<CatalogCategory[]>>("/api/v1/catalog/categories");

const announcements = computed<Announcement[]>(() => annData.value?.Data?.items ?? []);
const onSale = computed<CatalogProduct[]>(() =>
  (prodData.value?.Data ?? []).filter((p) => p.status === 2),
);

const updates = computed(() =>
  announcements.value
    .filter((a) => a.type === "news")
    .map((a) => ({ title: a.title, desc: a.description ?? "", image: a.image ?? "", category: a.category ?? "" })),
);

const tabs = computed(() => {
  const seen: string[] = [];
  for (const a of announcements.value) {
    if (a.type === "program" && a.tab && !seen.includes(a.tab)) seen.push(a.tab);
  }
  return seen;
});
const tabContent = computed(() =>
  tabs.value.map((t) =>
    announcements.value
      .filter((a) => a.type === "program" && a.tab === t)
      .map((a) => ({ badge: a.badge ?? "", title: a.title, desc: a.description ?? "" })),
  ),
);

const highlightVideos = computed(() =>
  announcements.value
    .filter((a) => a.type === "video")
    .map((a) => ({ title: a.title, duration: a.duration ?? "", image: a.image ?? "" })),
);

const categories = computed(() =>
  (catData.value?.Data ?? [])
    .map((c) => ({
      title: c.name,
      items: onSale.value.filter((p) => p.category === c.code).map((p) => p.productName),
      link: c.link,
    }))
    .filter((c) => c.items.length > 0),
);

const activeTab = ref(0);
</script>

<style>
/* ---------------------------------------------------------------------------
 * Google Cloud homepage style — clean white surfaces, NOT glass/blur.
 * Global tokens (--sc-* vars, font stack) are consumed from @sc/tokens,
 * but the mesh background and glass effects are intentionally overridden.
 * ------------------------------------------------------------------------- */
* { box-sizing: border-box; }

/* Override tokens body mesh — Google Cloud uses clean white/light bg. */
body {
  background-color: #ffffff;
  background-image: none;
  background-attachment: scroll;
  line-height: 1.6;
}

.container { max-width: 1280px; margin: 0 auto; padding: 0 24px; }

/* ---- Buttons (Google Cloud pill style) ---- */
.btn {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  padding: 12px 32px;
  border-radius: 999px;
  text-decoration: none;
  font-weight: 500;
  font-size: 14px;
  font-family: var(--sc-font-family);
  border: 1px solid transparent;
  cursor: pointer;
  transition: box-shadow 180ms cubic-bezier(0.4, 0, 0.2, 1),
              background 180ms cubic-bezier(0.4, 0, 0.2, 1);
  white-space: nowrap;
}
.btn-sm { padding: 8px 24px; font-size: 13px; }
.btn-lg { padding: 16px 40px; font-size: 16px; }
.btn-primary { background: var(--sc-color-brand); color: #fff; }
.btn-primary:hover { background: var(--sc-color-brand-hover); box-shadow: 0 1px 3px rgba(60, 64, 67, 0.3), 0 4px 8px rgba(60, 64, 67, 0.15); }
.btn-secondary { background: transparent; color: var(--sc-color-brand); border-color: var(--sc-color-brand); }
.btn-secondary:hover { background: var(--sc-color-brand-soft); box-shadow: 0 1px 2px rgba(60, 64, 67, 0.1); }

.text-link { color: var(--sc-color-brand); text-decoration: none; font-size: 14px; font-weight: 500; }
.text-link:hover { text-decoration: underline; }

/* ---- Header ---- */
.site-header {
  position: sticky;
  top: 0;
  z-index: 100;
  background: #fff;
  border-bottom: 1px solid #e8eaed;
}
.header-inner { display: flex; align-items: center; justify-content: space-between; height: 64px; gap: 32px; }
.logo { display: flex; align-items: center; gap: 8px; text-decoration: none; }
.logo-mark { flex-shrink: 0; }
.logo-text { font-weight: 500; font-size: 18px; color: var(--sc-text-primary); }
.main-nav { display: flex; gap: 8px; flex: 1; }
.main-nav a {
  color: var(--sc-text-primary);
  text-decoration: none;
  font-size: 14px;
  font-weight: 400;
  padding: 8px 16px;
  border-radius: 8px;
  transition: background 180ms;
}
.main-nav a:hover { background: #f1f3f4; }
.header-actions { display: flex; align-items: center; gap: 16px; }
.header-link { color: var(--sc-color-brand); text-decoration: none; font-size: 14px; font-weight: 500; }
.header-link:hover { text-decoration: underline; }

/* ---- Hero ---- */
.hero {
  padding: 100px 0 80px;
  text-align: center;
  background: linear-gradient(180deg, #f8fbff 0%, #ffffff 100%);
}
.hero-inner { max-width: 720px; margin: 0 auto; }
.hero h1 {
  font-size: 56px;
  font-weight: 400;
  line-height: 1.15;
  letter-spacing: -0.5px;
  margin: 0 0 20px;
  color: var(--sc-text-primary);
}
.hero-sub {
  font-size: 18px;
  line-height: 1.6;
  color: var(--sc-text-secondary);
  margin: 0 0 32px;
}
.hero-sub strong { color: var(--sc-text-primary); font-weight: 500; }
.hero-actions { display: flex; gap: 16px; justify-content: center; flex-wrap: wrap; }

/* ---- Section common ---- */
.section-title { font-size: 36px; font-weight: 400; color: var(--sc-text-primary); margin: 0 0 12px; letter-spacing: -0.3px; }
.section-sub { font-size: 16px; color: var(--sc-text-secondary); margin: 0 0 32px; }
.cta-row { display: flex; align-items: center; gap: 24px; margin-bottom: 48px; flex-wrap: wrap; }

/* ---- Updates carousel ---- */
.updates { padding: 64px 0; background: #fff; }
.update-carousel {
  display: flex;
  gap: 20px;
  overflow-x: auto;
  scroll-snap-type: x mandatory;
  padding-bottom: 8px;
  scrollbar-width: thin;
}
.update-carousel::-webkit-scrollbar { height: 6px; }
.update-carousel::-webkit-scrollbar-track { background: #f1f3f4; border-radius: 3px; }
.update-carousel::-webkit-scrollbar-thumb { background: #c6cadb; border-radius: 3px; }
.update-card {
  flex: 0 0 340px;
  scroll-snap-align: start;
  border-radius: 12px;
  overflow: hidden;
  border: 1px solid #e8eaed;
  background: #fff;
  transition: box-shadow 180ms, transform 180ms;
  cursor: pointer;
}
.update-card:hover { box-shadow: 0 2px 8px rgba(60, 64, 67, 0.12), 0 8px 24px rgba(60, 64, 67, 0.08); transform: translateY(-2px); }
.update-img { height: 180px; position: relative; overflow: hidden; }
.update-img img { width: 100%; height: 100%; object-fit: cover; display: block; }
.update-cat-tag {
  position: absolute;
  bottom: 12px;
  left: 12px;
  background: rgba(255, 255, 255, 0.95);
  color: var(--sc-text-primary);
  font-size: 12px;
  font-weight: 500;
  padding: 4px 10px;
  border-radius: 4px;
}
.update-body { padding: 20px; }
.update-body h3 { font-size: 16px; font-weight: 500; margin: 0 0 8px; color: var(--sc-text-primary); line-height: 1.4; }
.update-body p { font-size: 14px; color: var(--sc-text-secondary); margin: 0; line-height: 1.5; }

/* ---- Programs / tabs ---- */
.programs { padding: 64px 0; background: #f8f9fa; }
.tabs { display: flex; gap: 0; border-bottom: 1px solid #dadce0; margin-bottom: 40px; }
.tabs button {
  background: none;
  border: none;
  border-bottom: 3px solid transparent;
  padding: 16px 32px;
  font-size: 15px;
  font-weight: 500;
  color: var(--sc-text-secondary);
  cursor: pointer;
  font-family: var(--sc-font-family);
  transition: color 180ms, border-color 180ms;
}
.tabs button:hover { color: var(--sc-text-primary); }
.tabs button.active { color: var(--sc-color-brand); border-bottom-color: var(--sc-color-brand); }
.tab-grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(320px, 1fr)); gap: 24px; }
.program-card {
  background: #fff;
  border: 1px solid #e8eaed;
  border-radius: 12px;
  padding: 28px;
  transition: box-shadow 180ms;
}
.program-card:hover { box-shadow: 0 1px 3px rgba(60, 64, 67, 0.1), 0 4px 12px rgba(60, 64, 67, 0.08); }
.program-badge {
  display: inline-block;
  font-size: 12px;
  font-weight: 500;
  color: var(--sc-color-brand);
  background: var(--sc-color-brand-soft);
  padding: 4px 10px;
  border-radius: 4px;
  margin-bottom: 16px;
}
.program-card h3 { font-size: 18px; font-weight: 500; margin: 0 0 12px; color: var(--sc-text-primary); line-height: 1.4; }
.program-card p { font-size: 14px; color: var(--sc-text-secondary); margin: 0 0 16px; line-height: 1.6; }
.program-link { color: var(--sc-color-brand); text-decoration: none; font-size: 14px; font-weight: 500; }
.program-link:hover { text-decoration: underline; }

/* ---- Product categories ---- */
.products-section { padding: 80px 0; background: #fff; }
.category-grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(340px, 1fr)); gap: 24px; }
.category-card {
  background: #fff;
  border: 1px solid #e8eaed;
  border-radius: 12px;
  padding: 32px;
  transition: box-shadow 180ms;
}
.category-card:hover { box-shadow: 0 1px 3px rgba(60, 64, 67, 0.1), 0 4px 12px rgba(60, 64, 67, 0.08); }
.category-card h3 { font-size: 20px; font-weight: 500; margin: 0 0 20px; color: var(--sc-text-primary); }
.category-card ul { list-style: none; padding: 0; margin: 0 0 20px; }
.category-card li {
  font-size: 14px;
  color: var(--sc-text-secondary);
  line-height: 1.6;
  padding: 6px 0 6px 24px;
  position: relative;
}
.category-card li::before {
  content: "";
  position: absolute;
  left: 0;
  top: 14px;
  width: 6px;
  height: 6px;
  border-radius: 50%;
  background: var(--sc-color-brand);
}
.category-link { color: var(--sc-color-brand); text-decoration: none; font-size: 14px; font-weight: 500; }
.category-link:hover { text-decoration: underline; }

/* ---- Dark highlight section ---- */
.highlight {
  padding: 80px 0;
  background: #1a1a2e;
  color: #e8eaed;
}
.highlight-head { text-align: center; margin-bottom: 48px; }
.highlight-head h2 { font-size: 36px; font-weight: 400; margin: 0 0 16px; color: #fff; letter-spacing: -0.3px; }
.highlight-head p { font-size: 16px; color: #9aa0a6; margin: 0 0 24px; max-width: 560px; margin-left: auto; margin-right: auto; }
.highlight-cards { display: grid; grid-template-columns: repeat(auto-fill, minmax(260px, 1fr)); gap: 20px; }
.highlight-card {
  border-radius: 12px;
  overflow: hidden;
  background: #232342;
  transition: transform 180ms, box-shadow 180ms;
  cursor: pointer;
}
.highlight-card:hover { transform: translateY(-4px); box-shadow: 0 8px 32px rgba(0, 0, 0, 0.3); }
.highlight-thumb {
  height: 160px;
  display: flex;
  align-items: center;
  justify-content: center;
  position: relative;
  overflow: hidden;
}
.highlight-thumb img { width: 100%; height: 100%; object-fit: cover; position: absolute; top: 0; left: 0; }
.play-icon {
  font-size: 32px;
  color: rgba(255, 255, 255, 0.9);
  background: rgba(0, 0, 0, 0.5);
  border-radius: 50%;
  width: 56px;
  height: 56px;
  display: flex;
  align-items: center;
  justify-content: center;
  padding-left: 4px;
  position: relative;
  z-index: 1;
}
.highlight-card h3 { font-size: 15px; font-weight: 500; margin: 0; padding: 16px 20px 4px; color: #e8eaed; line-height: 1.4; }
.highlight-duration { display: block; padding: 0 20px 16px; font-size: 13px; color: #9aa0a6; }

/* ---- CTA section ---- */
.cta-section { padding: 80px 0; background: linear-gradient(180deg, #f8fbff 0%, #e8f0fe 100%); text-align: center; }
.cta-inner { max-width: 600px; margin: 0 auto; }
.cta-section h2 { font-size: 36px; font-weight: 400; margin: 0 0 16px; color: var(--sc-text-primary); letter-spacing: -0.3px; }
.cta-section p { font-size: 16px; color: var(--sc-text-secondary); margin: 0 0 32px; }

/* ---- Footer ---- */
.site-footer { background: #1a1a2e; color: #9aa0a6; padding: 64px 0 32px; }
.footer-grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(200px, 1fr)); gap: 40px; margin-bottom: 48px; }
.footer-col h4 { font-size: 14px; font-weight: 500; color: #e8eaed; margin: 0 0 16px; }
.footer-col a { display: block; font-size: 13px; color: #9aa0a6; text-decoration: none; padding: 6px 0; transition: color 180ms; }
.footer-col a:hover { color: #e8eaed; }
.footer-bottom { border-top: 1px solid #2a2a4a; padding-top: 24px; text-align: center; }
.footer-bottom p { font-size: 13px; margin: 0; }

/* ---- Responsive ---- */
@media (max-width: 768px) {
  .hero h1 { font-size: 36px; }
  .hero-sub { font-size: 16px; }
  .section-title { font-size: 28px; }
  .main-nav { display: none; }
  .header-actions .header-link { display: none; }
  .highlight-head h2 { font-size: 28px; }
}
</style>
