# Roles and prompts

What an agent is told and how it runs comes from a few central catalogs, combined per space. Every run records the
exact result in its manifest, so a later change never rewrites what a past run used.

## Prompts

**Prompts** hold the instructions for roles, dreams, knowledge bases and events, in one versioning system.

<Shot name="prompts" alt="Prompt catalog with engineering base, control and reviewer prompts, their kinds and latest versions" caption="The prompt catalog" />

- You edit a **draft**. **Publish** turns it into the next version, which never changes again.
- A prompt can **extend** a published version of another prompt; that prompt's content comes first. A control prompt
  can build on a shared engineering base, for example.
- **Fork** copies a published version into a new prompt that records where it came from. Forks never merge back
  automatically.
- **Diff** compares any two versions line by line.
- **Roll back** publishes an earlier version's content as a new version: history only grows.

<Shot name="prompt-detail" alt="Control prompt with an empty new draft and two published versions with Diff, Roll back and Fork actions" caption="A prompt's draft and versions" />

<Shot name="prompts-diff" alt="Diff between version 1 and 2 of the control prompt, with the added production rule highlighted" caption="Comparing two versions" />

## Harness profiles and role templates

A **harness profile** says how a run executes: the harness (Claude Code or Codex), the model and settings such as
Claude Code's permission mode. A **role template** says what a role is: its run kind (control, worker, …), its prompt
(following the latest version, or pinned to one), its memory conventions and its default profile.

<Shot name="roles" alt="Role templates for control and reviewer with their prompts and profiles" caption="Role templates" />

Editing a profile or template creates a new **revision**; it applies to launches from then on.

Templates can also give a role [skills](./skills): guidance files the harness loads.

## Space roles

A space configures the roles it uses from these templates on its **Overview**. A space role can override the profile,
the prompt (or its version), the model and settings, and add instructions that apply only in this space. **Effective
configuration** shows what a launch of the role gets now and where each value comes from.

<Shot name="space-role-preview" alt="Effective configuration of the control role: harness from the profile, kind from the template, permission mode from the profile, and the composed system text" caption="A role's effective configuration" />

Launching a run with a configured role needs only the role's name; its template, profile and prompt fill in the rest.

## How values are combined

Layers apply in this order, each overriding the previous one:

1. SpaceMan's built-in defaults;
2. the harness profile;
3. the role template;
4. the space role;
5. the launch itself (a model, settings or extra instructions given when launching).

Field by field:

| Field | Rule |
|---|---|
| Run kind | From the role template. A launch cannot change it. |
| Harness | From the profile, otherwise from the launch. A launch cannot contradict the profile. |
| Model | The last layer that sets one wins. |
| Settings | Merged key by key; the last layer that sets a key wins. |
| System text | Concatenated in order: SpaceMan's orientation, the role prompt, the template's memory conventions, the space's goals and standing instructions, the space role's instructions, then the launch's text. |

**Policy ceilings** apply last, whatever layer set the value. The central setting `harness.max_permission_mode`
(default `auto`) is the most permissive Claude Code permission mode any run gets; a higher mode is lowered to it, and
the run's configuration shows that the policy decided.

## What each run records

Each run stores a manifest of what it started with: the prompt version and its content, the profile, template and
space-role revisions, every effective value with its origin, the account reference, the versions of SpaceMan, the
node agent, the runner and the harness, and the policy version. Secrets are never part of it. The run page shows it
under **Configuration at launch**.

<Shot name="run-manifest" alt="A run's configuration at launch: role, template, profile and prompt versions with effective values and dependency versions" caption="A run's configuration at launch" />
