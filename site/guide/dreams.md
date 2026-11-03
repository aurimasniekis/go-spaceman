# Dreams

A **dream** is a run of a dreamer agent that tidies role memory: it reads what the space's agents wrote since the last
dream, merges what matters into the roles' memory, fixes notes that turned out wrong, and reports what it found. It is a
normal agent on the space's node, with the same tools as other agents and nothing more.

## Setting up a dream

1. Give the space a role whose template is of kind **dreamer**. The built-in prompt **Dream: memory consolidation**
   (`dream-consolidation` in Prompts) describes the method; use it or your own.
2. In **Memory → Dreams → Add dream**, choose:
   - **Dreams as** (the space role of kind dreamer),
   - **Memories to consolidate** (role names, or `*`),
   - **Transcripts it may read** (role names, or `*`; the dreamer's own transcripts are left out unless you include
     them),
   - **Every (hours)** for a schedule, or 0 to dream only on demand,
   - **budgets**: time (the dream is stopped when it runs out), tokens (told to the dreamer, not enforced) and
     evidence (how many transcript files and bytes it is given).

<Shot name="dream-runs" alt="Nightly consolidation dream: a manual dream that succeeded with no change, its summary and an insight finding" caption="A dream's report" />

## What a dream sees

When a dream starts, the node prepares a folder for it:

- `memory/<role>/`: the dreamer's own copy of each role memory it consolidates. Its changes are kept the same way as
  any instance's: checkpointed, merged and synced ([Role memory](./memory)).
- `evidence/`: the part of each permitted transcript written since the last successful dream, newest first, within
  the budget.
- `DREAM.md`: what is where.

It may also ask knowledge bases ([Knowledge bases](./knowledge)) and read the space's instructions. It needs no
recordings and no administrative access.

## How a dream ends

The dreamer calls `spaceman_dream_report` with a summary, the files it changed and its **findings** (contradictions,
instruction conflicts, stale facts, insights, questions). Findings that need someone go out through the usual tools: a
message to control, a human action, a KB question.

Then SpaceMan checkpoints the dream's memory changes, and only after that marks the evidence it read as done. A dream
that ends without reporting, or runs out of time, leaves the evidence for the next dream.

A dream that **changes nothing** is a successful dream.

::: warning What a dream never does
A dream does not change instructions, prompts, approvals or permissions, and it does not turn one agent's remark into a
rule. When it finds a conflict with instructions it reports it.
:::
