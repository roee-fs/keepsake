<!--
Thanks for this. CONTRIBUTING.md has the setup and the checks CI runs.
Delete any section that does not apply rather than writing "n/a" in it.
-->

## What this changes

<!-- And why. If it fixes an issue, "Fixes #123". -->

## How it was verified

<!--
Which tests you added or changed, and what they would catch. If you ran e2e/,
say so. "Tests pass" is not a verification — every test passed before, too.
-->

## Things a reviewer should know

<!--
Anything the diff cannot show: a measurement, an alternative you rejected and
why, a limit you accepted deliberately.
-->

---

- [ ] `uv run ruff check . && uv run ruff format --check .`
- [ ] `uv run ty check src tests e2e`
- [ ] `uv run lint-imports` — the layering contracts still hold
- [ ] `uv run pytest`
- [ ] If this changes the schema, it is a **new** migration and `helm upgrade`
      re-running the hook against a database already at head is fine
