import { visit, SKIP } from 'unist-util-visit';

const LANG_NAMES = { sh: 'shell', bash: 'shell', shell: 'shell', console: 'shell', yaml: 'yaml', yml: 'yaml', json: 'json', text: 'text', go: 'go', toml: 'toml', ini: 'ini', diff: 'diff' };

/** Let long unbroken strings (paths, domains) in table code cells wrap after '/' and '.'. */
function breakableCode(td) {
	visit(td, 'element', (el) => {
		if (el.tagName !== 'code' || !Array.isArray(el.children)) return;
		el.children = el.children.flatMap((child) => {
			if (child.type !== 'text' || !/[/.]/.test(child.value)) return [child];
			const out = [];
			for (const chunk of child.value.split(/(?<=[/.])/)) {
				if (!chunk) continue;
				out.push({ type: 'text', value: chunk }, { type: 'element', tagName: 'wbr', properties: {}, children: [] });
			}
			if (out.length) out.pop();
			return out.length ? out : [child];
		});
	});
}
/** Wrap tables for horizontal scroll and give code blocks a label strip + copy button. */
export default function rehypeDocBlocks() {
	return (tree) => {
		visit(tree, 'element', (node, index, parent) => {
			if (!parent || index === undefined) return;
			if (node.tagName === 'table') {
				visit(node, 'element', (cell) => {
					if (cell.tagName === 'td') breakableCode(cell);
					// A trailing space keeps search excerpts from running adjacent cells together; it does not render.
					if (cell.tagName === 'td' || cell.tagName === 'th') cell.children.push({ type: 'text', value: ' ' });
				});
				parent.children[index] = {
					type: 'element',
					tagName: 'div',
					properties: { className: ['table-wrap'], tabIndex: 0, role: 'region', ariaLabel: 'Table' },
					children: [node],
				};
				return SKIP;
			}
			if (node.tagName === 'pre') {
				const lang = String(node.properties?.dataLanguage ?? 'text');
				parent.children[index] = {
					type: 'element',
					tagName: 'figure',
					properties: { className: ['code'] },
					children: [
						{
							type: 'element',
							tagName: 'figcaption',
							properties: { className: ['code-bar'], dataPagefindIgnore: '' },
							children: [
								{ type: 'element', tagName: 'span', properties: { className: ['code-lang'] }, children: [{ type: 'text', value: LANG_NAMES[lang] ?? lang }] },
								{
									type: 'element',
									tagName: 'button',
									properties: { type: 'button', className: ['code-copy'], dataCopy: '', ariaLive: 'polite' },
									children: [{ type: 'text', value: 'Copy' }],
								},
							],
						},
						node,
					],
				};
				return SKIP;
			}
		});
	};
}
