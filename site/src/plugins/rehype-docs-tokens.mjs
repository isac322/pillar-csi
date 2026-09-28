// Rehype plugin for docs pages.
//
// 1. Inline code: keeps each token whole. Text is split after spaces and '/'
//    (and after '.' in tokens longer than 24 characters) into nowrap spans, so
//    code wraps only at those points and never at a hyphen (`lvextend -L`,
//    `democratic-csi`). The copied text is unchanged.
// 2. Status cells: a table cell whose whole text is "Shipped", "Planned" or
//    "Not applicable" is rendered as a badge (styles in starlight.css).

const STATUS = { Shipped: 'shipped', Planned: 'planned', 'Not applicable': 'na' };

/** @param {any} node */
const textOf = (node) =>
	node.type === 'text' ? node.value : (node.children ?? []).map(textOf).join('');

/**
 * Splits code text into nowrap spans joined by <wbr> break opportunities.
 * @param {string} value
 */
function splitToken(value) {
	const splitDots = value.length > 24;
	const parts = value.split(splitDots ? /(?<=[/. ])/ : /(?<=[/ ])/);
	return parts.flatMap((part, i) => [
		...(i > 0 ? [{ type: 'element', tagName: 'wbr', properties: {}, children: [] }] : []),
		{
			type: 'element',
			tagName: 'span',
			properties: { className: ['pc-nb'] },
			children: [{ type: 'text', value: part }],
		},
	]);
}

/** @param {any} node @param {boolean} inPre */
function visit(node, inPre) {
	if (node.type !== 'element' && node.type !== 'root') return;
	const cls = node.properties?.className ?? [];
	const skip = inPre || node.tagName === 'pre' || (Array.isArray(cls) && cls.includes('expressive-code'));

	if (!skip && node.tagName === 'code') {
		node.children = node.children.flatMap((child) =>
			child.type === 'text' ? splitToken(child.value) : [child],
		);
		return;
	}

	if (node.tagName === 'td') {
		const status = STATUS[textOf(node).trim()];
		if (status) {
			node.children = [
				{
					type: 'element',
					tagName: 'span',
					properties: { className: ['pc-badge', `pc-badge--${status}`] },
					children: [{ type: 'text', value: textOf(node).trim() }],
				},
			];
			return;
		}
	}

	node.children?.forEach((child) => visit(child, skip));
}

export default function rehypeDocsTokens() {
	return (tree) => visit(tree, false);
}
