---
name: session-handoff
description: Pick up work from HANDOFF.md at the start of a session and update it at the end. Use at the start of every session in gitopsify (and its provider repos), when the user says "continue", "pick up where you left off" or "what is the status", before context runs out, and always before you finish a session, even when the work is unfinished.
---

# Session handoff

`HANDOFF.md` at the repository root is the memory between sessions. Any agent (or
person) must be able to continue the work from it alone, without this
conversation. This skill is for people working **on** gitopsify; it is not one of
the user-facing skills in `skills/` that ship with the plugin and the binary.

## At the start of a session

1. Read `HANDOFF.md` completely.
2. Check that what it describes is still true before you rely on it:
   - Tree: `git status`, `git log --oneline -5`, current branch.
   - CI: `gh run list --limit 5` (is `main` green?).
   - Releases: `gh release list --limit 3` against the version the handoff names.
   - Hetzner (costs money): list every server, without printing the token:
     `set -a; . ./.env; set +a; curl -fsS -H "Authorization: Bearer $TF_VAR_hcloud_token" https://api.hetzner.cloud/v1/servers | jq -r '.servers[] | "\(.name) \(.labels)"'`.
     Test resources carry the label `gitopsify-e2e` or `gitopsify-e2e-runner`
     (`./test/live/hetzner/run.sh destroy` and `e2e-on-vm.sh destroy` remove
     them). The handoff should say "zero servers" unless something is meant to
     be running. Other servers on the account belong to other projects: leave
     them alone.
   - Sibling repos the handoff names (for example `../gitopsify-provider-grafana`):
     their `git status` and their own `HANDOFF.md`.
3. Tell the user in one or two sentences what the last session left open, then
   continue with the request. If the request conflicts with an open item, say so.

## At the end of a session

Update `HANDOFF.md` before your final message, even when the work is unfinished
or the session was short. Keep it short and factual. Replace stale content
instead of appending a diary: the file describes the state now, not the history
(git has the history).

Keep these sections current:

- **Header**: "State as of YYYY-MM-DD" (absolute date), the latest release, whether
  everything is committed and pushed, and CI state.
- **What it is / Where things are**: update only when structure changed (new
  packages, commands, repos, workflows).
- **Day to day**: commands that changed or were added.
- **Environment rules**: anything learned the hard way this session (machine
  limits, flaky links, cloud quirks). Never paste secret values; name the file
  and the variable (for example "`.env` holds `TF_VAR_hcloud_token`").
- **Verified**: what was tested and how (the command and its result), with the
  date. Remove claims that no longer hold.
- **Gotchas**: new upstream or tool behaviour that cost time.
- **Open / next**: unfinished work in priority order, with enough detail to start
  without asking. Include review findings that were not fixed, and why.

Before you write it:

1. Make sure the session's changes were reviewed by a fresh-context reviewer
   subagent (for example `agent-skills:code-reviewer`) and that findings were
   fixed or recorded under **Open / next** (or in `docs/DESIGN.md` known
   limitations when they can't be fixed).
2. Make sure nothing billable is left running on Hetzner unless the handoff says
   so on purpose (and says how to destroy it).
3. Run `make lint test` if code changed, and record the result.

Never write passwords, tokens, keys or other secret values into `HANDOFF.md`. It
is committed to git. Commit it with the session's work ("Update the handoff")
when the user asks for commits; otherwise leave it changed in the working tree
and say so.
