"""Release metadata, artifact verification and draft-first publishing (stdlib only)."""

import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import time
from urllib.parse import quote

RELEASE_SERIES = "0.2"


def github_failure(result, fallback):
    detail = (result.stderr or result.stdout or "").strip()
    if detail:
        return RuntimeError(f"{fallback}: {detail}")
    return RuntimeError(fallback)


def gh_json(*args, optional=False, timeout=120):
    result = subprocess.run(["gh", *args], capture_output=True, text=True, timeout=timeout)
    if result.returncode:
        if optional and "HTTP 404" in (result.stderr or ""):
            return None
        request = " ".join(args)
        raise github_failure(result, f"GitHub CLI request failed for '{request}'; no release was published by this step")
    return json.loads(result.stdout) if result.stdout.strip() else None


def gh_run(*args):
    result = subprocess.run(["gh", *args], capture_output=True, text=True, timeout=300)
    if result.returncode:
        raise github_failure(result, "GitHub release command failed; rerun the workflow to resume its draft")


def context(env):
    repo, sha = env["GITHUB_REPOSITORY"], env["GITHUB_SHA"]
    if not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", repo):
        raise ValueError("invalid repository")
    if not re.fullmatch(r"[0-9a-f]{40}", sha):
        raise ValueError("invalid commit SHA")
    return repo, sha


def merged_into_master(pulls, repo, sha):
    return any(
        pull.get("merged_at")
        and pull.get("merge_commit_sha") == sha
        and pull.get("base", {}).get("ref") == "master"
        and pull.get("base", {}).get("repo", {}).get("full_name", "").lower() == repo.lower()
        for pull in pulls
    )


def detect_merge(env):
    if env.get("GITHUB_EVENT_NAME") != "push" or env.get("GITHUB_REF") != "refs/heads/master":
        return False
    repo, sha = context(env)
    # The push event can arrive before GitHub finishes updating the PR record.
    for attempt in range(5):
        pages = gh_json("api", f"repos/{repo}/commits/{sha}/pulls?per_page=100", "--paginate", "--slurp")
        pulls = [pull for page in pages for pull in page]
        if merged_into_master(pulls, repo, sha):
            return True
        if attempt < 4:
            time.sleep(2)
    return False


def versions(env, merged):
    _, sha = context(env)
    publish = merged and env.get("GITHUB_EVENT_NAME") == "push" and env.get("GITHUB_REF") == "refs/heads/master"
    if publish:
        series = env.get("RELEASE_SERIES", RELEASE_SERIES)
        run = env["GITHUB_RUN_NUMBER"]
        if not re.fullmatch(r"(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)", series) or not re.fullmatch(r"[1-9][0-9]*", run):
            raise ValueError("invalid automatic release version")
        version = f"{series}.{run}"
    elif env.get("GITHUB_REF_TYPE") == "tag" and re.fullmatch(r"v[0-9]+\.[0-9]+\.[0-9]+", env.get("GITHUB_REF_NAME", "")):
        version = env["GITHUB_REF_NAME"][1:]
    else:
        version = f"0.0.0-dev.{env['GITHUB_RUN_NUMBER']}+{sha[:12]}"
    numeric = version.split("-", 1)[0] if "-" not in version else "0.0.0"
    if any(int(part) > 65535 for part in numeric.split(".")):
        raise ValueError("version exceeds Windows installer numeric limits; bump RELEASE_SERIES")
    return {"version": version, "numeric_version": numeric + ".0", "release_tag": "v" + version,
            "publish_release": str(publish).lower(), "commit_sha": sha}


def payloads(version):
    if not re.fullmatch(r"[0-9]+\.[0-9]+\.[0-9]+", version):
        raise ValueError("release version must be a stable numeric version")
    prefix = f"powerchute-exporter_{version}"
    return {
        "checksums-linux-amd64.txt": [f"{prefix}_linux_amd64.{ext}" for ext in ("deb", "rpm", "tar.gz")],
        "checksums-linux-arm64.txt": [f"{prefix}_linux_arm64.{ext}" for ext in ("deb", "rpm", "tar.gz")],
        "checksums-windows-amd64.txt": [f"{prefix}_windows_amd64_setup.exe", f"{prefix}_windows_amd64.zip"],
    }


def sha256(path):
    result = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            result.update(chunk)
    return result.hexdigest()


def prepare_assets(directory, version, dashboard):
    groups = payloads(version)
    required = set(groups) | {name for names in groups.values() for name in names}
    actual = {path.name for path in directory.iterdir()}
    if actual != required:
        raise ValueError("release artifacts are incomplete or contain unexpected files")
    if any((directory / name).is_symlink() or not (directory / name).is_file() or (directory / name).stat().st_size == 0 for name in required):
        raise ValueError("release artifacts must be nonempty regular files")
    for manifest, expected in groups.items():
        seen = set()
        lines = (directory / manifest).read_text(encoding="utf-8-sig").splitlines()
        for line in lines:
            match = re.fullmatch(r"([0-9a-fA-F]{64})\s+\*?([^\r\n]+)", line)
            if not match:
                raise ValueError("invalid checksum manifest")
            digest, name = match.groups()
            if name not in expected or name in seen or sha256(directory / name) != digest.lower():
                raise ValueError("artifact checksum or manifest contents do not match")
            seen.add(name)
        if seen != set(expected):
            raise ValueError("checksum manifest omits a required artifact")
        # PowerShell manifests may contain BOM/CRLF; publish normalized UTF-8/LF.
        (directory / manifest).write_bytes(("\n".join(lines) + "\n").encode("utf-8"))
    model = json.loads(dashboard.read_text(encoding="utf-8"))
    if not model.get("panels"):
        raise ValueError("Grafana dashboard has no panels")
    shutil.copyfile(dashboard, directory / "powerchute-exporter-grafana.json")
    files = sorted(directory.iterdir())
    sums = "".join(f"{sha256(path)}  {path.name}\n" for path in files)
    (directory / "SHA256SUMS").write_bytes(sums.encode("utf-8"))


def tag_commit(repo, tag):
    ref = gh_json("api", f"repos/{repo}/git/ref/tags/{tag}", optional=True)
    if ref is None:
        return None
    obj = ref["object"]
    for _ in range(5):
        if obj["type"] == "commit":
            return obj["sha"]
        if obj["type"] != "tag":
            break
        obj = gh_json("api", f"repos/{repo}/git/tags/{obj['sha']}")["object"]
    raise ValueError("release tag does not resolve to a commit")


def numeric_id(value, label):
    if isinstance(value, bool) or not isinstance(value, int) or value < 1:
        raise ValueError(f"invalid {label}")
    return value


def find_release(repo, tag):
    """Published releases are visible by tag. Drafts are not, because the git tag does not exist yet."""
    if not re.fullmatch(r"v[0-9]+\.[0-9]+\.[0-9]+", tag):
        raise ValueError("invalid release tag")
    published = gh_json("api", f"repos/{repo}/releases/tags/{tag}", optional=True)
    if published is not None:
        return published
    pages = gh_json("api", f"repos/{repo}/releases?per_page=100", "--paginate", "--slurp") or []
    if not isinstance(pages, list):
        raise ValueError("release list was not returned")
    matches = []
    for page in pages:
        rows = page if isinstance(page, list) else [page]
        matches.extend(item for item in rows if isinstance(item, dict) and item.get("tag_name") == tag)
    if len(matches) > 1:
        raise ValueError("multiple releases share this tag")
    return matches[0] if matches else None


def create_draft(repo, tag, sha):
    created = gh_json(
        "api", "--method", "POST", f"repos/{repo}/releases",
        "-f", f"tag_name={tag}", "-f", f"target_commitish={sha}", "-f", f"name={tag}",
        "-F", "draft=true", "-F", "generate_release_notes=true",
    )
    if not isinstance(created, dict) or not created.get("draft") or created.get("tag_name") != tag:
        raise ValueError("draft release was not created")
    return created


def upload_assets(repo, release, directory, names):
    release_id = numeric_id(release.get("id"), "release id")
    seen = {}
    for asset in release.get("assets") or []:
        name = asset.get("name")
        if name in seen:
            raise ValueError("draft contains a duplicate asset name")
        seen[name] = asset
    if not set(seen) <= set(names):
        raise ValueError("draft contains unverified extra assets; refusing to publish")
    for name in sorted(names):
        asset = seen.get(name)
        if asset is not None:
            gh_json("api", "--method", "DELETE", f"repos/{repo}/releases/assets/{numeric_id(asset.get('id'), 'release asset')}")
        gh_json(
            "api", "--method", "POST", "-H", "Content-Type: application/octet-stream",
            f"https://uploads.github.com/repos/{repo}/releases/{release_id}/assets?name={quote(name)}",
            "--input", str(directory / name),
            timeout=300,
        )
    uploaded = gh_json("api", f"repos/{repo}/releases/{release_id}")
    return release_id, uploaded


def commit_sha(repo, commitish):
    """Resolve a release target to a commit. A branch name is not a SHA."""
    if re.fullmatch(r"[0-9a-f]{40}", commitish):
        return commitish
    if (
        not re.fullmatch(r"[A-Za-z0-9._/-]{1,255}", commitish)
        or commitish.startswith(("/", "-"))
        or any(part in ("", ".", "..") for part in commitish.split("/"))
    ):
        raise ValueError("invalid commitish")
    body = gh_json("api", f"repos/{repo}/commits/{quote(commitish, safe='')}")
    sha = body.get("sha") if isinstance(body, dict) else None
    if not isinstance(sha, str) or not re.fullmatch(r"[0-9a-f]{40}", sha):
        raise ValueError("commitish did not resolve to a commit")
    return sha


def publish_assets(env, directory):
    if env.get("GITHUB_EVENT_NAME") != "push" or env.get("GITHUB_REF") != "refs/heads/master" or env.get("PUBLISH_RELEASE") != "true":
        raise ValueError("releases are allowed only for a verified merge into master")
    repo, sha = context(env)
    version, tag = env["VERSION"], env["RELEASE_TAG"]
    if tag != "v" + version or env["COMMIT_SHA"] != sha:
        raise ValueError("release metadata differs from the built commit")
    groups = payloads(version)
    expected = set(groups) | {name for names in groups.values() for name in names} | {"SHA256SUMS", "powerchute-exporter-grafana.json"}
    if {path.name for path in directory.iterdir()} != expected:
        raise ValueError("prepared release assets are incomplete")
    # Recheck the combined manifest immediately before any GitHub writes.
    covered = set()
    for line in (directory / "SHA256SUMS").read_text(encoding="utf-8").splitlines():
        digest, separator, name = line.partition("  ")
        if separator != "  " or name not in expected - {"SHA256SUMS"} or name in covered or sha256(directory / name) != digest:
            raise ValueError("prepared release assets changed")
        covered.add(name)
    if covered != expected - {"SHA256SUMS"}:
        raise ValueError("prepared release assets changed")
    existing_commit = tag_commit(repo, tag)
    if existing_commit is not None and existing_commit != sha:
        raise ValueError("release tag already belongs to another commit; refusing to move it")
    release = find_release(repo, tag)
    if release is not None and not release["draft"]:
        if existing_commit != sha or not expected <= {asset["name"] for asset in release["assets"]}:
            raise ValueError("published release does not match the expected commit/assets")
        print(f"Release {tag} is already published; existing assets are preserved")
        return
    if release is None:
        release = create_draft(repo, tag, sha)
    if not release.get("draft") or commit_sha(repo, release["target_commitish"]) != sha:
        raise ValueError("existing draft does not belong to the built commit")
    release_id, uploaded = upload_assets(repo, release, directory, expected)
    if {asset["name"] for asset in uploaded["assets"]} != expected or any(asset["state"] != "uploaded" or asset["size"] == 0 for asset in uploaded["assets"]):
        raise ValueError("draft asset upload is incomplete; draft remains unpublished")
    # GitHub's legacy policy selects Latest by semantic version and creation
    # date, so an older run finishing late need not replace a newer version.
    # Pin the tag to the built SHA. A draft often reports target_commitish as the branch name.
    gh_json(
        "api", "--method", "PATCH", f"repos/{repo}/releases/{release_id}",
        "-f", f"target_commitish={sha}", "-F", "draft=false", "-f", "make_latest=legacy",
    )
    print(f"Published {tag} for {sha}")


def main():
    command = sys.argv[1]
    if command == "metadata":
        values = versions(os.environ, detect_merge(os.environ))
        with open(os.environ["GITHUB_OUTPUT"], "a", encoding="utf-8") as output:
            for key, value in values.items():
                output.write(f"{key}={value}\n")
        print(f"Version: {values['version']}; publish release: {values['publish_release']}")
    elif command == "prepare":
        prepare_assets(Path(sys.argv[2]), os.environ["VERSION"], Path("grafana/powerchute-exporter.json"))
    elif command == "publish":
        publish_assets(os.environ, Path(sys.argv[2]))
    else:
        raise ValueError("unknown release command")


if __name__ == "__main__":
    try:
        main()
    except (ValueError, RuntimeError, OSError, subprocess.TimeoutExpired) as error:
        print(f"Release step failed: {error}", file=sys.stderr)
        sys.exit(1)
