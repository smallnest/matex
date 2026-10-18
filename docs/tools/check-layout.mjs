/* 布局自检：在四档视口下检查整站有没有横向溢出。
 *
 * 用法（先起本地站点）：
 *   python3 -m http.server -d docs 8899
 *   node docs/tools/check-layout.mjs [http://localhost:8899]
 *
 * check-site.mjs 看的是静态文本（断链、契约），看不到布局；
 * 这个脚本真的开一个无头浏览器，按 360/414/768/1024 四档宽度逐页量
 * documentElement.scrollWidth——只要大于视口宽度，就说明有元素把页面撑宽了，
 * 并把它所在页与溢出元素报出来。
 *
 * 它需要 puppeteer 与 chrome-headless-shell，所以不进 CI（CI 里没有浏览器）；
 * 改完 CSS 或正文后本地跑一次。
 */
import { createRequire } from "node:module";
import { homedir } from "node:os";
import { readdirSync } from "node:fs";
import { execSync } from "node:child_process";
import { join, relative, dirname } from "node:path";
import { fileURLToPath } from "node:url";

const HERE = dirname(fileURLToPath(import.meta.url));
const DOCS = join(HERE, "..");
const ORIGIN = process.argv[2] || "http://localhost:8899";
const WIDTHS = [360, 414, 768, 1024];

/* puppeteer 不是仓库依赖（站点本身零依赖），所以从几个已知位置找。
   找不到就报清楚该怎么装，而不是丢一个 MODULE_NOT_FOUND 栈。 */
function loadPuppeteer() {
	const roots = [DOCS, "/tmp/mermaid-fetch", process.cwd()];
	for (const root of roots) {
		try {
			const req = createRequire(join(root, "noop.js"));
			return req("puppeteer");
		} catch {
			/* 换下一个位置 */
		}
	}
	console.error("找不到 puppeteer。先安装再跑：");
	console.error("  npm i -g puppeteer        # 或 mkdir -p /tmp/pup && cd /tmp/pup && npm i puppeteer");
	process.exit(2);
}

const puppeteer = loadPuppeteer();

/* chrome-headless-shell 由 puppeteer 缓存，目录名两层都带平台与版本，取最新一个。
   找不到缓存就直接让 puppeteer 用它自带的浏览器（装了 puppeteer 就有）。 */
function chromePath() {
	const cacheVer = process.arch === "arm64" ? "mac_arm" : "mac";
	const binName = process.arch === "arm64" ? "chrome-headless-shell-mac-arm64" : "chrome-headless-shell-mac-x64";
	const base = join(homedir(), ".cache/puppeteer/chrome-headless-shell");
	const versions = readdirSync(base, { withFileTypes: true })
		.filter((e) => e.isDirectory() && e.name.startsWith(cacheVer))
		.map((e) => e.name)
		.sort();
	if (!versions.length) return undefined;
	return join(base, versions.pop(), binName, "chrome-headless-shell");
}

const files = execSync(`find ${DOCS} -name '*.html' -not -path '*/assets/*' -not -name '_*'`)
	.toString()
	.trim()
	.split("\n");

const exe = chromePath();
const browser = await puppeteer.launch({ executablePath: exe, headless: true, args: ["--no-sandbox"] });
const problems = [];

for (const width of WIDTHS) {
	const page = await browser.newPage();
	await page.setViewport({ width, height: 900 });
	for (const file of files) {
		const rel = relative(DOCS, file);
		await page.goto(`${ORIGIN}/${rel === "index.html" ? "" : rel}`, { waitUntil: "networkidle0" });
		await new Promise((r) => setTimeout(r, 150));
		const r = await page.evaluate((w) => {
			const doc = document.documentElement;
			const over = [];
			/* 溢出元素只看可见的：被裁掉的定位元素不算。 */
			doc.querySelectorAll("body *").forEach((el) => {
				const b = el.getBoundingClientRect();
				if (b.width > 0 && (b.right > w + 1 || b.left < -1)) {
					over.push(el.tagName.toLowerCase() + (el.className ? "." + String(el.className).slice(0, 30) : ""));
				}
			});
			const main = doc.querySelector("main");
			return {
				sw: doc.scrollWidth,
				over: over.slice(0, 3),
				mw: main ? Math.round(main.getBoundingClientRect().width) : 0,
			};
		}, width);
		if (r.sw > width + 1) problems.push(`${width}px ${rel}: scrollWidth=${r.sw} [${r.over.join(", ")}]`);
		/* 正文被挤到 200px 以下说明外壳抢了宽度。 */
		if (r.mw < 200) problems.push(`${width}px ${rel}: main width ${r.mw}px`);
	}
	await page.close();
}

await browser.close();
console.error(`${WIDTHS.length * files.length} 次量测，${problems.length} 个问题`);
for (const p of problems) console.error("  - " + p);
process.exit(problems.length ? 1 : 0);
