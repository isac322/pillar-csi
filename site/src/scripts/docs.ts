const root = document.documentElement;

/* Theme */
const themeBtn = document.querySelector<HTMLButtonElement>('[data-theme-toggle]');
const syncThemeLabel = () =>
	themeBtn?.setAttribute('aria-label', root.dataset.theme === 'dark' ? 'Switch to light theme' : 'Switch to dark theme');
syncThemeLabel();
themeBtn?.addEventListener('click', () => {
	const next = root.dataset.theme === 'dark' ? 'light' : 'dark';
	root.dataset.theme = next;
	localStorage.setItem('pcsi-theme', next);
	syncThemeLabel();
});

/* Navigation drawer (narrow screens) */
const menuBtn = document.querySelector<HTMLButtonElement>('[data-menu]');
const scrim = document.querySelector<HTMLElement>('[data-scrim]');
function setMenu(open: boolean) {
	document.body.classList.toggle('nav-open', open);
	menuBtn?.setAttribute('aria-expanded', String(open));
	menuBtn?.setAttribute('aria-label', open ? 'Close navigation' : 'Open navigation');
	if (scrim) scrim.hidden = !open;
}
menuBtn?.addEventListener('click', () => setMenu(!document.body.classList.contains('nav-open')));
scrim?.addEventListener('click', () => setMenu(false));
document.querySelector('.d-nav .active')?.scrollIntoView({ block: 'center' });

/* Tabs, synced by syncKey across the page and visits */
const TAB_KEY = 'pcsi-tabs';
const tabStore: Record<string, string> = JSON.parse(localStorage.getItem(TAB_KEY) ?? '{}');
const tabGroups = [...document.querySelectorAll<HTMLElement>('[data-tabs]')];

function selectTab(group: HTMLElement, label: string, focus = false) {
	const tabs = [...group.querySelectorAll<HTMLButtonElement>(':scope > .tab-list > [role="tab"]')];
	const panels = [...group.querySelectorAll<HTMLElement>(':scope > .tab-panels > .tab-panel')];
	const idx = tabs.findIndex((t) => t.dataset.label === label);
	if (idx < 0) return;
	tabs.forEach((t, i) => {
		t.setAttribute('aria-selected', String(i === idx));
		t.tabIndex = i === idx ? 0 : -1;
	});
	panels.forEach((p, i) => (p.hidden = i !== idx));
	if (focus) tabs[idx].focus();
}

for (const group of tabGroups) {
	const tabs = [...group.querySelectorAll<HTMLButtonElement>(':scope > .tab-list > [role="tab"]')];
	const panels = [...group.querySelectorAll<HTMLElement>(':scope > .tab-panels > .tab-panel')];
	panels.forEach((p, i) => {
		const tab = tabs[i];
		if (!tab) return;
		p.id = tab.getAttribute('aria-controls') ?? '';
		p.setAttribute('aria-labelledby', tab.id);
		p.tabIndex = 0;
	});
	group.classList.add('is-ready');
	const key = group.dataset.syncKey;
	selectTab(group, (key && tabStore[key]) || tabs[0]?.dataset.label || '');

	group.querySelector('.tab-list')?.addEventListener('click', (e) => {
		const tab = (e.target as HTMLElement).closest<HTMLButtonElement>('[role="tab"]');
		if (!tab?.dataset.label) return;
		const label = tab.dataset.label;
		if (!key) return selectTab(group, label);
		// Keep the clicked group fixed on screen while synced groups above it change height.
		const before = group.getBoundingClientRect().top;
		tabStore[key] = label;
		localStorage.setItem(TAB_KEY, JSON.stringify(tabStore));
		for (const g of tabGroups) if (g.dataset.syncKey === key) selectTab(g, label);
		window.scrollBy(0, group.getBoundingClientRect().top - before);
	});
	group.querySelector('.tab-list')?.addEventListener('keydown', (e) => {
		const ev = e as KeyboardEvent;
		const cur = tabs.findIndex((t) => t.getAttribute('aria-selected') === 'true');
		const step = ev.key === 'ArrowRight' ? 1 : ev.key === 'ArrowLeft' ? -1 : 0;
		if (!step) return;
		ev.preventDefault();
		const nextTab = tabs[(cur + step + tabs.length) % tabs.length];
		nextTab.click();
		nextTab.focus();
	});
}

/* Copy buttons on code blocks */
document.addEventListener('click', async (e) => {
	const btn = (e.target as HTMLElement).closest<HTMLButtonElement>('[data-copy]');
	if (!btn) return;
	const code = btn.closest('figure')?.querySelector('pre')?.innerText ?? '';
	try {
		await navigator.clipboard.writeText(code.replace(/\n$/, ''));
		btn.textContent = 'Copied';
	} catch {
		btn.textContent = 'Press Ctrl+C';
	}
	btn.classList.add('is-done');
	setTimeout(() => {
		btn.textContent = 'Copy';
		btn.classList.remove('is-done');
	}, 1600);
});

/* Table of contents: mark the section in view */
const tocLinks = new Map([...document.querySelectorAll<HTMLAnchorElement>('[data-toc-link]')].map((a) => [a.dataset.tocLink!, a]));
if (tocLinks.size) {
	const heads = [...tocLinks.keys()].map((id) => document.getElementById(id)).filter(Boolean) as HTMLElement[];
	const onScroll = () => {
		let current = heads[0];
		for (const h of heads) if (h.getBoundingClientRect().top < 140) current = h;
		tocLinks.forEach((a, id) => a.classList.toggle('is-current', id === current?.id));
	};
	addEventListener('scroll', onScroll, { passive: true });
	onScroll();
}

/* Search (Pagefind) */
type PagefindResult = { data: () => Promise<{ url: string; excerpt: string; meta: { title?: string } }> };
type Pagefind = { search: (q: string) => Promise<{ results: PagefindResult[] }>; init?: () => Promise<void> };

const dialog = document.querySelector<HTMLDialogElement>('[data-search]');
const input = document.querySelector<HTMLInputElement>('[data-search-input]');
const list = document.querySelector<HTMLOListElement>('[data-search-results]');
const status = document.querySelector<HTMLElement>('[data-search-status]');
let pagefind: Pagefind | null = null;
let seq = 0;

async function getPagefind(): Promise<Pagefind | null> {
	if (pagefind) return pagefind;
	try {
		// The Pagefind bundle is generated into dist after `astro build`, so it cannot be a static import.
		const url = '/pagefind/pagefind.js';
		pagefind = (await import(/* @vite-ignore */ url)) as Pagefind;
		await pagefind.init?.();
	} catch {
		pagefind = null;
	}
	return pagefind;
}

function openSearch() {
	if (!dialog || dialog.open) return;
	dialog.showModal();
	input?.focus();
	input?.select();
	getPagefind();
}

document.querySelector('[data-search-open]')?.addEventListener('click', openSearch);
document.querySelector('[data-search-close]')?.addEventListener('click', () => dialog?.close());
dialog?.addEventListener('click', (e) => {
	if (e.target === dialog) dialog.close();
});
addEventListener('keydown', (e) => {
	const t = e.target as HTMLElement;
	const typing = t.closest('input, textarea, [contenteditable="true"]');
	if ((e.key === '/' && !typing) || ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'k')) {
		e.preventDefault();
		openSearch();
	}
	if (e.key === 'Escape' && document.body.classList.contains('nav-open')) setMenu(false);
});

input?.addEventListener('input', async () => {
	const q = input.value.trim();
	const mine = ++seq;
	if (!list || !status) return;
	if (!q) {
		list.replaceChildren();
		status.textContent = 'Type to search every page.';
		return;
	}
	const pf = await getPagefind();
	if (!pf) {
		status.textContent = 'Search is unavailable in this build. Run npm run build to create the index.';
		return;
	}
	const { results } = await pf.search(q);
	const data = await Promise.all(results.slice(0, 8).map((r) => r.data()));
	if (mine !== seq) return;
	status.textContent = data.length ? `${results.length} ${results.length === 1 ? 'page' : 'pages'} match "${q}".` : `No pages match "${q}". Try a module name, CRD kind, or Helm value.`;
	list.replaceChildren(
		...data.map((d) => {
			const li = document.createElement('li');
			const a = document.createElement('a');
			a.href = d.url;
			const title = document.createElement('span');
			title.className = 'sr-title';
			title.textContent = d.meta.title ?? d.url;
			const ex = document.createElement('span');
			ex.className = 'sr-excerpt';
			ex.innerHTML = d.excerpt;
			a.append(title, ex);
			li.append(a);
			return li;
		}),
	);
});
