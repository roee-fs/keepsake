# Follow-ups

Known debt carried out of the v1 build. Nothing here is a correctness defect in
the isolation or round-trip guarantees.

## Open

- **Revision retention.** Still unimplemented, and still the item that only gets
  harder: `docs/design.md` notes the policy must be decided while
  `concept_revision` is nearly empty. Migration 0002 added the index that a
  pruning pass would need, but the policy — how long, pruned by what, and whether
  an export has to happen first — is a decision, not a patch.
- **Revisions outlive their concept.** Deleting a row from `concept` leaves its
  `concept_revision` rows behind, and re-creating that path then fails on
  `(tenant_id, path, version)` — the version restarts at 1. No tool deletes, so
  an agent cannot reach this today; a delete tool has to deal with it, either by
  cascading or by continuing the version sequence.
- **`test_verify.py`'s catalog-shadowing decoys detect via an exception** rather
  than the planted value. Real today; vacuous if `raw()` ever sets the GUC.
- **`POOL_SIZE` and `KEEPSAKE_PORT` are read at import.** Both now refuse a bad
  value legibly, but a config error still surfaces as a crash at startup rather
  than as a validated setting. The chart's `values.schema.json` catches the same
  mistakes earlier, so this only bites a deployment that is not the chart.

## Operability

- The chart sets no `nodeSelector`, `tolerations` or `affinity` passthrough. A
  `topologySpreadConstraint` covers the case that mattered; the rest is
  boilerplate until someone needs it.
- `docs/design.md` describes the store but not the operational surface the
  harness established — `/readyz`, the pool's reconnect bound, the worker-thread
  model. Worth one pass when the design doc is next touched.

## CI

- `e2e.yaml` runs on every pull request and takes several minutes. Worth a path
  filter once the repo has traffic.

## Making this an open-source project rather than a public repository

Assessed against [GitHub's community profile][gh], the [OpenSSF Scorecard
checks][sc] and the [OpenSSF Best Practices passing badge][bp]. Most of it now
exists; what remains needs either a decision or repository-admin rights.

[gh]: https://docs.github.com/en/communities/setting-up-your-project-for-healthy-contributions/about-community-profiles-for-public-repositories
[sc]: https://github.com/ossf/scorecard/blob/main/docs/checks.md
[bp]: https://www.bestpractices.dev/en/criteria/0

### Needs a human

- **`CODE_OF_CONDUCT.md` has no enforcement address.** It points at the
  repository's maintainers and at GitHub's own abuse reporting, which works today
  and needs no inbox — but a real mailbox is better, because it gives a reporter
  somewhere to go that is not the platform the conduct happened on. Add one when
  one exists, and only then: a harassment report that bounces is worse than no
  policy at all.
- **Moving the repository is a release-breaking change.** A GHCR namespace
  follows the repository owner, so a transfer silently makes a hardcoded one a
  namespace the workflow's token cannot push to. The release derives
  `ghcr.io/<owner>/keepsake` rather than naming it, and refuses to publish when
  the chart's default image disagrees — so after a move,
  `charts/keepsake/values.yaml` is the one value to change and a failed check
  names it. This bit once already.
- **Enable GitHub private vulnerability reporting** in Settings → Security.
  `SECURITY.md` and the issue-template chooser both link to the advisory form;
  until the setting is on, those links 404.

### Not done yet

- **Nothing publishes `okf-core`.** The release workflow covers the image, the
  chart, the SBOM and the GitHub Release, but the leaf package the import-linter
  contract exists to keep publishable is still not on PyPI. It needs its own
  `pyproject.toml` and a trusted-publisher configuration, which is a decision
  about whether that package is a product or an internal boundary.
- **The release workflow has never run.** It lints clean, the version and
  changelog gates run before anything is pushed, and the GitHub Release is
  created last so it cannot name an artefact that failed — but the first `v*` tag
  is still the first real test. Push a throwaway tag on a fork first if that
  matters.
- **No image is built on `main`.** Only tags publish, so there is no
  `:main` or `:sha` image to test an unreleased commit against a real cluster
  without building it yourself. Worth adding once the tag path has run once.
- **Nothing scans the built image.** osv-scanner reads `uv.lock`, which covers
  the Python tree but not the base image's own packages. Trivy or Grype on the
  pushed digest would close that.
- **No OpenSSF Scorecard workflow.** Running it in CI turns all of the above into
  a number that moves, rather than a document that goes stale. Worth adding once
  branch protection is set, so the first score is not misleading.
- **No Best Practices badge.** Worth applying for once the repo is public; the
  passing criteria are mostly met now, and the ones that are not (release notes
  for fixed vulnerabilities, a documented bug-reporting response record) need a
  history the project does not have yet.

### Judgement calls, not omissions

- **Fuzzing** is a Scorecard check keepsake will not score on. The parser is the
  only plausible target, and `okf_core` is small, pure and already
  property-shaped; Hypothesis over `parse`/`serialize` would be a better use of
  the effort than OSS-Fuzz.
- **Contributors-from-3-companies** is a trust signal, not an action. It will be
  false until the project has outside users, and nothing should be done about it.
- **`Maintained`** cannot pass until the repo is 90 days old. Informational only.

### Done

- **The `main` ruleset now enforces something.** It existed but was
  `enforcement: disabled` with an empty `ref_name.include`, so even switched on
  it would have matched no branch — and its pull-request rule asked for zero
  approving reviews. It is now active on `~DEFAULT_BRANCH`, requires one approving
  review with stale reviews dismissed on push, and gates merges on `check`,
  `kind`, `codeql` and `osv` under a strict up-to-date policy. No bypass actors,
  which is what Scorecard wants and which also means a solo maintainer needs a
  second reviewer; set enforcement to `evaluate` if that ever has to give.
- `LICENSE` (MIT) at the top level, `license` and `license-files` in
  `pyproject.toml` so the wheel carries the terms, and a test holding all three
  in agreement. Verified: the built wheel contains
  `dist-info/licenses/LICENSE`.
- `SECURITY.md`, with a private channel, a 14-day response commitment, and an
  explicit scope — including what is *not* a vulnerability, so that the documented
  absence of authentication does not arrive as a finding every month.
- `CONTRIBUTING.md`, `CODE_OF_CONDUCT.md`, issue templates, a pull-request
  template, and a template chooser that closes the blank-issue escape hatch and
  points security reports at the advisory form.
- `CHANGELOG.md`, and a test pinning `pyproject.toml` against `Chart.yaml`'s
  `version` and `appVersion` — a drift there ships a chart whose default image
  tag was never built.
- `.github/dependabot.yml` for actions, uv and Docker. Pinning without an updater
  only produces stale pins, which is why Scorecard checks for both.
- `security.yaml`: CodeQL on the `security-and-quality` suite, and osv-scanner
  against `uv.lock`, on pull requests and weekly. The weekly run is the point —
  a dependency becomes vulnerable without anyone touching this repository. OSV
  reports no findings as of this commit.
- `release.yaml`: tag-driven, refusing to publish when the tag disagrees with the
  tree, then pushing the image with SLSA provenance, the chart to OCI, and a
  CycloneDX SBOM. All four workflows pass `actionlint`.
