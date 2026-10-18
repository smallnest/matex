/* 站点自检：断链、外部资源、页面契约。
 *
 * 用法（仓库根目录）：
 *   node docs/tools/check-site.mjs
 *
 * 检查三件事：
 *   1. 每个 HTML 的 href/src 指向的文件是否存在（相对路径按页面所在目录解析）；
 *   2. 页面没有引用外部运行时资源（CDN 字体、外部脚本）——站点必须能离线打开；
 *   3. 每页都满足页面契约：有 <main id="main">、data-page、三份 assets 的 script、
 *      以及 title/description。
 * 退出码非 0 表示有问题，可直接用在 CI 里。
 */
import { readdir, readFile } from "node:fs/promises";
import { existsSync } from "node:fs";
import { join, dirname, relative, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const HERE = dirname(fileURLToPath(import.meta.url));
const DOCS = join(HERE, "..");

const problems = [];
const note = (file, msg) => problems.push(`${relative(DOCS, file)}: ${msg}`);

async function* walk(dir) {
	for (const entry of await readdir(dir, { withFileTypes: true })) {
		if (entry.name === "assets" || entry.name === "tools") continue;
		const full = join(dir, entry.name);
		if (entry.isDirectory()) yield* walk(full);
		else if (entry.name.endsWith(".html") && !entry.name.startsWith("_")) yield full;
	}
}

const requiredScripts = ["assets/mermaid.min.js", "assets/search-index.js", "assets/site.js"];

let pages = 0;
for await (const file of walk(DOCS)) {
	pages++;
	const html = await readFile(file, "utf8");
	const dir = dirname(file);

	if (!/<main id="main"/.test(html)) note(file, "missing <main id=\"main\">");
	if (!/<body data-page="/.test(html)) note(file, "missing <body data-page=\"...\">");
	if (!/<title>[^<]+<\/title>/.test(html)) note(file, "missing <title>");
	// 属性可能换行书写，所以匹配跨行。
	if (!/<meta\s+name="description"/.test(html)) note(file, "missing meta description");

	// 三份 asset script 必须齐全（顺序无关）
	for (const s of requiredScripts) {
		if (!html.includes(s)) note(file, `missing script ${s}`);
	}

	// 外部运行时资源（字体/样式/脚本）一律不允许
	for (const m of html.matchAll(/(?:src|href)="(https?:\/\/[^"]+)"/g)) {
		const url = m[1];
		if (url.startsWith("https://github.com/smallnest/matex")) continue; // 站外链接可以，但不该是资源
		if (/\.(js|css|woff2?|ttf)$/i.test(url)) note(file, `external runtime asset: ${url}`);
	}

	// 站内引用必须存在
	for (const m of html.matchAll(/(?:src|href)="([^"#][^"]*)"/g)) {
		const ref = m[1];
		if (/^(https?:|mailto:|data:)/.test(ref)) continue;
		const target = ref.split("#")[0].split("?")[0];
		if (!target) continue;
		const abs = resolve(dir, target);
		const asDir = resolve(dir, target, "index.html");
		if (!existsSync(abs) && !existsSync(asDir)) {
			note(file, `broken link: ${ref}`);
		}
	}
}

console.error(`checked ${pages} pages, ${problems.length} problem(s)`);
for (const p of problems) console.error("  - " + p);
process.exit(problems.length ? 1 : 0);
