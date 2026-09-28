// @ts-check
import { defineConfig } from 'astro/config';
import starlight from '@astrojs/starlight';
import sitemap from '@astrojs/sitemap';
import rehypeDocsTokens from './src/plugins/rehype-docs-tokens.mjs';

const site = 'https://pillar-csi.bhyoo.com';
const repo = 'https://github.com/isac322/pillar-csi';
// Cloudflare Web Analytics token, read from the build environment. When it is
// empty, no beacon is emitted. src/pages/index.astro reads the same variable.
const cfBeaconToken = process.env.PUBLIC_CF_WEB_ANALYTICS_TOKEN?.trim();

/**
 * Expressive Code plugin that keeps code-frame chrome and ASCII diagrams out of
 * the Pagefind index. Frame headers carry screen-reader labels such as
 * "Terminal window" that otherwise show up in search excerpts. A `text` block
 * is treated as a diagram when it has box-drawing pipes and arrows; any block
 * can also opt out with the `search=false` meta option.
 * @type {import('@astrojs/starlight/expressive-code').ExpressiveCodePlugin}
 */
const pagefindIgnoreChrome = {
	name: 'pillar-pagefind-ignore',
	hooks: {
		postprocessRenderedBlock: ({ codeBlock, renderData }) => {
			const code = codeBlock.code;
			const isDiagram =
				codeBlock.metaOptions.getBoolean('search') === false ||
				(['text', 'txt', 'plaintext'].includes(codeBlock.language) &&
					/^\s*\|(\s|$)/m.test(code) &&
					/->|-->|<-/.test(code));
			/** @param {any} node */
			const visit = (node) => {
				if (node.type !== 'element') return;
				if (isDiagram && node === renderData.blockAst) node.properties.dataPagefindIgnore = '';
				if (node.tagName === 'figcaption') node.properties.dataPagefindIgnore = '';
				node.children?.forEach(visit);
			};
			visit(renderData.blockAst);
		},
	},
};

export default defineConfig({
	site,
	trailingSlash: 'ignore',
	// Keep `--flag`, straight quotes and "..." exactly as written; typographic
	// replacement turns CLI flags into en dashes that fail when copied.
	markdown: { smartypants: false, rehypePlugins: [rehypeDocsTokens] },
	integrations: [
		starlight({
			title: 'pillar-csi',
			description:
				'Kubernetes CSI driver that exports ZFS zvols and LVM volumes from your storage nodes to pods over NVMe-oF/TCP.',
			favicon: '/favicon.svg',
			social: [{ icon: 'github', label: 'GitHub', href: repo }],
			editLink: { baseUrl: `${repo}/edit/master/site/` },
			expressiveCode: {
				styleOverrides: { borderRadius: '2px' },
				plugins: [pagefindIgnoreChrome],
			},
			customCss: [
				'@fontsource/inter-tight/600.css',
				'@fontsource/inter-tight/700.css',
				'@fontsource/inter/400.css',
				'@fontsource/inter/600.css',
				'@fontsource/jetbrains-mono/400.css',
				'@fontsource/jetbrains-mono/600.css',
				'./src/styles/tokens.css',
				'./src/styles/starlight.css',
				'./src/styles/scroll.css',
			],
			logo: {
				dark: './public/brand/logo.svg',
				light: './public/brand/logo-light.svg',
				alt: 'pillar-csi',
				replacesTitle: true,
			},
			components: {
				ThemeProvider: './src/components/starlight/ThemeProvider.astro',
				SocialIcons: './src/components/starlight/SocialIcons.astro',
				Search: './src/components/starlight/Search.astro',
			},
			head: [
				{ tag: 'meta', attrs: { property: 'og:image', content: `${site}/og.png` } },
				{ tag: 'meta', attrs: { property: 'og:image:width', content: '1200' } },
				{ tag: 'meta', attrs: { property: 'og:image:height', content: '630' } },
				{ tag: 'meta', attrs: { name: 'twitter:image', content: `${site}/og.png` } },
				{ tag: 'meta', attrs: { name: 'theme-color', content: '#0A0F1D' } },
				{ tag: 'link', attrs: { rel: 'apple-touch-icon', href: '/apple-touch-icon.png' } },
				{ tag: 'script', attrs: { src: '/scroll-hint.js', defer: true } },
				{ tag: 'script', attrs: { src: '/star.js', defer: true } },
				...(cfBeaconToken
					? [
							{
								tag: /** @type {const} */ ('script'),
								attrs: {
									defer: true,
									src: 'https://static.cloudflareinsights.com/beacon.min.js',
									'data-cf-beacon': JSON.stringify({ token: cfBeaconToken }),
								},
							},
						]
					: []),
			],
			sidebar: [
				{ label: 'Overview', link: '/docs/' },
				{ label: 'Tutorials', items: [{ autogenerate: { directory: 'docs/tutorials' } }] },
				{ label: 'How-to guides', items: [{ autogenerate: { directory: 'docs/how-to' } }] },
				{ label: 'Reference', items: [{ autogenerate: { directory: 'docs/reference' } }] },
				{ label: 'Explanation', items: [{ autogenerate: { directory: 'docs/explanation' } }] },
				{ label: 'Community', items: [{ autogenerate: { directory: 'docs/community' } }] },
			],
		}),
		sitemap(),
	],
});
