# The seven-minute walkthrough

Everything below is clickable by a judge; nothing is staged footage. Personas and PINs:

| Person | Role | PIN |
|---|---|---|
| Riya Sharma | the leaver | — |
| Arjun Mehta | manager, accepts the resignation (RSV-1) | 2201 |
| Kavita Rao | payroll approver, signs off final pay (RSV-6) | 3307 |
| Dev Patel | HR operations: in-doubt queue, kill switch, drills | 4418 |
| Meera Nair | HR administrator: packages, reserved list | 5529 |
| A. Kumar | auditor | 6630 |

Start fresh, then keep two browser windows: the product (7070) and **TargetSys** (7072) —
the other teams' systems. What TargetSys stores is the fact the product must prove.

```
go run ./cmd/cfoil demo -reset
```

## 1 · Riya resigns (chat)

Sign in as **Riya Sharma** → type: *What is my notice period, and does my leave get paid
out?* With a model configured (`.env`), the **agent** answers: the chip under the reply
shows which model ran and that `get_policy` actually executed — twice, once per
sub-question, each answer cited. Without a key, the rule pack answers directly and the
chip says `rules`. Then resign — either plainly (*I wish to resign, my last working day
should be* **today's date**; rules detect this, no model needed) or softly (*I feel it
is time for me to move on…*; the agent catches it and stages the same card via
`propose_resignation`). Either way a confirmation card appears — **the click is the
formal tender**, never the model. Confirm — you land on **My exit**, the leaver's own
case room: a summary strip, an honest "what happens next" read from the step engine,
the tracker, clocks, documents and passbook.

Worth asking afterwards: *Where does my exit stand?* (`get_my_exit_status` reads the
live case from receipts) and *How much will my settlement be?* — the honest answer
before computation is "not computed yet", never a guess.

Open **My exit** in the sidebar: steps 1–5 complete in seconds (records check, disclosure screen,
notice rule proposes the earliest date and the buyout for the shortfall, approval pack).
Step 6 waits on an amber diamond: **nothing is accepted by an agent, ever**.

## 2 · Arjun decides (RSV-1)

Sign in as **Arjun Mehta** → *Your work* → open the approval. Left: the fact sheet, every
line with its source. Right: **exactly what his signature binds**, with its hash.

- For the clean story, **untick the notice buyout** (he waives the recovery while agreeing
  the early release; the settlement stays positive). Leaving it ticked is also honest —
  the settlement simply ends negative as a recovery.
- Enter PIN **2201** → Approve. The approval mints a single-use mandate bound to that hash.

Back on the case (Cases → EXIT-0001): steps 7–10 run — ticket, directory disable, card
revoke — each with a green ✓ and evidence **attributed**. Check TargetSys: the records are
there, carrying `client_ref`. Complete the handover under *Your work*; as Riya, decline or
fill the survey.

## 3 · The money, attacked first (RSV-6 + tamper drill)

Sign in as **Dev Patel** → *Controls & drills* → **Arm: add ₹1 to the next reserved
dispatch**. Now as **Kavita Rao** → *Your work* → the sign-off shows the settlement, every
line cited (gratuity honestly "not eligible: 3.7 y < 4.75 y"). PIN **3307** → Approve.

The dispatcher changes the amount by one rupee *after* her approval — and the gate
refuses: **request hash ≠ mandate hash**, receipted, and an in-doubt item lands with Dev.
As Dev → *In doubt* → resolution **Send again** (the drill is over; the bytes now match).
The release goes out, the payroll stand-in shows the payment with our reference, and the
**final-wages clock closes on `payment.released` at evidence `attributed`** — the only
thing that can close it. The letter issues; the case closes with its closure statement.

## 4 · The lie (why read-back exists)

Controls → set **payroll** (or itsm) to *lie mode* and run a second exit (another chat
resignation as Riya is blocked — one open case per employee — so use the canary instead,
or re-run after `-reset`). Quickest on stage: **Run the 7 canaries** — C6 sends a create to
a target that always replies "201 Created" and stores nothing; the verdict must be
*in doubt*, never Done. All seven guards are re-tested live, each run receipted.

For the full beat, arm **itsm lie mode before step 2**: the asset ticket then lands in
doubt ("replied 201 but its own records hold nothing"). As Dev, press **Draft
investigation summary**: a bounded agent calls `get_item_details`, then
`read_target_record` — a *fresh, real* read of the target with the production read-back
logic — and writes a schema-constrained recommendation. The card lists what actually
ran, step by step, and says in bold that a named person decides. Clear the lie, resolve
**Send again**, and the step completes `attributed`.

## 5 · Kill switch and crash

- Controls → **Pause agents now**: the next action blocks, approvals stay readable, and
  the clocks keep running — pausing agents never extends a legal deadline. Resume.
- Crash drill: kill the `cfoil` process mid-run (Task Manager or Ctrl-C) and start
  `go run ./cmd/cfoil serve` again. State rebuilds from the receipts; any intent left
  PENDING is reconciled by **reading the target**, never by resending blindly.

## 6 · Extending the platform, live — then RUNNING the extension

Sign in as **Meera Nair** → Controls → *Service packages*. The **onboarding** package (16
steps) ships inactive. **Run cfoil check** → PASS with a certificate hash. Activate with
two named people (Meera **5529** + Dev **4418**). The core did not change — a package is
data; `git status` stays clean. To show a refusal, add `"timeout_approve_hours": 48` to
any approval step of `extensions/onboarding/package.json` and run the check again:
**REFUSED — approval by silence or timeout is forbidden.**

Then prove it is a journey, not a slide: as **Dev** → *People* → **Start onboarding**.
(Try it before activating and the form itself is refused — the admission gate guards case
creation too.) Enter an offer — name, email, title, manager, gross, joining date. Watch
the same kernel run the second service line: the duplicate-hire check reads the
directory's own records live; Meera extends the offer under **RSV-2** (a new mandate in
the registry); laptop, card and payroll effects go out under the warrant path and come
back **attributed**; day-one readiness is re-checked against the step evidence; the
week-one survey is a real form whose submission is a receipt; the case closes with its
own closure statement. The new joiner now has a profile under *People* whose live panels
honestly show "no account yet" until IT provisions them.

## 7 · The audit room

Sign in as **A. Kumar** → *Audit room* → click any receipt number in the chain: the
counterfoil opens as its own page, links back to its predecessor, and **recomputes its
hash in front of you** — with the signer's checkpoint coverage stated. (Receipt numbers
link like this everywhere: the passbook, the clocks board, a person's profile.)

Worth showing on the way: **People** → Riya — the directory and card panels read the
target systems' own records live, each carrying the `last_action_ref` that ties the
disabled account and revoked card to our requests. **Clocks** shows every statutory
deadline on the instance with what alone can close it. **How decisions are made** renders
the reserved list and rule pack from the instance's own configuration.

Then **Export + verify now**: ten checks pass against the exported bundle. Prove it owes
the running system nothing — in a terminal (network off if you like):

```
go run ./cmd/verify-audit -dir export
```

Close on the dashboard: automation measured from receipts beside the 15-of-19 design
target, coverage by evidence level, clocks closed on time, 7/7 canaries, the countersigned
chain head — and model spend ₹0.00 unless a key was configured, because nothing here
pretends.

**The line to end on:** an "OK" is a claim; the system's own record is the fact.
Done means confirmed.
