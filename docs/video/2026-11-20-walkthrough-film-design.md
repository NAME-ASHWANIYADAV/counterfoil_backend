# Counterfoil — Round-1 walkthrough film (design & script)

Approved 2026-11-20. This is the build contract for the ~5-minute submission video.

## Shape

- **Format:** hybrid film — animated TBO-styled bookends and chapter cards (the visual
  language of the 55s brag film) around a core of **real recorded footage of the running
  product**. Nothing in Act II is a mock-up: if it is on screen, it is the app responding.
- **Length:** target 4:50–4:58 (limit 5:00); **as built: 4:46 (285.8 s)**, the voice
  setting the pace. **File:** MP4, 1080p, ≤50 MB.
- **Voice:** Kokoro `af_heart` at 1.08×, one narration through the whole film.
- **Score:** three acts from the bundled library (vol-12 opens — the brand sound of the
  55s film; a second track under the walkthrough; an uplift for the close), crossfaded at
  chapter cards, voiceover-carved. Sparse SFX only where the eye is (bell, click, refuse).
- **Design tokens:** the product's measured TBO set (navy #013A7A/#014693/#0B5CB8,
  blue #307FE2, orange #F26B25, gold #F4B434, Montserrat / Open Sans / IBM Plex Mono,
  self-hosted OFL fonts). No TBO logo or mark — styling only, credited in the README.
- **Honesty rails:** all people and data synthetic (said out loud in VO line 5); the
  sign-in picker is narrated as a stand-in for SSO; no fabricated metrics — every number
  on screen is the app's own output; nothing claimed that the demo does not do.

## Act structure

| Act | Time | Content |
|---|---|---|
| I — The claim | 0:00–0:48 | Cold open (verdict + the lie), wordmark, thesis |
| II — The walkthrough | 0:48–4:35 | Five chapters of real footage with callouts |
| III — The close | 4:35–~4:55 | Scale tiles, creed, team slate, end mark |

Chapter cards: 2s navy cards with gold chapter numerals and Montserrat titles.

## The script (28 lines, ≈640 words)

Written for the voice: short clauses, hard periods. Initialisms spelled for TTS
("H R", "T B O", "A I"); IDs stay on screen, not in the mouth ("a single-use mandate",
not "MD-0002"). Target times shift to the measured WAVs at build; the voice sets the pace.

**Act I — cold open (animated)**
1. Every system in your H R stack knows how to say done. Almost none of them can prove it.
2. This is the costliest failure in automation. A system that says created… and saves nothing.
3. Counterfoil. An H R services platform, built for T B O — and for the next five minutes, it runs live.

**Chapter 1 — Sign in without secrets (footage)**
4. We sign in as real roles. No passwords here — this picker stands in for corporate single sign-on, and a pin appears only when a decision does.
5. Every person and every record you'll see is synthetic.

**Chapter 2 — The exit, end to end (footage)**
6. Meet the leaver. She doesn't file a ticket. She just says it, in plain language.
7. The agent reads intent, grounds itself in real tools — policy, dates, her actual record — and stages a card.
8. Note the chip. It names the model, and the one tool it was allowed to propose with. And still — nothing moves until she clicks.
9. One click, and the journey exists: a nineteen-step exit, with every routine step assigned to an agent.
10. This is her exit page. Clocks that never close by silence. Documents as they're earned. A passbook of receipts, growing as the work happens.
11. And one rule above all: a step is done only when the target system's own record says so. A reply is a claim. The record is the fact.

**Chapter 3 — Named humans sign (footage)**
12. Routine runs on agents. Decisions don't. Six reserved decisions can only be made by named people.
13. Her manager sees a fact sheet where every line names its source — and signs exactly this, nothing else. The hash makes "exactly" literal.

**Chapter 4 — The drill (footage; the climax). As built, the film follows the
product's real order: the drill is armed first, then payroll signs, then the
tampered dispatch is refused.**
14. Now we break it. On purpose. In front of you.
15. We arm a drill: add one rupee to the next reserved dispatch. Then we let the money move.
16. Payroll releases the settlement the same way. The signature mints a single-use mandate, bound to that hash, valid forty-five minutes.
17. The gate re-checks the hash — and refuses. It doesn't argue. It refuses, and writes down why.
18. The case lands in-doubt, with a named owner — and a drafted investigation that shows exactly what ran, and says in bold: a person decides. *(Retuned to the footage: the refusal fires before any send, so there is no external intent to read back — the investigation card states exactly that.)*
19. A human orders the retry, under the same mandate rules. And this time: released. Evidence — attributed. Confirmed in their system, not ours.

**Chapter 5 — Second journey, same kernel + proof (footage)**
20. Here's what makes this a platform, not a workflow. Onboarding is not new code. It's a package — data, checked and activated by two named people.
21. An offer flows through the same spine. Same warrants, same read-backs, same receipts — a second journey on one kernel.
22. And everything we just did is provable. Every receipt recomputes its own hash, in front of you.
23. An offline audit — ten checks, ten passes. No network. No database. A judge can re-run it.
24. Seven canaries try to break the guards on demand. Every one is refused, and every refusal is receipted.

**Act III — close (animated)**
25. Two journeys. One kernel. Fifteen of nineteen steps run by agents — six decisions that never leave named humans.
26. Packages are data. The guarantees travel to every H R line T B O runs.
27. Counterfoil. Named humans decide. Agents do the rest. Done means confirmed.
28. Team Init to Win It. For T B O VoyageHack four point oh.

## Shot list (Act II, per line)

- **L4–5:** login page; cursor hovers two personas; zoom chip on "pick anyone; one click,
  no password"; click Priya (leaver).
- **L6:** chat page; type "I think it's time for me to move on…" char by char.
- **L7:** agent reply slides in; callout ring on the reply.
- **L8:** zoom-in on the `src:agent · gemini-2.5-flash · propose_resignation ✓` chip,
  then the confirm card; the cursor clicks **Confirm and submit**.
- **L9:** My exit page loads; tracker visible; callout "15 of 19 steps · agent-run".
- **L10:** slow scroll: statstrip → clocks → documents → passbook.
- **L11:** zoom on a records-check/read-back receipt row; callout "done-at: evidence".
- **L12:** chapter card + switch persona (manager).
- **L13:** RSV-1 approval page: fact sheet, bound card, hash chip zoom, PIN, APPROVED.
- **L14:** switch to payroll; RSV-6 approval; mandate line callout.
- **L15–16:** admin page; arm "add ₹1 to the next reserved dispatch"; callout on the drill.
- **L17:** the REFUSED moment — receipt/flash; animated REFUSED echo slab over footage.
- **L18:** in-doubt console: item detail, "read the target's own record" → records: 0.
- **L19:** retry → `payment.released · attributed` receipt zoom.
- **L20:** admin packages table: `cfoil check` PASS → two-person activation (two PINs).
- **L21:** onboarding offer → ONB case → survey with `answers_hash` receipt.
- **L22:** receipts page scroll; hash chips.
- **L23:** audit card: 10/10, "no network, no database".
- **L24:** canaries table: 7/7 HELD.

## Production plan

1. **Stage:** build the app; reset to a pristine chain; mocks honest; `.env` key present
   (agent replies are live Gemini — real product, real chips). Pre-seed nothing that the
   film shows being created.
2. **Capture:** drive the real app with a scripted browser (puppeteer-core over the
   pinned chrome-headless-shell) at 1920×1080; record via CDP screencast to frames →
   ffmpeg → per-chapter MP4 takes. A soft fake cursor (blue dot + click ripple) is
   injected so actions read on screen. Takes are deterministic and retakable; each take
   is eyeballed frame-by-frame before it is cut in.
3. **Compose:** Hyperframes project `brag-output/walkthrough/` — animated scenes +
   `<video>` footage clips (`data-media-start` trims), callout overlays timed to the VO,
   chapter cards, three-track score with crossfades, voiceover carve, SFX.
4. **Gate:** `npx hyperframes check` zero errors; draft render; ~20 probe frames
   eyeballed; delivery render; ffprobe duration <5:00 and size ≤50 MB; poster frame
   baked as frame 0; captions file written.
5. **Deliverables:** `brag-output/walkthrough/walkthrough.mp4` (+ poster, captions,
   this script). The 55s brag film remains a separate shareable asset.

## Risks & answers

- **Gemini latency/nondeterminism on camera** → takes are per-chapter; re-run a take
  until clean; the chip and reply shown are whatever the live model actually said.
- **Headless flake on this machine** → the pinned headless-shell (already installed)
  cured it for the 55s film; capture uses the same binary.
- **50 MB budget** → 1080p H.264 at ~4:55 lands ~35–45 MB at delivery settings; verified
  with ffprobe before handover.
