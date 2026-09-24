"""The facts a release depends on, which nothing else checks.

A version that disagrees with itself ships a chart whose `appVersion` pulls an
image tag that was never built. A missing licence file makes the whole thing
legally unusable however permissive the README says it is.
"""

from __future__ import annotations

from pathlib import Path

import pytest
from ruamel.yaml import YAML

ROOT = Path(__file__).resolve().parent.parent
CHART = ROOT / "charts" / "keepsake"


def test_the_version_is_the_same_in_every_place_that_states_it() -> None:
    """`appVersion` is the image tag the chart pulls by default, so a drift from the
    chart's own version is an install that fails on ImagePullBackOff for a tag nobody
    built."""
    chart = YAML(typ="safe").load((CHART / "Chart.yaml").read_text())
    assert chart["version"] == chart["appVersion"], (
        f"Chart.yaml version {chart['version']!r} != appVersion {chart['appVersion']!r}"
    )
    compose = YAML(typ="safe").load((ROOT / "compose.yaml").read_text())
    for name, service in compose["services"].items():
        if "keepsake" in service["image"]:
            assert service["image"].endswith(f":{chart['appVersion']}"), (
                f"compose.yaml {name} pulls {service['image']!r}, not {chart['appVersion']!r}"
            )


def test_the_licence_file_grants_the_licence() -> None:
    """A README line alone is not a grant: with no file, the default is exclusive
    copyright."""
    licence = (ROOT / "LICENSE").read_text()
    assert "MIT License" in licence
    assert "Copyright (c)" in licence


@pytest.mark.parametrize(
    "name",
    [
        "LICENSE",
        "README.md",
        "SECURITY.md",
        "CONTRIBUTING.md",
        "CODE_OF_CONDUCT.md",
        ".github/pull_request_template.md",
        ".github/ISSUE_TEMPLATE/bug_report.yml",
        ".github/dependabot.yml",
    ],
)
def test_the_community_health_files_are_where_github_looks_for_them(name: str) -> None:
    """GitHub's community profile only counts a file in a supported location, so a
    correct CONTRIBUTING in the wrong directory reads as an absent one."""
    assert (ROOT / name).is_file(), f"{name} is missing"
