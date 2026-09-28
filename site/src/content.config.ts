import { defineCollection } from 'astro:content';
import { glob } from 'astro/loaders';
import { z } from 'astro/zod';

const docs = defineCollection({
	loader: glob({ pattern: '**/*.{md,mdx}', base: './src/content/docs' }),
	schema: z
		.object({
			title: z.string(),
			description: z.string().optional(),
			sidebar: z
				.object({ order: z.number().optional(), label: z.string().optional() })
				.passthrough()
				.optional(),
			tableOfContents: z
				.union([z.boolean(), z.object({ minHeadingLevel: z.number().optional(), maxHeadingLevel: z.number().optional() })])
				.optional(),
		})
		.passthrough(),
});

export const collections = { docs };
