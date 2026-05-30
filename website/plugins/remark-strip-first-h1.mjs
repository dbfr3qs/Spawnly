// Removes the first top-level H1 from each page. The canonical Markdown in
// `docs/` keeps its `# Title` so it reads well on GitHub, but Starlight renders
// the page title from frontmatter — without this we'd get a duplicate heading.
export function remarkStripFirstH1() {
  return (tree) => {
    const i = tree.children.findIndex(
      (node) => node.type === 'heading' && node.depth === 1,
    );
    if (i !== -1) tree.children.splice(i, 1);
  };
}
