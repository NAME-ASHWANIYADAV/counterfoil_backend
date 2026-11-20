# Counterfoil

**Named humans decide. Agents do the rest. Done means confirmed.**

Counterfoil is an agent-first HR services platform, shown in depth on employee exits
(resignation to full-and-final settlement). It is Team *Init to Win It*'s prototype for
TBO VoyageHack 4.0, PS2 — "AI-native HR services platform" (Unstop topic: Darwin box
Automation).

The one idea: **a reply is a claim; the record is the fact.** Every HR action on another
team's system runs under a *warrant* — who asked, under which rule and, for reserved
decisions, a named person's signed approval of exactly that request — and a step counts as
done only when the *target system's own record* confirms it. Every state transition leaves
a *counterfoil*: a hash-chained, countersigned receipt that an auditor can re-check offline.

## Run it

Requirements: Go 1.24+ on Windows, macOS or Linux. No database, no Node, no external
dependencies — the standard library only.

```
go run ./cmd/cfoil demo -reset
```

That starts three separate processes and wipes old demo data:

| Process | Port | What it is |
|---|---|---|
| `cfoil` (product) | http://localhost:7070 | the platform: chat, approvals, case room, dashboard, controls, audit room |
| `cfoil-mocks` | http://localhost:7072 | **TargetSys** — the other teams' systems (IT service desk, directory, access cards, payroll), built and labelled as ours; each has a *lie mode* |
| `cfoil-signer` | http://localhost:7071 | the separate process that alone holds the checkpoint signing key |

Then open http://localhost:7070 and follow [DEMO.md](DEMO.md) — the whole journey takes
about seven minutes. Sign-in is a demo persona picker; PINs (in
[rules/users.json](rules/users.json)) stand in for the identity provider's re-login and
are asked only at decisions.

Useful commands:

```
go run ./cmd/cfoil check extensions/onboarding    # package admission check (REFUSED / PASS + certificate)
go run ./cmd/verify-audit -dir export             # offline audit: 10 checks, no network, no database
go test ./...                                     # targeted tests: hashes, ledger, clocks, checker
```

## Why this design (the problem behind the problem)

TBO's brief already names the failure that matters: *"Retries must not duplicate actions,
for example ordering two laptops."* Retry protection handles that — but it does not notice
the opposite failure, which real incident reports keep showing: **a system replies
"created" and saves nothing**, an agent believes it, and the workflow marks a step done
that never happened. In HR this is an ex-employee's account that still works, or final
wages that were never released — with a statutory clock running.

So Counterfoil treats an exit as a set of **clocks** (final wages within two working days
of the last working day; the relieving letter; gratuity where eligible), and a clock
closes **only on evidence at its declared level**:

| Evidence level | Meaning |
|---|---|
| `attributed` | the target's own record carries *our* reference — proof our request did this |
| `observed` | the end state is right, but cannot be tied to our request |
| `reply_only` | only the target's reply claims success — never enough to close a money clock |

A success reply cannot close a clock. An agent's message cannot. A timeout cannot. And the
kill switch cannot — pausing agents never silently extends a legal deadline.

## The exit journey (19 steps, 2 reserved decisions)

Agents and rules run 15 of the 19 steps; a named manager accepts the resignation and fixes
the release date (RSV-1), a payroll approver signs off and releases the settlement
(RSV-6), the manager confirms handover, and the leaver may fill the exit survey. The
journey ships as a data package ([extensions/exits/package.json](extensions/exits/package.json));
dates, money and approvers come from the cited rule pack
([rules/rulepack.json](rules/rulepack.json)) and from people — never from model text.

The warrant path for every external effect:

```
policy gate (reserved list · kill switch · mandate hash check)
  → intent ledger: PENDING, synced to disk BEFORE any bytes leave
  → send once, with Idempotency-Key and X-Client-Ref
  → read back the target's own record, by our reference
  → Done (with its evidence level) or IN DOUBT → a named person
```

A crash between send and commit is reconciled on restart by *reading the target*, never by
resending blindly. A changed request after approval — one rupee added — fails the mandate's
hash check and is refused with a receipt.

## What makes it a platform, not a point solution

- **Packages.** A service line is one folder of data: steps, effects, read-back
  declarations. `cfoil check` refuses unsafe rules (approval by silence or timeout, a
  reserved effect without its approval step, an effect with no read-back declaration) and
  issues a certificate hash; **two named people** activate exactly that hash. The
  onboarding package ships inactive so activation can be shown live. Zero core files
  change.
- **Reserved list as configuration.** The brief's six reserved decisions are a floor HR
  can add to but never remove ([policy/reserved.json](policy/reserved.json)).
- **Canaries.** Seven probes re-test the guards on demand — a forged mandate, a tampered
  amount, an agent trying to decide an approval, an expired mandate, a duplicate intent, a
  lying success reply, a kill-switch bypass — and every run leaves a receipt.
- **Audit by export.** The audit room exports the receipt bundle; `verify-audit` re-checks
  it offline: the hash chain, the signer's countersignatures, a human-issued mandate behind
  every reserved effect, a read-back or in-doubt behind every dispatch, clock closures at
  their declared evidence, intent uniqueness, canaries and two-person activations. The
  signer's public key is pinned outside the bundle (`signer_pub.txt`, written from the live
  signer, or the `-pub` flag), so a re-signed bundle fails; the signer itself refuses to
  countersign a different history for a sequence it already signed. An in-doubt item on a
  reserved step can only be re-dispatched under its mandate — never closed or cancelled by
  hand — and a skipped step satisfies dependencies only when it was optional by design.
  All three processes bind to loopback by default; `CFOIL_SIGNER_TOKEN` adds a shared
  secret on the signing endpoint.

## Where the AI is (and is not)

Rules decide dates, money, approvers and routing. A model only *reads and drafts text* —
acknowledging the resignation, answering policy questions — and policy Q&A is served from
the cited rule pack without any model call. With `ANTHROPIC_API_KEY` set, the intake
acknowledgement is drafted by a model (default `claude-haiku-4-5`, override with
`CFOIL_MODEL`); every call is metered and receipted, and the dashboard shows the spend.
With no key, the product runs whole and says "rules (no model configured)" — nothing here
pretends. POSH/grievance text is routed only, sealed, and never reaches a model or the
receipts (a hash stands in).

## Status: what is real here, and what is not

This is a hackathon prototype on synthetic data. Honestly separated:

**Working in this repository** — the 19-step journey end to end on the stand-ins; mandates
bound to canonical request hashes with refusal on mismatch; the intent ledger with
crash-safe claims; read-back with three evidence levels; the in-doubt queue; clocks with
working-day arithmetic and holiday skips; kill switch; package check + two-person
activation; canaries; hash-chained receipts, a separate signer, export and the 10-check
offline verifier; the full web product.

**Deliberately simplified** — storage is append-only JSONL + replay (PostgreSQL is the
production design); the demo IdP is a persona picker + PIN (production design: OIDC
re-authentication with nonce = mandate hash); the durable-execution engine (DBOS/Temporal)
is replaced by a tick loop + ledger reconciliation; TargetSys stand-ins are ours — **TBO's
mock APIs are not published yet**, and when they are, the dispatcher points at them;
laptop-return tracking, PF/ESI, multi-entity and multi-tenant are out of scope; statutory
rules are demo fixtures flagged `needs_legal_confirmation` — not legal advice.

**Measured elsewhere, not here** — our design spikes (separate from this repo, 17 Nov):
read-back caught 82 of 82 false "created" replies (control: 0 of 82); 0 duplicate creates
across 275 mid-protocol process kills; the package checker found 80 of 82 planted defects
in 91 synthetic packages. Crash-stop faults only; not TBO's mocks; a target that commits
after our deadline is outside the stated fault model and can still duplicate.

## Prior art, credited

None of the building blocks is new to the world, and we say so: idempotency keys and
intent logs (mycelium-runtime; Anchor — 1st place, CockroachDB × AWS hackathon 2026, whose
protocol write-up we also credit for *attributable evidence* and the three-valued
verdict), request-bound signed approvals (the AP2 protocol; the counter-sign draft),
read-back verification (SailPoint IdentityIQ), canary probes (Overpass), statutory
deadline traces (Instanter), replay-gated activation (OneDecision), and the governed
action flow sketched in TBO's own brief (as in fellow PS2 designs TBO-Helm, HireLoop and
Karya). **What we add is the join**: read-back on every HR effect including final pay,
HR's own reserved list enforced on every path, legal clocks closed only by confirmed
receipts, and extensions that inherit the same guarantees.

We never claim "exactly once", "tamper-proof", "guaranteed" or "first".

## Repository map

```
cmd/cfoil            the binary: demo | serve | signer | mocks | check | verify
cmd/cfoil-signer     the signer as its own binary        cmd/cfoil-mocks   the stand-ins
cmd/verify-audit     the offline verifier
internal/core        canonical JSON + hashing, JSONL stores, receipt chain, intent ledger
internal/kernel      policy gate & mandates, rules & clocks, dispatcher & read-back,
                     step engine, packages & admission check, canaries, model router
internal/webapp      the product surface (server-rendered; one CSS file; no JS framework)
internal/mocksrv     TargetSys stand-ins with lie modes
internal/signersrv   the key-holding process              internal/audit    the 10 checks
extensions/          exits (19 steps) + onboarding (16, ships inactive)
policy/ rules/       reserved floor; cited rule pack, users, synthetic employees
```

## Team and tools

Team **Init to Win It** — Sonal Verma (lead), Ashwani Yadav, Arpita, Vaibhav Gupta,
Lakshay Bansal. Built with Go's standard library only. AI tools: Claude (Claude Code) was
used for research, drafting and writing this prototype's code under the team's direction;
see the Round 1 architecture brief for the full disclosure. License: to be chosen by the
team before publishing; no third-party code is vendored.
