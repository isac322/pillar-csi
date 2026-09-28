import { defineConfig } from 'astro/config';
import { fileURLToPath } from 'node:url';
import mdx from '@astrojs/mdx';
import sitemap from '@astrojs/sitemap';
import remarkDirective from 'remark-directive';
import rehypeSlug from 'rehype-slug';
import rehypeAutolinkHeadings from 'rehype-autolink-headings';
import remarkAsides from './src/lib/remark-asides.mjs';
import rehypeDocBlocks from './src/lib/rehype-doc-blocks.mjs';
import vellum from './src/lib/shiki-vellum.json' with { type: 'json' };

export default defineConfig({
	site: 'https://pillar-csi.bhyoo.com',
	trailingSlash: 'always',
	build: { format: 'directory' },
	integrations: [mdx(), sitemap()],
	markdown: {
		shikiConfig: { theme: vellum, wrap: false },
		remarkPlugins: [remarkDirective, remarkAsides],
		rehypePlugins: [
			rehypeSlug,
			[
				rehypeAutolinkHeadings,
				{
					behavior: 'append',
					properties: { className: ['heading-anchor'], ariaLabel: 'Link to this section' },
					content: [],
					test: ['h2', 'h3', 'h4'],
				},
			],
			rehypeDocBlocks,
		],
	},
	vite: {
		resolve: {
			alias: {
				'@astrojs/starlight/components': fileURLToPath(new URL('./src/components/docs/index.ts', import.meta.url)),
			},
		},
	},
});
