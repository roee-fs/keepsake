"""The facts a release depends on, which nothing else checks.

A version that disagrees with itself ships a chart whose `appVersion` pulls an
image tag that was never built. A missing licence file makes the whole thing
legally unusable however permissive the README says it is.
"""

from __future__ import annotations

import tomllib
from pathlib import Path
from typing import Any

import pytest
from ruamel.yaml import YAML

ROOT = Path(__file__).resolve().parent.parent
CHART = ROOT / "charts" / "keepsake"


def _pyproject() -> dict[str, Any]:
    return tomllib.loads((ROOT / "pyproject.toml").read_text())


def test_the_version_is_the_same_in_every_place_that_states_it() -> None:
    """Three files carry it and nothing keeps them in step. `appVersion` is the
    image tag the chart pulls by default, so a drift here is an install that fails
    on ImagePullBackOff for a tag nobody built."""
    chart = YAML(typ="safe").load((CHART / "Chart.yaml").read_text())
    project = str(_pyproject()["project"]["version"])
    assert chart["appVersion"] == project, (
        f"Chart.yaml appVersion {chart['appVersion']!r} != "
        f"pyproject version {project!r}"
    )
    assert chart["version"] == project, (
        f"Chart.yaml version {chart['version']!r} != pyproject version {project!r}"
    )


def test_the_licence_is_declared_where_each_consumer_looks() -> None:
    """Three audiences, three places: GitHub and redistributors read the file, a
    wheel's metadata reads pyproject, and a human reads the README. A README line
    alone is not a grant — with no file, the default is exclusive copyright."""
    licence = (ROOT / "LICENSE").read_text()
    assert "MIT License" in licence
    assert "Copyright (c)" in licence

    project = _pyproject()["project"]
    assert project["license"] == "MIT"
    assert "LICENSE" in project["license-files"]


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
