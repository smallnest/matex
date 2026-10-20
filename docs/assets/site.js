/* matex 站点外壳。
 *
 * 每页 HTML 只写 <main> 正文；顶栏、侧边栏、面包屑、上下页、页脚与 TOC
 * 都由这里注入。理由：手写多页站点最容易坏在「导航复制 30 份」，改一处要
 * 改三十处——所以导航只有这一个真源（NAV），页面用 <body data-page="...">
 * 声明自己是谁。
 *
 * 渐进增强：脚本失败时正文照常可读，只是没有导航。
 */
(function () {
	"use strict";

	var REPO = "https://github.com/smallnest/matex";

	/* ── 站点地图（唯一真源） ──────────────────────────────────────────── */

	var NAV = [
		{
			title: "开始",
			items: [
				{ path: "guide/index", label: "文档总览" },
				{ path: "guide/introduction", label: "介绍" },
				{ path: "guide/quickstart", label: "快速开始" },
				{ path: "guide/architecture", label: "架构总览" }
			]
		},
		{
			title: "核心概念",
			items: [
				{ path: "guide/concepts/service", label: "服务与生命周期" },
				{ path: "guide/concepts/config", label: "配置" },
				{ path: "guide/concepts/http", label: "HTTP 与路由" },
				{ path: "guide/concepts/errors", label: "错误模型" },
				{ path: "guide/concepts/observability", label: "可观测性" }
			]
		},
		{
			title: "数据与集成",
			items: [
				{ path: "guide/concepts/database", label: "数据库" },
				{ path: "guide/concepts/cache", label: "缓存模式" },
				{ path: "guide/concepts/messaging", label: "Kafka 消息" },
				{ path: "guide/concepts/rpc", label: "gRPC 与 rpcx" }
			]
		},
		{
			title: "生产治理",
			items: [
				{ path: "guide/concepts/resilience", label: "稳定性防护" },
				{ path: "guide/concepts/security", label: "认证与安全" },
				{ path: "guide/concepts/governance", label: "开关与审计" },
				{ path: "guide/concepts/scheduling", label: "定时任务" },
				{ path: "guide/concepts/llm", label: "LLM 客户端" }
			]
		},
		{
			title: "测试与参考",
			items: [
				{ path: "guide/concepts/testing", label: "离线测试" },
				{ path: "guide/reference/packages", label: "pkg/core 速查" },
				{ path: "guide/reference/layout", label: "目录与约定" },
				{ path: "guide/reference/commands", label: "命令与部署" },
				{ path: "guide/reference/skills", label: "AI Skills" }
			]
		}
	];

	/* 示例清单：侧边栏用它列出每个示例，避免"示例页没有 active 项"。 */
	var EXAMPLES = [
		{ id: "config", label: "配置" },
		{ id: "nacos", label: "Nacos 配置中心" },
		{ id: "errors", label: "错误模型" },
		{ id: "http", label: "HTTP 与路由" },
		{ id: "lifecycle", label: "生命周期与热更" },
		{ id: "observability", label: "可观测性" },
		{ id: "database", label: "数据库" },
		{ id: "cache", label: "缓存模式" },
		{ id: "redis", label: "Redis" },
		{ id: "memcache", label: "Memcached" },
		{ id: "kafka", label: "Kafka" },
		{ id: "grpc", label: "gRPC" },
		{ id: "rpcx", label: "rpcx" },
		{ id: "resilience", label: "稳定性防护" },
		{ id: "auth", label: "认证授权" },
		{ id: "cron", label: "定时任务" },
		{ id: "feature", label: "功能开关" },
		{ id: "audit", label: "审计日志" },
		{ id: "llm", label: "LLM 客户端" }
	];

	/* ── 路径工具 ──────────────────────────────────────────────────────── */

	// 页面在站点中的层级决定相对前缀：站点里的链接统一写成 site-root 相对形式。
	var depth = (function () {
		var p = location.pathname;
		var i = p.indexOf("/docs/");
		var rel = i >= 0 ? p.slice(i + 6) : p.replace(/^\/+/, "");
		rel = rel.replace(/index\.html$/, "").replace(/[^/]*$/, "");
		return rel ? rel.split("/").length : 0;
	})();

	function url(path) {
		if (path === "") return "../".repeat(depth) + "index.html";
		return "../".repeat(depth) + path + ".html";
	}

	function repoURL(p) {
		return REPO + "/blob/master/" + p;
	}

	/* ── 页面标识 ─────────────────────────────────────────────────────── */

	var body = document.body;
	var pageId = body.dataset.page || "index";
	body.dataset.page = pageId;

	function titleFor(id) {
		for (var g = 0; g < NAV.length; g++) {
			for (var i = 0; i < NAV[g].items.length; i++) {
				if (NAV[g].items[i].path === id) return NAV[g].items[i].label;
			}
		}
		return null;
	}

	var flat = NAV.flatMap(function (g) {
		return g.items;
	});

	/* ── 图标 ─────────────────────────────────────────────────────────── */

	var ICON_MARK =
		'<svg class="brand__mark" viewBox="0 0 24 24" fill="none" aria-hidden="true">' +
		'<rect x="1.5" y="1.5" width="21" height="21" rx="6" stroke="currentColor" stroke-width="1.6"/>' +
		'<path d="M6.5 16.5 12 6l5.5 10.5" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"/>' +
		"</svg>";

	var ICON_CHECK =
		'<svg viewBox="0 0 24 24" fill="none" aria-hidden="true">' +
		'<path d="m5 12.5 4.5 4.5L19 7" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"/></svg>';

	/* ── 外壳 ─────────────────────────────────────────────────────────── */

	function el(html) {
		var t = document.createElement("template");
		t.innerHTML = html.trim();
		return t.content.firstElementChild;
	}

	function buildHeader() {
		var top = pageId.split("/")[0];
		var nav = [
			{ href: url(""), label: "首页", active: pageId === "index" },
			{ href: url("guide/index"), label: "文档", active: pageId.indexOf("guide/") === 0 },
			{ href: url("examples/index"), label: "Examples", active: pageId.indexOf("examples/") === 0 },
			{ href: REPO, label: "GitHub", external: true }
		];

		var html =
			'<header class="topbar"><div class="topbar__inner">' +
			'<a class="brand" href="' + url("") + '">' + ICON_MARK + "matex</a>" +
			'<button class="navtoggle" type="button" aria-expanded="false" aria-controls="topnav">菜单</button>' +
			'<nav class="topnav" id="topnav" aria-label="主导航">';

		nav.forEach(function (n) {
			var attrs = n.external
				? ' target="_blank" rel="noopener"'
				: n.active
					? ' aria-current="true"'
					: "";
			var label = n.external
				? n.label + '<span class="visually-hidden">（在新标签页打开）</span>'
				: n.label;
			html += '<a href="' + n.href + '"' + attrs + ">" + label + "</a>";
		});

		html +=
			"</nav>" +
			'<button class="searchbtn" type="button" aria-label="搜索文档" data-search-open>' +
			'<svg viewBox="0 0 24 24" width="15" height="15" fill="none" aria-hidden="true">' +
			'<circle cx="11" cy="11" r="6.5" stroke="currentColor" stroke-width="1.8"/>' +
			'<path d="m16 16 4.5 4.5" stroke="currentColor" stroke-width="1.8" stroke-linecap="round"/></svg>' +
			"<span>搜索</span><kbd>/</kbd></button>" +
			"</div></header>";

		return el(html);
	}

	function buildSidebar() {
		var html = '<aside class="sidebar" id="sidebar" aria-label="文档目录"><div class="sidebar__inner">';
		NAV.forEach(function (group) {
			html += '<div class="sidebar__group"><p class="sidebar__title">' + group.title + "</p><ul>";
			group.items.forEach(function (item) {
				var current = item.path === pageId ? ' aria-current="page"' : "";
				html += '<li><a href="' + url(item.path) + '"' + current + ">" + item.label + "</a></li>";
			});
			html += "</ul></div>";
		});
		if (pageId.indexOf("examples/") === 0) {
			html += '<div class="sidebar__group"><p class="sidebar__title">示例</p><ul>';
			html +=
				'<li><a href="' + url("examples/index") + '"' +
				(pageId === "examples/index" ? ' aria-current="page"' : "") +
				">全部示例</a></li>";
			EXAMPLES.forEach(function (e) {
				var current = pageId === "examples/" + e.id ? ' aria-current="page"' : "";
				html +=
					'<li><a href="' + url("examples/" + e.id) + '"' + current + ">" + e.label + "</a></li>";
			});
			html += "</ul></div>";
		}
		html += "</div></aside>";
		return el(html);
	}

	function buildFooter() {
		var blocks = [
			{
				title: "文档",
				links: [
					{ href: url("guide/quickstart"), label: "快速开始" },
					{ href: url("guide/architecture"), label: "架构总览" },
					{ href: url("guide/reference/packages"), label: "pkg/core 速查" }
				]
			},
			{
				title: "示例",
				links: [
					{ href: url("examples/http"), label: "HTTP 服务" },
					{ href: url("examples/database"), label: "数据库" },
					{ href: url("examples/resilience"), label: "稳定性防护" }
				]
			},
			{
				title: "项目",
				links: [
					{ href: REPO, label: "GitHub", external: true },
					{ href: repoURL("docs/design.md"), label: "设计取舍", external: true },
					{ href: repoURL("AGENTS.md"), label: "AGENTS.md", external: true }
				]
			}
		];

		var html = '<footer class="footer"><div class="footer__inner">';
		blocks.forEach(function (b) {
			html += "<div><h2>" + b.title + "</h2><ul>";
			b.links.forEach(function (l) {
				html +=
					'<li><a href="' +
					l.href +
					'"' +
					(l.external ? ' target="_blank" rel="noopener"' : "") +
					">" +
					l.label +
					"</a></li>";
			});
			html += "</ul></div>";
		});
		html += "</div>";
		html +=
			'<p class="footer__note">matex — 大道至简的企业级 Go 脚手架 · MIT License · ' +
			"本站为静态站点，不加载任何外部运行时资源。</p></footer>";
		return el(html);
	}

	/* ── 面包屑 / 上下页 / TOC ────────────────────────────────────────── */

	function decorateMain(main) {
		if (pageId.indexOf("guide/") !== 0 && pageId.indexOf("examples/") !== 0) return;

		var crumb = document.createElement("nav");
		crumb.className = "breadcrumb";
		crumb.setAttribute("aria-label", "面包屑");
		var chain = [{ href: url(""), label: "首页" }];
		if (pageId.indexOf("guide/") === 0) {
			chain.push({ href: url("guide/index"), label: "文档" });
			var section = pageId.split("/")[1];
			if (section === "concepts") chain.push({ href: url("guide/index"), label: "核心概念" });
			if (section === "reference") chain.push({ href: url("guide/index"), label: "参考" });
			var local = titleFor(pageId);
			if (local) chain.push({ label: local });
		} else {
			chain.push({ href: url("examples/index"), label: "Examples" });
			var h1 = main.querySelector("h1");
			if (h1) chain.push({ label: h1.textContent.trim() });
		}

		var ol = document.createElement("ol");
		chain.forEach(function (c) {
			var li = document.createElement("li");
			if (c.href) {
				var a = document.createElement("a");
				a.href = c.href;
				a.textContent = c.label;
				li.appendChild(a);
			} else {
				li.setAttribute("aria-current", "page");
				li.textContent = c.label;
			}
			ol.appendChild(li);
		});
		crumb.appendChild(ol);
		// 插在 shell 内部（shell 之后会把它和 main 一起摆进网格），
		// 这样面包屑和正文共享同一套内边距。
		main.insertBefore(crumb, main.firstChild);
		main.classList.add("content--with-crumb");
	}

	function buildPager() {
		var idx = -1;
		for (var i = 0; i < flat.length; i++) {
			if (flat[i].path === pageId) {
				idx = i;
				break;
			}
		}
		if (idx < 0) return null;
		var prev = flat[idx - 1];
		var next = flat[idx + 1];
		if (!prev && !next) return null;

		var html = '<nav class="pager" aria-label="翻页">';
		if (prev) {
			html +=
				'<a href="' + url(prev.path) + '"><span class="pager__label">上一页</span>' +
				prev.label +
				"</a>";
		}
		if (next) {
			html +=
				'<a href="' + url(next.path) + '"><span class="pager__label">下一页</span>' +
				next.label +
				"</a>";
		}
		html += "</nav>";
		return el(html);
	}

	function buildTOC(main) {
		var heads = main.querySelectorAll("h2, h3");
		if (heads.length < 3) return null;

		var html = '<div class="toc"><div class="toc__inner"><p class="sidebar__title">本页内容</p><ul>';
		Array.prototype.forEach.call(heads, function (h) {
			if (!h.id) {
				h.id = slugify(h.textContent);
			}
			html +=
				'<li><a href="#' +
				h.id +
				'" data-depth="' +
				(h.tagName === "H3" ? "3" : "2") +
				'">' +
				escapeHTML(h.textContent) +
				"</a></li>";
		});
		html += "</ul></div></div>";
		return el(html);
	}

	function slugify(text) {
		return (
			text
				.trim()
				.toLowerCase()
				.replace(/[^\p{Letter}\p{Number}]+/gu, "-")
				.replace(/^-+|-+$/g, "") || "section"
		);
	}

	function escapeHTML(s) {
		return s.replace(/[&<>"]/g, function (c) {
			return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c];
		});
	}

	/* ── 代码高亮（极简分词，不引第三方） ─────────────────────────────── */

	var GO_KEYWORDS =
		/\b(func|type|struct|interface|map|chan|return|if|else|for|range|switch|case|default|var|const|package|import|go|defer|select|break|continue|nil|true|false|error|string|int|int64|bool|float64|byte|any|time|context)\b/g;

	function highlight(code, lang) {
		var text = escapeHTML(code);
		if (lang === "bash" || lang === "sh" || lang === "shell") {
			// 注释 + 命令名，够用且不会把路径里的斜杠搞错。
			return text
				.replace(/^(\s*)(#.*)$/gm, '$1<span class="tok-comment">$2</span>')
				.replace(/^(\s*)([a-z][\w.-]*)(\s)/gm, '$1<span class="tok-fn">$2</span>$3');
		}
		if (lang === "yaml" || lang === "yml") {
			return text
				.replace(/^(\s*)(#.*)$/gm, '$1<span class="tok-comment">$2</span>')
				.replace(/^(\s*)([\w.-]+)(:)/gm, '$1<span class="tok-key">$2</span>$3')
				.replace(/(&quot;[^&]*?&quot;|'[^']*?')/g, '<span class="tok-str">$1</span>');
		}
		// Go 与其它类 C 语言
		return text
			.replace(/(\/\/[^\n]*)/g, '<span class="tok-comment">$1</span>')
			.replace(/(&quot;[^&]*?&quot;|`[^`]*?`)/g, '<span class="tok-str">$1</span>')
			.replace(GO_KEYWORDS, '<span class="tok-key">$1</span>')
			.replace(/\b(\d+(?:\.\d+)?(?:ms|s|us|ns)?)\b/g, '<span class="tok-num">$1</span>');
	}

	/* 超长的行内代码（DSN、JSON 响应）会把父段落顶出视口。
	 * CSS 里唯一的解法是 contain: inline-size，但它不能按内容长度条件生效：
	 * 容器查询比对的是「容器自身的尺寸」，与盒子里装什么无关，永远为真；
	 * 无条件加又会让短代码丢掉「宽度随内容」的气泡外观（实测 13px 的窄条）。
	 * 所以这个判断只能在这里做：量一次，超了就加属性，由 CSS 认属性收紧。
	 * 阈值取「超过所在列宽」——只有放不下的才需要换行。 */
	function markLongInlineCode() {
		Array.prototype.forEach.call(
			document.querySelectorAll("main :not(pre) > code"),
			function (code) {
				var host = code.closest("p, li, td, th, dd") || code.parentElement;
				var avail = host.getBoundingClientRect().width;
				if (avail > 0 && code.scrollWidth > avail) {
					code.setAttribute("data-block-long", "");
				}
			}
		);
	}

	function enhanceCode() {
		markLongInlineCode();
		Array.prototype.forEach.call(document.querySelectorAll("pre > code"), function (code) {
			var pre = code.parentElement;
			if (pre.classList.contains("mermaid")) return;
			// 已经手写在 .codeblock 里的（落地页为了首屏无 JS 也好看）：
			// 不再包一层（那样会出现两个标题栏），但要把高亮和复制按钮补上。
			var existing = pre.closest(".codeblock");
			if (existing) {
				code.innerHTML = highlight(code.textContent, lang);
				if (!existing.querySelector(".codeblock__copy")) {
					existing.querySelector(".codeblock__bar").appendChild(
						el('<button class="codeblock__copy" type="button">复制</button>')
					);
				}
				return;
			}

			var lang = (code.className.match(/language-([\w-]+)/) || [, ""])[1];
			code.innerHTML = highlight(code.textContent, lang);

			var block = document.createElement("div");
			block.className = "codeblock" + (lang === "bash" || lang === "sh" ? " codeblock--shell" : "");
			pre.parentElement.insertBefore(block, pre);
			block.appendChild(
				el(
					'<div class="codeblock__bar"><span class="codeblock__lang">' +
						escapeHTML(lang || "text") +
						'</span><button class="codeblock__copy" type="button">复制</button></div>'
				)
			);
			block.appendChild(pre);
		});

		document.addEventListener("click", function (e) {
			var btn = e.target.closest(".codeblock__copy");
			if (!btn) return;
			var code = btn.closest(".codeblock").querySelector("code");

			navigator.clipboard.writeText(code.textContent).then(
				function () {
					btn.textContent = "已复制";
					setTimeout(function () {
						btn.textContent = "复制";
					}, 1500);
				},
				function () {
					btn.textContent = "复制失败";
				}
			);
		});
	}

	/* ── 搜索 ─────────────────────────────────────────────────────────── */

	var search = { open: false, index: -1, hits: [] };

	function searchIndex() {
		return window.__MATEX_SEARCH || [];
	}

	function score(entry, terms) {
		var haystackTitle = (entry.title + " " + (entry.section || "")).toLowerCase();
		var haystackBody = (entry.text || "").toLowerCase();
		var total = 0;
		for (var i = 0; i < terms.length; i++) {
			var t = terms[i];
			var inTitle = haystackTitle.split(t).length - 1;
			var inBody = haystackBody.split(t).length - 1;
			if (!inTitle && !inBody) return 0;
			total += inTitle * 12 + Math.min(inBody, 6);
		}
		return total;
	}

	function snippet(text, terms) {
		var lower = text.toLowerCase();
		var at = -1;
		for (var i = 0; i < terms.length && at < 0; i++) at = lower.indexOf(terms[i]);
		if (at < 0) at = 0;
		var start = Math.max(0, at - 40);
		var out = (start > 0 ? "…" : "") + text.slice(start, start + 160) + (text.length > start + 160 ? "…" : "");
		return out.replace(/[&<>"]/g, function (c) {
			return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c];
		});
	}

	function mark(text, terms) {
		var out = text;
		terms.forEach(function (t) {
			if (t.length < 2) return;
			var re = new RegExp("(" + t.replace(/[.*+?^${}()|[\]\\]/g, "\\$&") + ")", "gi");
			out = out.replace(re, "<mark>$1</mark>");
		});
		return out;
	}

	function renderSearch(q) {
		var list = document.querySelector(".search__results");
		var terms = q.toLowerCase().split(/\s+/).filter(Boolean);
		if (!terms.length) {
			list.innerHTML = "";
			search.hits = [];
			search.index = -1;
			return;
		}

		search.hits = searchIndex()
			.map(function (e) {
				return { e: e, s: score(e, terms) };
			})
			.filter(function (x) {
				return x.s > 0;
			})
			.sort(function (a, b) {
				return b.s - a.s;
			})
			.slice(0, 12)
			.map(function (x) {
				return x.e;
			});

		search.index = search.hits.length ? 0 : -1;

		if (!search.hits.length) {
			list.innerHTML = '<li class="search__empty">没有匹配的页面。试试「熔断」「配置」「Kafka」。</li>';
			return;
		}

		list.innerHTML = search.hits
			.map(function (h, i) {
				return (
					'<li><a href="' +
					url(h.url) +
					'" aria-selected="' +
					(i === 0) +
					'"><span class="r-title">' +
					mark(escapeHTML(h.title), terms) +
					'<span class="r-section"> · ' +
					escapeHTML(h.section || "") +
					"</span></span>" +
					'<span class="r-text">' +
					mark(snippet(h.text, terms), terms) +
					"</span></a></li>"
				);
			})
			.join("");
	}

	function openSearch() {
		var modal = document.querySelector(".search");
		if (!modal) return;
		modal.hidden = false;
		search.open = true;
		var input = modal.querySelector(".search__input");
		input.value = "";
		renderSearch("");
		input.focus();
	}

	function closeSearch() {
		var modal = document.querySelector(".search");
		if (modal) modal.hidden = true;
		search.open = false;
	}

	function moveSelection(delta) {
		if (!search.hits.length) return;
		search.index = (search.index + delta + search.hits.length) % search.hits.length;
		Array.prototype.forEach.call(document.querySelectorAll(".search__results a"), function (a, i) {
			a.setAttribute("aria-selected", String(i === search.index));
			if (i === search.index) a.scrollIntoView({ block: "nearest" });
		});
	}

	function buildSearch() {
		document.body.appendChild(
			el(
				'<div class="search" hidden role="dialog" aria-modal="true" aria-label="搜索文档">' +
					'<div class="search__panel">' +
					'<input class="search__input" type="search" placeholder="搜索文档…" aria-label="搜索关键词" autocomplete="off">' +
					'<ul class="search__results" role="listbox"></ul>' +
					'<div class="search__hint"><span>↑ ↓ 选择</span><span>Enter 打开</span><span>Esc 关闭</span></div>' +
					"</div></div>"
			)
		);

		document.addEventListener("click", function (e) {
			if (e.target.closest("[data-search-open]")) {
				e.preventDefault();
				openSearch();
			}
		});
		document.addEventListener("input", function (e) {
			if (e.target.classList.contains("search__input")) renderSearch(e.target.value);
		});
		document.addEventListener("keydown", function (e) {
			var modal = document.querySelector(".search");
			if (search.open) {
				if (e.key === "Escape") closeSearch();
				if (e.key === "ArrowDown") {
					e.preventDefault();
					moveSelection(1);
				}
				if (e.key === "ArrowUp") {
					e.preventDefault();
					moveSelection(-1);
				}
				if (e.key === "Enter" && search.index >= 0) {
					location.href = url(search.hits[search.index].url);
				}
				return;
			}
			if (e.key === "/" && !/^(INPUT|TEXTAREA|SELECT)$/.test(e.target.tagName)) {
				e.preventDefault();
				openSearch();
			}
		});
		modal: {
			var m = document.querySelector(".search");
			if (m) m.addEventListener("click", function (e) {
				if (e.target === m) closeSearch();
			});
		}
	}

	/* ── Mermaid ─────────────────────────────────────────────────────── */

	function initMermaid() {
		var blocks = document.querySelectorAll("pre.mermaid");
		if (!blocks.length || typeof window.mermaid === "undefined") return;
		window.mermaid.initialize({
			startOnLoad: false,
			securityLevel: "strict",
			theme: "base",
			fontFamily: "ui-sans-serif, system-ui, -apple-system, 'PingFang SC', sans-serif",
			themeVariables: {
				background: "#ffffff",
				primaryColor: "#fdf6f0",
				primaryTextColor: "#2b2722",
				primaryBorderColor: "#c8641f",
				lineColor: "#8a8378",
				secondaryColor: "#f5f2ee",
				tertiaryColor: "#faf8f5",
				fontSize: "14px"
			}
		});
		window.mermaid.run({ nodes: blocks });
	}

	/* ── 侧边栏 / 顶栏交互 ───────────────────────────────────────────── */

	function wireToggles() {
		var toggle = document.querySelector(".navtoggle");
		var nav = document.querySelector(".topnav");
		if (toggle && nav) {
			toggle.addEventListener("click", function () {
				var open = nav.hidden;
				nav.hidden = !open;
				toggle.setAttribute("aria-expanded", String(open));
			});
		}

		var side = document.querySelector(".sidebar");
		if (side && window.matchMedia("(max-width: 59.99rem)").matches) {
			side.hidden = true;
			var btn = document.createElement("button");
			btn.type = "button";
			btn.className = "btn btn--ghost";
			btn.textContent = "显示目录";
			btn.style.marginBlockEnd = "1rem";
			btn.addEventListener("click", function () {
				side.hidden = !side.hidden;
				btn.textContent = side.hidden ? "显示目录" : "隐藏目录";
			});
			side.parentElement.insertBefore(btn, side);
		}
	}

	function wireTOCScroll() {
		var links = document.querySelectorAll(".toc a");
		if (!links.length || !("IntersectionObserver" in window)) return;
		var targets = [];
		Array.prototype.forEach.call(links, function (a) {
			var id = a.getAttribute("href").slice(1);
			var node = document.getElementById(id);
			if (node) targets.push({ link: a, node: node });
		});
		var obs = new IntersectionObserver(
			function (entries) {
				entries.forEach(function (entry) {
					if (!entry.isIntersecting) return;
					targets.forEach(function (t) {
						t.link.setAttribute("aria-current", String(t.node === entry.target));
					});
				});
			},
			{ rootMargin: "-72px 0px -70% 0px" }
		);
		targets.forEach(function (t) {
			obs.observe(t.node);
		});
	}

	/* ── 启动 ─────────────────────────────────────────────────────────── */

	function boot() {
		if (document.querySelector(".topbar")) return; // 已经装过（不应发生）

		var main = document.querySelector("main");
		if (!main) return;

		decorateMain(main);

		// 落地页是全宽的，不要侧边栏与 TOC；文档页才有三栏。
		if (pageId !== "index") {
			var shell = document.createElement("div");
			shell.className = "shell" + (pageId.indexOf("guide/") === 0 ? " shell--wide" : "");
			main.parentElement.insertBefore(shell, main);
			shell.appendChild(buildSidebar());
			shell.appendChild(main);

			var toc = buildTOC(main);
			if (toc) shell.appendChild(toc);
		}

		var pager = buildPager();
		if (pager) main.appendChild(pager);

		document.body.insertBefore(buildHeader(), document.body.firstChild);
		document.body.appendChild(buildFooter());

		enhanceCode();
		buildSearch();
		wireToggles();
		wireTOCScroll();
		initMermaid();
	}

	if (document.readyState === "loading") {
		document.addEventListener("DOMContentLoaded", boot);
	} else {
		boot();
	}

	// 导出一小部分供页面内联脚本复用（例如 landing 的统计数字无需导出，
	// 但工具脚本可以复用 escapeHTML）。
	window.matexSite = { escapeHTML: escapeHTML, icon: { check: ICON_CHECK } };
})();
