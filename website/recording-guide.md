# Demo recording guide

Shot-by-shot scripts for the three showcase demos. These are the source for the
clips embedded in the docs and reused on LinkedIn. Not published by the docs
site (Starlight only globs `../docs`); this lives here as a recording reference.

**Before any recording**

- Cluster up and seeded: `/spawnly:up` (or `make bootstrap`), then confirm green.
- Port-forwards: `kubectl port-forward svc/orchestrator 8080:8080` and
  `kubectl port-forward svc/dashboard 8090:8080`.
- Dashboard logged in as **alice / alice** at http://localhost:8090.
- Record at a fixed window size (1280×720 is plenty; bigger fonts read better in
  a feed). Hide bookmarks bar and any personal tabs. Silent is fine — these are
  short visual loops; add captions in post if you want narration.
- Keep each clip **30–75s**. Trim dead air aggressively; the goal is one "aha".

---

## 1. Real-time revocation cascade  (the most visual — lead with this)

**What it proves:** revoke one agent and its entire descendant subtree loses
authorisation within seconds, live, while its ancestors keep working. Reversible.

**Target length:** ~60s.

**Setup (off-camera):** spawn a `chain-worker` chain so you have a 3–4 deep tree.
```
curl -s -X POST localhost:8080/spawn -H 'Content-Type: application/json' \
  -d '{"userId":"user-1","tenantId":"tenant-1","agentType":"chain-worker","task":"chain"}'
```
Wait until the dashboard shows the nested tree with every node green / `work_ok`.

**Shot list:**
1. **(0–8s)** Open on the dashboard with the full chain visible, all nodes
   ticking `work_ok`. Let it breathe for a beat so the viewer sees it's *live*.
2. **(8–15s)** Hover/click the **middle** node to select it. Make it obvious
   which node you're about to revoke (not the root, not a leaf).
3. **(15–20s)** Click **Revoke**.
4. **(20–40s)** The money shot: the selected node **and everything below it**
   flips to `work_denied` (403) within a couple of seconds, while the nodes
   *above* it keep showing fresh `work_ok` events. Hold on this contrast.
5. **(40–55s)** Click **Resume** on the same node; watch the subtree recover to
   `work_ok`.
6. **(55–60s)** Rest on the recovered tree.

**Caption ideas:** "Revoke cascades to the whole subtree — in real time" /
"Ancestors keep working. Pods stay up; their next call just gets a 403."

---

## 2. Human-in-the-loop spawn consent (CIBA)  (the most novel)

**What it proves:** a sub-agent can't get credentials until a human approves the
spawn — using OpenID CIBA, the backchannel flow banks use for payment approval.
Stored consent auto-approves repeats; revoking consent re-prompts.

**Target length:** ~75s.

**Setup:** use a `chain-worker` type whose template marks its child as
consent-gated (scenario 3 in `/spawnly:demo`). Have the dashboard in view.

**Shot list:**
1. **(0–10s)** Spawn the chain. The first link comes up but its child sits
   **pending** — show the agent waiting, not yet working.
2. **(10–20s)** The **consent prompt** appears on the dashboard (the banner /
   pending card). Read it: which user, which parent type, which child type,
   which scopes. This is the whole point — frame it clearly.
3. **(20–30s)** Click **Approve**. The child immediately gets its token and
   flips to `work_ok`. Show the `consent_granted` → `work_ok` transition.
4. **(30–45s)** Deeper links spawn and **auto-approve** from the stored consent
   — no new prompt. Point this out (caption): "same (user, parent, child) ⇒
   approved once, remembered."
5. **(45–60s)** Open the **Consents** modal and **Revoke** the consent.
6. **(60–75s)** Trigger the next spawn / renewal; the prompt **re-appears**.
   Optionally **Deny** it and show the child go `failed` with `consent_denied`
   while the parent keeps working.

**Caption ideas:** "An agent asks permission to spawn another — and a human
decides" / "CIBA: the bank-grade approval flow, repurposed for agent authority."

---

## 3. Plugin onboarding  (the differentiator — Claude walks you through it)

**What it proves:** you don't read a README — you install a Claude Code plugin
and it brings the platform up and narrates a live demo on your own machine.

**Target length:** ~75–90s (this one can run a touch longer).

**Format note:** this is a Claude Code terminal session, so it records well as a
screen capture of the terminal (or asciinema if you prefer text-selectable). The
others are browser/dashboard captures.

**Shot list:**
1. **(0–10s)** `/plugin marketplace add dbfr3qs/Spawnly` then
   `/plugin install spawnly@spawnly` — show it resolving the commands.
2. **(10–20s)** Type `/spawnly:` and show the command palette:
   `up`, `status`, `doctor`, `explain`, `demo`.
3. **(20–35s)** Run `/spawnly:status` (or `/spawnly:up` if recording from cold)
   — show Claude reporting real cluster health, not canned text.
4. **(35–75s)** Run `/spawnly:demo 2` (revoke cascade) and let Claude narrate
   the explain → act → show beats while the dashboard updates alongside. A
   split view (terminal + dashboard) is ideal here.
5. **(75–90s)** End on `/spawnly:explain <concept>` to show it can go deeper on
   demand.

**Caption ideas:** "Most projects ship a README. This one ships an interactive
guide." / "Install the plugin; Claude brings up the platform and demos it live."

---

## Post-production checklist

- Trim to the beats above; cut all waiting/spinner time.
- If self-hosting: export H.264 MP4, ~720p, target a few MB; generate a poster
  frame (first clear frame) for the `<video>` element.
- If using GIFs (e.g. README hero loop): keep them short and palette-optimised.
- Same three clips upload natively to LinkedIn for the content series.
