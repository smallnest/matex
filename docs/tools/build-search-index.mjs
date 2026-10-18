/* 生成 docs/assets/search-index.js —— 站点离线搜索的静态索引。
 *
 * 用法（在仓库根目录）：
 *   node docs/tools/build-search-index.mjs
 *
 * 为什么是"多关键词子串匹配"而不是分词：中文分词要引词典并维护停用词，
 * 而本站只有几十个页面——索引和查询都在浏览器里跑，子串匹配已经够用，
 * 而且零依赖、零构建。文档量级真的上去时，替换的只有 search-index.js 的
 * 格式与 site.js 里的 score()，页面不用动。
 */
import { readdir, readFile, writeFile, stat } from "node:fs/promises";
import { join, relative, dirname } from "node:path";
import { fileURLToPath } from "node:url";

const HERE = dirname(fileURLToPath(import.meta.url));
const DOCS = join(HERE, "..");

/** 页面在站点里的 id（无 .html 后缀），site.js 用它拼相对链接。 */
function pageId(file) {
	return relative(DOCS, file).replace(/\.html$/, "").replace(/\\/g, "/");
}

async function* walk(dir) {
	for (const entry of await readdir(dir, { withFileTypes: true })) {
		if (entry.name === "assets" || entry.name === "tools" || entry.name.startsWith("_")) continue;
		const full = join(dir, entry.name);
		if (entry.isDirectory()) yield* walk(full);
		else if (entry.name.endsWith(".html")) yield full;
	}
}

const strip = (s) =>
	s
		.replace(/<script[\s\S]*?<\/script>/gi, " ")
		.replace(/<style[\s\S]*?<\/style>/gi, " ")
		.replace(/<[^>]+>/g, " ")
		.replace(/&amp;/g, "&")
		.replace(/&lt;/g, "<")
		.replace(/&gt;/g, ">")
		.replace(/&quot;/g, '"')
		.replace(/&#\d+;/g, " ")
		.replace(/\s+/g, " ")
		.trim();

/** 按 <h2>/<h3> 切段，让命中能定位到小节而不是整页。 */
function sections(html) {
	const title = (html.match(/<h1[^>]*>([\s\S]*?)<\/h1>/i) || [, ""])[1];
	const body = html.replace(/<h1[\s\S]*?<\/h1>/i, "");
	const parts = body.split(/(?=<h[23][^>]*>)/i);
	const out = [];
	for (const part of parts) {
		const heading = (part.match(/<h[23][^>]*>([\s\S]*?)<\/h[23]>/i) || [, ""])[1];
		const text = strip(part);
		if (text.length < 40) continue;
		out.push({ section: strip(heading), text });
	}
	return { title: strip(title), sections: out };
}

const index = [];
for await (const file of walk(DOCS)) {
	const html = await readFile(file, "utf8");
	const { title, sections: secs } = sections(html);
	if (!title) continue;
	if (!secs.length) {
		index.push({ url: pageId(file), title, section: "", text: strip(html) });
		continue;
	}
	for (const s of secs) {
		index.push({ url: pageId(file), title, section: s.section, text: s.text });
	}
}

index.sort((a, b) => (a.url === b.url ? 0 : a.url < b.url ? -1 : 1));

const banner =
	"/* 由 docs/tools/build-search-index.mjs 生成，请勿手工编辑。 */\n" +
	"window.__MATEX_SEARCH = ";
await writeFile(
	join(DOCS, "assets", "search-index.js"),
	banner + JSON.stringify(index) + ";\n",
	"utf8"
);

const pages = new Set(index.map((e) => e.url)).size;
console.error(`search index: ${index.length} 段 / ${pages} 页`);
void stat;
