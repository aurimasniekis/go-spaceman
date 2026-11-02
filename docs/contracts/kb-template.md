# llm-knowledge-base-template (example KB layout)

- **Source:** <https://github.com/aurimasniekis/llm-knowledge-base-template>, inspected at `b84970f` (pushed 2026-10-11)
  through the GitHub API on 2026-11-02.
- **Licence:** code (`tools/`) MIT; documentation, templates and playbooks CC BY 4.0 (`LICENSE`, `LICENSE-docs`).
  SpaceMan copies none of it; it only documents how to use such a repository.
- **Role in SpaceMan:** an example of a KB repository, not a required layout or schema (SM-KB-001).

## Facts that shape how SpaceMan uses it

- `AGENTS.md` names one **keeper** session that writes and commits; every other session is a **consumer** that reads
  and may only write to `agents/inbox/`, messaging the keeper with `ASK:` / `REPORT:` / `PROPOSE:` / `CONFLICT:` /
  `DONE:`.
- **Only the owner makes anything `accepted`**; agents answer from the KB, cite paths, and label suggestions.
- `python3 tools/kb.py check` must pass before every commit: a natural KB validator.
- The template ships an **agent-manager** playbook (handing work to coding sessions, worktrees). SpaceMan does not
  import it: control orchestrates workers; KB sessions only answer and propose (SM-KB-006).
- Repository hooks live in `.githooks/` and run only if `core.hooksPath` is set; SpaceMan's acceptance never runs
  repository hooks.

## Using it with SpaceMan

- Repository: the KB folder in the space; knowledge branch: the branch the owner accepts into (not checked out
  anywhere, so SpaceMan may move it).
- Entry instructions: "Follow AGENTS.md. You are a consumer: answer and propose; never mark anything accepted."
- Validator: `python3 tools/kb.py check`.
- Write policy: **review** (a person accepts, matching "only the owner accepts").
