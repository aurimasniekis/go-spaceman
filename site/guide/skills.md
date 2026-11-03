# Skills

A **skill** is guidance for an agent: a `SKILL.md` file, and optionally reference files beside it, that the harness
loads when the skill is relevant. SpaceMan keeps skills in a central, versioned catalog and gives them to runs through
their roles.

::: warning Skills inform, they never grant
A skill can tell an agent how to request access to a server; it cannot give that access. Nothing in a skill runs when
it is installed, and its files are written without execute permission. What an agent may do is still decided by its
tools, accounts and approvals.
:::

<Shot name="skills" alt="Skill catalog listing the built-in HallPass gateway skill and a custom refunds runbook with their latest versions and files" caption="The skill catalog" />

## The catalog

- **Configuration → Skills** lists every skill with its latest published version and files.
- **New skill** starts a draft with a `SKILL.md` template. Its frontmatter `name` must equal the skill's slug.
- You edit the **draft**: change files, add files under folders such as `references/`, remove them. **Publish** turns
  the draft into the next version, which never changes again.
- A skill holds at most 32 text files and 512 KB. Paths must be relative and stay inside the skill.

<Shot name="skill-detail" alt="The HallPass gateway skill open in a drawer with tabs for SKILL.md and references/commands.md, and its published version with checksum" caption="A skill's files and versions" />

SpaceMan ships one built-in skill, **HallPass gateway** (`hallpass`). It is a copy of the skill from go-hallpass, for
agents that reach servers through a HallPass SSH gateway. HallPass itself still enforces who may connect.

## Giving skills to runs

- A **role template** lists its skills. Each one follows the skill's latest published version.
- A **space role** can add more skills for that space.
- **Effective configuration** on a space role shows which skill versions a launch would get.
- Each run's **Configuration at launch** records the slug, version and checksum of every skill it started with. A
  skill without a published version is skipped and listed as not installed.

When a run starts, the node writes its skills into the run's harness folder. Only folders SpaceMan wrote are replaced
or removed (they carry a `.spaceman-managed` marker). A skill you placed there yourself under the same name is never
overwritten; SpaceMan's copy is skipped instead.

## Harness support

| Harness | Skills |
|---|---|
| Claude Code | Written to `$CLAUDE_CONFIG_DIR/skills/<name>/`. Loading verified with Claude Code 2.1.316. |
| Codex | Written to `$CODEX_HOME/skills/<name>/`. Loading verified with codex-cli 0.166.0 (the skill is listed in the model's prompt input). |
