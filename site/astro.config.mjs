// @ts-check
import { defineConfig } from 'astro/config';
import starlight from '@astrojs/starlight';
import sitemap from '@astrojs/sitemap';

const site = 'https://pillar-csi.bhyoo.com';
const repo = 'https://github.com/isac322/pillar-csi';
// Cloudflare Web Analytics token, read from the build environment. When it is
// empty, no beacon is emitted. src/pages/index.astro reads the same variable.
const cfBeaconToken = process.env.PUBLIC_CF_WEB_ANALYTICS_TOKEN?.trim();

export default defineConfig({
	site,
	trailingSlash: 'ignore',
	integrations: [
		starlight({
			title: 'pillar-csi',
			description:
				'Kubernetes CSI driver that exports ZFS zvols and LVM volumes from your storage nodes to pods over NVMe-oF/TCP.',
			favicon: '/favicon.svg',
			social: [{ icon: 'github', label: 'GitHub', href: repo }],
			editLink: { baseUrl: `${repo}/edit/master/site/` },
			expressiveCode: { styleOverrides: { borderRadius: '2px' } },
			customCss: [
				'@fontsource/inter-tight/600.css',
				'@fontsource/inter-tight/700.css',
				'@fontsource/inter/400.css',
				'@fontsource/inter/600.css',
				'@fontsource/jetbrains-mono/400.css',
				'@fontsource/jetbrains-mono/600.css',
				'./src/styles/tokens.css',
				'./src/styles/starlight.css',
			],
			logo: {
				dark: './public/brand/logo.svg',
				light: './public/brand/logo-light.svg',
				alt: 'pillar-csi',
				replacesTitle: true,
			},
			components: {
				ThemeProvider: './src/components/starlight/ThemeProvider.astro',
			},
			head: [
				{ tag: 'meta', attrs: { property: 'og:image', content: `${site}/og.png` } },
				{ tag: 'meta', attrs: { property: 'og:image:width', content: '1200' } },
				{ tag: 'meta', attrs: { property: 'og:image:height', content: '630' } },
				{ tag: 'meta', attrs: { name: 'twitter:image', content: `${site}/og.png` } },
				{ tag: 'meta', attrs: { name: 'theme-color', content: '#0A0F1D' } },
				{ tag: 'link', attrs: { rel: 'apple-touch-icon', href: '/apple-touch-icon.png' } },
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
