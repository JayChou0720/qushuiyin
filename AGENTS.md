# AGENTS.md

Guidance for coding agents (Codex and others) working in this repository.

Before doing anything, read these files in full, in this order:

1. `CLAUDE.md`: architecture summary, stack, invariants, and the rules for the parse-video dependency. It applies to every agent, not just Claude.
2. `03-去水印小程序-开发执行说明.md`: how to execute: repository layout, engineering conventions, local development and mocks, execution order, and the steps only the project owner can do (H1–H7).
3. `02-去水印小程序-功能说明与开发规格.md`: the authoritative spec. If anything conflicts, doc 02 wins.
4. `01-去水印小程序-功能大纲与开发路线.md`: scope, milestones and work packages.

Hard rules (details are in the files above):

- The repository root is the only git repository. Do not create nested repositories, do not touch `D:\Projects\去水印-backup\`, and only push to GitHub when the owner asks (doc 03 §2).
- Never modify the parse-video library source; never commit secrets; never hardcode values that belong in spec §16.
- When the spec is unclear, take the most conservative option, keep going, and record it in `open-questions.md` as doc 03 §1 describes.
