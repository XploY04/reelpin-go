# Documentation map

`AGENTS.md` at the root is the short map: commands, layout, rules, traps. This
folder is the detail behind it. Read the page the task needs, not the folder.

| Page | Read it when |
|------|--------------|
| [`../ARCHITECTURE.md`](../ARCHITECTURE.md) | You need the shape of the system and why it is that shape. |
| [`domain-model.md`](domain-model.md) | You are touching reels, jobs, or how one becomes the other. |
| [`api-contract.md`](api-contract.md) | You are adding, changing, or breaking an endpoint. |
| [`operations.md`](operations.md) | You are running it, configuring it, or it is misbehaving. |
| [`cutover.md`](cutover.md) | You are deploying the image, or switching traffic from Python to Go. |
| [`decisions/`](decisions/) | You want to know why something is the way it is before changing it. |
| [`plans/`](plans/) | You are picking up work someone else scoped. |

## How these are kept true

Everything here was checked against the code at the commit that introduced it.
A page that disagrees with the code is a bug in the page.

- **Change the behaviour, change the page in the same PR.** A doc that lags is
  worse than no doc, because the next person believes it.
- **Do not write what the code already says.** Function signatures, field lists
  and route tables belong in the code or in generated artifacts. These pages
  carry the parts that are not visible from one file: the shape, the reasons,
  and the traps.
- `make check` fails on a broken link between these pages.

## Decisions

A decision record explains a choice that is expensive to reverse or surprising
to a newcomer. Add one when you make such a choice; do not add one for a routine
implementation detail.

Numbered, immutable once merged. Superseding one means adding a new record that
says so, never editing history.

## Plans

`plans/active/` holds work that is scoped but unfinished. `plans/completed/`
holds what shipped, kept because the reasoning outlives the work. A plan is a
checked-in artifact so that a handover is a file, not a conversation.

Files matching `*.local.md` in `plans/active/` are ignored: scratch space for
whoever is holding the pen.
