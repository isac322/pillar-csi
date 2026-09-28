/**
 * Mark a horizontal scroller with the edges that still hide content (data-more="left|right|both"),
 * and show its cue element only while it overflows. CSS draws the edge fades from data-more.
 */
export function watchScroll(el: HTMLElement, cue?: HTMLElement | null) {
	const update = () => {
		const max = el.scrollWidth - el.clientWidth;
		const left = el.scrollLeft > 2;
		const right = el.scrollLeft < max - 2;
		if (max <= 2) el.removeAttribute('data-more');
		else el.dataset.more = left && right ? 'both' : left ? 'left' : 'right';
		if (cue) cue.hidden = max <= 2;
	};
	el.addEventListener('scroll', update, { passive: true });
	new ResizeObserver(update).observe(el);
	update();
}
