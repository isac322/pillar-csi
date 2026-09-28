import { visit, SKIP } from 'unist-util-visit';

const LABELS = { note: 'Note', tip: 'Tip', caution: 'Caution', danger: 'Danger' };

/**
 * Starlight-style asides (`:::note[Title]`) rendered as framed drawing notes.
 * Any other directive syntax is put back as the literal text it came from,
 * so prose such as `host:port` is never swallowed.
 */
export default function remarkAsides() {
	return (tree) => {
		visit(tree, (node, index, parent) => {
			if (node.type === 'containerDirective' && LABELS[node.name]) {
				let title = LABELS[node.name];
				const first = node.children[0];
				if (first?.data?.directiveLabel) {
					title = toText(first);
					node.children.shift();
				}
				node.data = {
					hName: 'aside',
					hProperties: { className: ['aside', `aside-${node.name}`], 'aria-label': title },
				};
				node.children.unshift({
					type: 'paragraph',
					data: { hName: 'p', hProperties: { className: ['aside-label'] } },
					children: [{ type: 'text', value: title }],
				});
				return;
			}
			if ((node.type === 'textDirective' || node.type === 'leafDirective') && parent) {
				const prefix = node.type === 'textDirective' ? ':' : '::';
				const restored = [{ type: 'text', value: prefix + node.name }];
				if (node.children?.length) {
					restored.push({ type: 'text', value: '[' }, ...node.children, { type: 'text', value: ']' });
				}
				if (node.attributes && Object.keys(node.attributes).length) {
					const attrs = Object.entries(node.attributes)
						.map(([k, v]) => (v ? `${k}="${v}"` : k))
						.join(' ');
					restored.push({ type: 'text', value: `{${attrs}}` });
				}
				if (node.type === 'leafDirective') {
					parent.children.splice(index, 1, { type: 'paragraph', children: restored });
				} else {
					parent.children.splice(index, 1, ...restored);
				}
				return [SKIP, index];
			}
		});
	};
}

function toText(node) {
	if ('value' in node) return node.value;
	return (node.children ?? []).map(toText).join('');
}
