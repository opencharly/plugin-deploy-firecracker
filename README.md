# plugin-deploy-firecracker

The **`deploy:firecracker` substrate plugin** — a Firecracker microVM deploy target for charly,
beside `plugin-deploy-vm` and `plugin-deploy-pod`.

## Why this repo exists

`plugin-firecracker` owns the *behaviour* of the substrate (`verb:firecracker` for a plan step,
`command:firecracker` for the CLI) and deliberately declares **two** classes, one of which must
serve both. It does **not** own the deploy target, and its own `AGENTS.md` says why: *"one provider
must serve BOTH classes"*, and a second claimant for a word is the duplication R2 forbids.

A check written into that repo makes the point mechanically —
`TestDeclaredWordsMatchServedClasses` fails when the candy's `plugin.providers:` claims a class the
binary does not serve. It caught exactly that: `deploy:firecracker` declared in a plugin that serves
`command` and `verb` only. A provider word is a claim the registry hands out, so a word nothing
serves is a **route to a refusal** — worse than an absent word, because it is published in the
generated index.

This repo is where that word belongs: the deploy target is its own provider, as it is for every
other substrate.

## Status

Scaffolded. The candy entity, its CUE schema and the deploy-target provider follow; until then
`deploy:firecracker` is unserved everywhere, and `charly#916` tracks the reachability gap that
makes the three dependent beds red.

See `opencharly/opencharly` for the umbrella rulebook.
