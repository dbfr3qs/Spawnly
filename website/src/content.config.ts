import { defineCollection } from 'astro:content';
import { docsSchema } from '@astrojs/starlight/schema';
import { glob } from 'astro/loaders';

// Keep docs/ as the single source of truth: the site globs the canonical
// Markdown from the repo's docs/ directory rather than copying it in.
export const collections = {
  docs: defineCollection({
    loader: glob({ pattern: '**/*.md', base: '../docs' }),
    schema: docsSchema(),
  }),
};
