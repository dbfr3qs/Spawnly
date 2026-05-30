// Rewrites the relative links in the canonical `docs/` Markdown so they work on
// the published site:
//   - doc → doc   ( ../delegation-design.md , 01-job-and-exit.md )  → internal route
//   - doc → source ( ../../internal/foo.go#L10 , ../../Dockerfile ) → GitHub URL
// Files keep their original relative links (correct when read in-repo); this
// transform only affects the rendered site.
import path from 'node:path';

const BLOB = 'https://github.com/dbfr3qs/Spawnly/blob/main/';
const TREE = 'https://github.com/dbfr3qs/Spawnly/tree/main/';

export function remarkRewriteRepoLinks() {
  return (tree, file) => {
    // Astro runs with cwd = website/; the repo root is its parent.
    const repoRoot = path.resolve(file.cwd, '..');
    const fileDir = path.dirname(file.path);

    const walk = (node) => {
      if (node.type === 'link' && typeof node.url === 'string') {
        node.url = rewrite(node.url, fileDir, repoRoot);
      }
      if (Array.isArray(node.children)) node.children.forEach(walk);
    };
    walk(tree);
  };
}

function rewrite(url, fileDir, repoRoot) {
  // Leave external URLs, site-absolute paths, and pure anchors untouched.
  if (/^(https?:|mailto:|\/|#)/.test(url)) return url;

  const [target, hash = ''] = url.split('#');
  const anchor = hash ? `#${hash}` : '';
  if (!target) return url;

  const abs = path.resolve(fileDir, target);
  const rel = path.relative(repoRoot, abs).split(path.sep).join('/');

  // A Markdown file inside docs/ → an internal Starlight route.
  if (rel.startsWith('docs/') && rel.endsWith('.md')) {
    const route = '/' + rel.slice('docs/'.length).replace(/\.md$/, '');
    return route + anchor;
  }

  // Anything else points at repo source: link to GitHub. Use blob for files,
  // tree for directories (no file extension).
  const base = path.extname(rel) ? BLOB : TREE;
  return base + rel + anchor;
}
