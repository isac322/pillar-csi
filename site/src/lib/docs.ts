import { getCollection, type CollectionEntry } from 'astro:content';

export type DocEntry = CollectionEntry<'docs'>;

const SECTIONS = [
	{ key: 'tutorials', label: 'Tutorials' },
	{ key: 'how-to', label: 'How-to guides' },
	{ key: 'reference', label: 'Reference' },
	{ key: 'explanation', label: 'Explanation' },
	{ key: 'community', label: 'Community' },
] as const;

/** `docs/how-to/install-helm` → `how-to/install-helm`; the docs index → undefined. */
export function slugOf(entry: DocEntry): string | undefined {
	const rel = (entry.filePath ?? '').replace(/^.*src\/content\/docs\/docs\//, '').replace(/\.(md|mdx)$/, '');
	return rel === 'index' ? undefined : rel;
}

export function hrefOf(entry: DocEntry): string {
	const slug = slugOf(entry);
	return slug ? `/docs/${slug}/` : '/docs/';
}

export interface NavItem {
	label: string;
	href: string;
}
export interface NavGroup {
	label: string;
	items: NavItem[];
}

export async function loadDocs() {
	const all = await getCollection('docs');
	const order = (e: DocEntry) => e.data.sidebar?.order ?? 999;
	const groups: NavGroup[] = SECTIONS.map(({ key, label }) => ({
		label,
		items: all
			.filter((e) => slugOf(e)?.startsWith(`${key}/`))
			.sort((a, b) => order(a) - order(b) || a.data.title.localeCompare(b.data.title))
			.map((e) => ({ label: e.data.sidebar?.label ?? e.data.title, href: hrefOf(e) })),
	})).filter((g) => g.items.length);
	const flat: NavItem[] = [{ label: 'Overview', href: '/docs/' }, ...groups.flatMap((g) => g.items)];
	return { all, groups, flat };
}
