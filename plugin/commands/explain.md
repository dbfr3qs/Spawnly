---
description: Explain how a Spawnly concept works, grounded in the real code, with an option to show it live
argument-hint: "[identity|token-minting|consent|delegation|revoke|tenancy|events]"
allowed-tools: Bash, Read, Grep, Glob
---

The user wants to understand how Spawnly works. Topic: **$ARGUMENTS**

Use the **spawnly-platform** skill, specifically its "Concept → source of truth"
map. Do this:

1. If no topic was given (empty `$ARGUMENTS`), list the concepts you can explain
   (identity, token-minting, consent, delegation/token-exchange, revoke/resume
   cascade, tenancy, lifecycle events) and ask which one — or take a
   plain-English question and map it to the closest concept.

2. **Read the authoritative file(s)** for the concept from the skill's map
   BEFORE explaining. Do not rely on memory or generic SPIFFE/OAuth knowledge —
   the point is to be correct for THIS codebase.

3. Explain in layers:
   - a tight **mental model** (a few sentences),
   - then **trace it through the code**, citing `file:line` for each step
     (use clickable links),
   - reference the matching doc at https://docs.spawnly.run/internals/ if one exists.

4. **Offer to show it live.** Propose the concrete demonstration from the skill
   (e.g. decode a running agent's JWT-SVID `sub`, `kubectl get clusterspiffeid`,
   show the `csi.spiffe.io` volume mount, or read an agent's
   `svid_fetched`/`token_issued` event timeline). Only run it if the user says
   yes and the cluster is up.

Keep the prose grounded and concrete. Prefer quoting the real code over
paraphrasing it.
