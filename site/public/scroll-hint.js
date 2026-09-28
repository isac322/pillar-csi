// Marks tables and code blocks that scroll sideways: wraps each in .pc-scroll,
// shows a "scroll" chip and an edge shadow while more content sits to the
// right, and makes the scroller keyboard-focusable. Styles: src/styles/scroll.css.
(() => {
	const sel = '.sl-markdown-content table, .expressive-code pre, .lp-code pre';
	const update = (box, s) => {
		const overflow = s.scrollWidth - s.clientWidth > 2;
		if (overflow) s.tabIndex = 0;
		box.classList.toggle('pc-scroll--overflow', overflow);
		box.classList.toggle('pc-scroll--more', overflow && s.scrollWidth - s.clientWidth - s.scrollLeft > 2);
	};
	const ro = new ResizeObserver((entries) => entries.forEach((e) => update(e.target.closest('.pc-scroll'), e.target)));
	document.querySelectorAll(sel).forEach((el) => {
		if (el.closest('.pc-scroll')) return;
		const box = document.createElement('div');
		box.className = 'pc-scroll';
		el.before(box);
		let s = el;
		if (el.tagName === 'TABLE') {
			// Tables scroll inside an inner wrapper so the chip stays in place.
			s = document.createElement('div');
			s.className = 'pc-scroll__table';
			s.append(el);
			box.append(s);
		} else box.append(el);
		s.addEventListener('scroll', () => update(box, s), { passive: true });
		ro.observe(s);
		update(box, s);
	});
})();
