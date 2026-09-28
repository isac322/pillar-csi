// GitHub star nudge: fills [data-star-count] with the live stargazer count
// (cached per session, hidden on failure) and handles the dismissible bar.
(() => {
	const ss = { get: (k) => { try { return sessionStorage.getItem(k); } catch { return null; } }, set: (k, v) => { try { sessionStorage.setItem(k, v); } catch {} } };
	const show = (n) => document.querySelectorAll('[data-star-count]').forEach((el) => { el.textContent = new Intl.NumberFormat('en', { notation: 'compact' }).format(n); el.hidden = false; });
	const repo = document.querySelector('[data-star-repo]')?.dataset.starRepo;
	const cached = ss.get('pc-stars');
	if (cached !== null) show(Number(cached));
	else if (repo) fetch(`https://api.github.com/repos/${repo}`).then((r) => (r.ok ? r.json() : null)).then((d) => { if (typeof d?.stargazers_count === 'number') { ss.set('pc-stars', String(d.stargazers_count)); show(d.stargazers_count); } }).catch(() => {});
	const bar = document.querySelector('[data-starbar]');
	if (bar && ss.get('pc-starbar-hidden')) bar.hidden = true;
	bar?.querySelectorAll('[data-starbar-close]').forEach((b) => { b.hidden = false; b.addEventListener('click', () => { bar.hidden = true; ss.set('pc-starbar-hidden', '1'); }); });
})();
