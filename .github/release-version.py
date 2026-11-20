"""Validate release tags and select the highest stable GitHub Release."""

import json
import os
import re
import sys
from pathlib import Path


NUMBER = r"(0|[1-9][0-9]*)"
IDENTIFIER = r"(?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)"
SEMVER = re.compile(
    rf"v{NUMBER}\.{NUMBER}\.{NUMBER}"
    rf"(?:-({IDENTIFIER}(?:\.{IDENTIFIER})*))?"
    r"(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?"
)


def stable_releases(pages):
    for page in pages:
        for release in page:
            tag = release["tag_name"]
            version = SEMVER.fullmatch(tag)
            if version and not version[4] and not release["draft"] and not release["prerelease"]:
                yield (tuple(map(int, version.groups()[:3])), release["published_at"], tag)


if len(sys.argv) == 2:
    pages = json.loads(Path(sys.argv[1]).read_text())
    print(max(stable_releases(pages), default=((), "", ""))[2])
else:
    tag = os.environ["RELEASE_TAG"]
    version = SEMVER.fullmatch(tag)
    image_tag = tag.replace("+", "_")  # Registry tags cannot contain a plus sign.
    if not version or len(image_tag) > 128:
        sys.exit("Expected a v-prefixed SemVer release tag that fits in a container tag")
    stable = not version[4] and os.environ["PRERELEASE"] == "false"
    with Path(os.environ["GITHUB_OUTPUT"]).open("a") as output:
        print(f"tag={image_tag}", file=output)
        print(f"stable={str(stable).lower()}", file=output)
