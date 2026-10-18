# matex 官网（静态站点）

`docs/` 目录本身就是一个可部署的静态站点：落地页、多页文档、示例索引。
零构建、零 npm 依赖、零外部运行时资源——`docs/` 直接丢给任意静态服务器或 GitHub Pages 即可。

```sh
# 本地预览（任选一种）
python3 -m http.server -d docs 8899     # http://localhost:8899
npx serve docs
```

## 目录结构

```
docs/
  index.html                  落地页
  guide/
    index.html                文档总览
    introduction.html         介绍与取舍
    quickstart.html           快速开始
    architecture.html         架构总览（启动序列、依赖方向）
    concepts/*.html           按能力分类的 16 页正文
    reference/*.html          pkg/core 速查、目录约定、命令与部署、AI skills
  examples/
    index.html                示例索引
    <name>.html               19 个示例各一页
  design.md                   设计取舍（也在站点外被 README 引用）
  assets/
    site.css                  全部样式（唯一 CSS 文件）
    site.js                   站点外壳 + 搜索 + 高亮 + Mermaid 初始化
    search-index.js           搜索索引（生成物）
    mermaid.min.js            Mermaid（已裁剪，仅保留 flowchart/sequence/state）
  tools/
    build-search-index.mjs    重新生成 search-index.js
    check-site.mjs            断链 / 外部资源 / 页面契约自检
```

## 页面怎么组织

**每页只写 `<main>` 里的正文。** 顶栏、侧边栏、面包屑、本页 TOC、上一页/下一页、页脚
全部由 `assets/site.js` 注入，导航只有一处真源（`site.js` 里的 `NAV` 数组）。
加一个页面 = 写一个 HTML + 在 `NAV` 里加一行。

页面骨架（复制即可）：

```html
<!doctype html>
<html lang="zh-CN">
	<head>
		<meta charset="utf-8" />
		<meta name="viewport" content="width=device-width, initial-scale=1" />
		<title>标题 — matex</title>
		<meta name="description" content="一句话描述。" />
		<link rel="stylesheet" href="../assets/site.css" />
	</head>
	<body data-page="guide/concepts/xxx">
		<a class="skip-link" href="#main">跳到正文</a>
		<main id="main" class="content prose">…</main>
		<script src="../assets/mermaid.min.js" defer></script>
		<script src="../assets/search-index.js" defer></script>
		<script src="../assets/site.js" defer></script>
	</body>
</html>
```

`data-page` 必须等于页面在站点中的路径（去掉 `.html`），它决定侧边栏高亮与面包屑。

## 可用的正文元素

| 元素 | 写法 |
|---|---|
| 导语 | `<p class="lede">…</p>` |
| 代码块 | `<pre><code class="language-go">…</code></pre>`（自动加语言标签与复制按钮） |
| 提示框 | `<div class="callout callout--note"><span class="callout__label">标题</span>正文</div>`（另有 `--warn` / `--tip`） |
| 表格 | `<div class="table-wrap"><table>…</table></div>`（窄屏可横滚） |
| 图 | `<figure class="mermaid-figure"><pre class="mermaid">flowchart LR …</pre><figcaption>说明</figcaption></figure>` |
| 卡片 | `<div class="cards"><a class="card" href="…"><span class="card__title">…</span><span class="card__desc">…</span></a></div>` |

代码块里的 `<` `>` `&` 必须写成 `&lt;` `&gt;` `&amp;`。

**行内 `<code>` 的长度限制**：行内代码是 `inline-block`，默认 `nowrap`。超过所在列宽
会被 `site.js` 标记为 `data-block-long`，改为允许在盒内折行。所以长 JSON / DSN 不会撑出横向
滚动条；但**别在正文里塞整段响应体**——那种内容该放进代码块。加长的示例已按此改过
（见 `examples/audit.html` 的审计事件字段）。

改完要跑的三件事：

```sh
node docs/tools/build-search-index.mjs   # 内容变了就重建搜索索引
node docs/tools/check-site.mjs           # 断链、外部资源与页面契约
node docs/tools/check-layout.mjs         # 四档视口的横向溢出扫描
```

`check-site.mjs` 检查：站内链接是否都存在、有没有引用外部运行时资源、每页是否
带 `main`/`data-page`/三份 assets 脚本/title/description。README 改动后也建议跑一次。

## 内容与代码的一致性

站点是**呈现层**，事实源始终是仓库代码与 `README.md` / `AGENTS.md` / `skills/`。
改了能力之后，以下页面对照关系需要同步：

| 代码变更 | 需要同步的页面 |
|---|---|
| `pkg/core/*` 新增包或改 API | `guide/reference/packages.html`、对应 `guide/concepts/*.html` |
| 新增/修改示例 | `docs/examples/<name>.html`、`docs/examples/index.html` |
| Makefile 目标、CI、Dockerfile | `guide/reference/commands.html`、`guide/quickstart.html` |
| 新增/修改 skill | `guide/reference/skills.html` |
| 分层或启动序列变化 | `guide/architecture.html`、`guide/reference/layout.html` |

## Mermaid 的体积取舍

`assets/mermaid.min.js` 是官方包经 esbuild 打包、并把图类型注册表裁剪到
**flowchart / sequence / state** 三个之后的结果（约 2.8MB，未压缩；gzip 后约 750KB）。
裁剪的理由很简单：站点只用这三种图，而完整包要多带十几个图解析器（甘特、饼图、
桑基、思维导图…），对离线可用的静态站是纯负担。要新增图类型时，需要重新打包并补注册。

## 样式约定

样式遵循仓库的全局 CSS 基线：颜色一律 `oklch()`（派生色走 `color-mix`）、逻辑属性、
流式 `clamp(min, rem + vw, max)`、`:focus-visible` 画环、`:hover` 包在
`@media (hover: hover) and (pointer: fine)`、动效尊重 `prefers-reduced-motion`。
改配色请只改 `assets/site.css` 顶部的设计令牌，不要在别处写死颜色。
