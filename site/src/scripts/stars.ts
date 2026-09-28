import { REPO, STAR_THRESHOLD } from '../lib/site';

const KEY = 'pcsi-stars';
const TTL = 60 * 60 * 1000;

function format(n: number): string {
	return n >= 10000 ? `${(n / 1000).toFixed(1).replace(/\.0$/, '')}k` : n.toLocaleString('en-US');
}

function show(n: number) {
	if (!Number.isFinite(n) || n < STAR_THRESHOLD) return;
	const text = format(n);
	for (const el of document.querySelectorAll<HTMLElement>('[data-star-count]')) {
		el.textContent = text;
		el.hidden = false;
		el.setAttribute('aria-label', `${n} stars`);
	}
}

async function load() {
	try {
		const cached = JSON.parse(sessionStorage.getItem(KEY) ?? 'null');
		if (cached && Date.now() - cached.t < TTL) return show(cached.n);
	} catch {}
	try {
		const res = await fetch(`https://api.github.com/repos/${REPO}`, { headers: { Accept: 'application/vnd.github+json' } });
		if (!res.ok) return;
		const { stargazers_count: n } = await res.json();
		if (typeof n !== 'number') return;
		try {
			sessionStorage.setItem(KEY, JSON.stringify({ n, t: Date.now() }));
		} catch {}
		show(n);
	} catch {}
}

load();
