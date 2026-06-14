import { defineCollection } from 'astro:content';
import { docsSchema } from '@astrojs/starlight/schema';
import { glob } from 'astro/loaders';

// Keep docs/ as the single source of truth: the site globs the canonical
// Markdown from the repo's docs/ directory rather than copying it in.
//
// saas/ is excluded for now: those are internal design docs with no Starlight
// frontmatter (just a leading `# Title`), so docsSchema() rejects them and the
// build fails. Add `title:` frontmatter to publish them on the site later.
export const collections = {
  docs: defineCollection({
    loader: glob({ pattern: ['**/*.md', '!saas/**'], base: '../docs' }),
    schema: docsSchema(),
  }),
};
