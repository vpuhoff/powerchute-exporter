import re
import subprocess
from pathlib import Path
from tempfile import TemporaryDirectory
import unittest
from unittest import mock

import release


SHA = "a" * 40
VERSION = "0.2.17"


def master_env(**extra):
    env = {
        "GITHUB_REPOSITORY": "vpuhoff/powerchute-exporter",
        "GITHUB_SHA": SHA,
        "GITHUB_EVENT_NAME": "push",
        "GITHUB_REF": "refs/heads/master",
        "GITHUB_RUN_NUMBER": "17",
    }
    env.update(extra)
    return env


def semver(version):
    return tuple(int(part) for part in version.split("."))


def pull(sha=SHA, ref="master", repo="vpuhoff/powerchute-exporter", merged="2026-10-31T00:00:00Z"):
    return {
        "merged_at": merged,
        "merge_commit_sha": sha,
        "base": {"ref": ref, "repo": {"full_name": repo}},
    }


def write_packages(directory, version):
    for names in release.payloads(version).values():
        for name in names:
            (directory / name).write_bytes(b"artifact:" + name.encode())
    for manifest, names in release.payloads(version).items():
        lines = [f"{release.sha256(directory / name)}  {name}" for name in names]
        (directory / manifest).write_bytes(("\n".join(lines) + "\n").encode())


class GitHubFake:
    def __init__(self, sha, names):
        self.sha = sha
        self.names = set(names)
        self.tag_sha = None
        self.target = "master"
        self.resolved = sha
        self.created = False
        self.draft_ready = False
        self.uploaded_names = []
        self.patched = False
        self.patch_args = ()
        self.create_args = ()
        self.published = False
        self.tag_lookups = 0

    def body(self, draft, names):
        return {
            "id": 7,
            "draft": draft,
            "tag_name": "v" + VERSION,
            "target_commitish": self.target,
            "assets": [
                {"id": index + 1, "name": name, "state": "uploaded", "size": 1}
                for index, name in enumerate(sorted(names))
            ],
        }

    def gh_json(self, *args, optional=False, timeout=120):
        path = next(arg for arg in args if arg.startswith(("repos/", "https://")))
        if "/git/ref/tags/" in path:
            if self.tag_sha is None:
                return None
            return {"object": {"type": "commit", "sha": self.tag_sha}}
        if "/commits/" in path:
            return {"sha": self.resolved}
        if "PATCH" in args:
            self.patched = True
            self.patch_args = args
            return self.body(False, self.names)
        if "/releases/tags/" in path:
            # Drafts have no git tag, so this endpoint cannot see them.
            self.tag_lookups += 1
            if self.published:
                return self.body(False, self.names)
            return None
        if "releases?per_page=100" in path:
            if not (self.draft_ready or self.published):
                return [[]]
            listed = self.names if self.published else []
            return [[self.body(not self.published, listed)]]
        if args[1:3] == ("--method", "POST") and path.endswith("/releases"):
            self.created = True
            self.draft_ready = True
            self.create_args = args
            return self.body(True, [])
        if "DELETE" in args and "/releases/assets/" in path:
            return None
        if path.startswith("https://uploads.github.com/") and "/assets?name=" in path:
            self.uploaded_names.append(path.split("name=", 1)[1])
            return {"id": 1, "name": self.uploaded_names[-1], "state": "uploaded", "size": 1}
        if path.endswith("/releases/7"):
            present = self.names if set(self.uploaded_names) == self.names or self.published else set()
            return self.body(not self.published, present)
        raise AssertionError(args)


class VersionTests(unittest.TestCase):
    def test_merge_uses_configured_series(self):
        workflow = Path(__file__).resolve().parents[2] / ".github" / "workflows" / "build.yml"
        found = re.search(r"^ *RELEASE_SERIES: '([^']+)'", workflow.read_text(encoding="utf-8"), re.M)
        self.assertEqual(found.group(1), release.RELEASE_SERIES)
        got = release.versions(master_env(), True)
        self.assertEqual(got["version"], f"{release.RELEASE_SERIES}.17")
        self.assertEqual(got["release_tag"], "v" + got["version"])
        self.assertEqual(got["numeric_version"], got["version"] + ".0")
        self.assertEqual(got["publish_release"], "true")
        self.assertEqual(got["commit_sha"], SHA)
        self.assertGreater(semver(got["version"]), (0, 0, 0))

    def test_other_builds_stay_unpublished(self):
        dev = release.versions(master_env(GITHUB_EVENT_NAME="pull_request", GITHUB_REF="refs/pull/1/merge"), False)
        self.assertEqual(dev["publish_release"], "false")
        self.assertEqual(dev["numeric_version"], "0.0.0.0")
        self.assertTrue(dev["version"].startswith("0.0.0-dev.17+"))
        tagged = release.versions(master_env(GITHUB_REF_TYPE="tag", GITHUB_REF_NAME="v1.2.3", GITHUB_REF="refs/tags/v1.2.3"), False)
        self.assertEqual(tagged["version"], "1.2.3")
        self.assertEqual(tagged["publish_release"], "false")

    def test_windows_version_limit(self):
        with self.assertRaises(ValueError):
            release.versions(master_env(GITHUB_RUN_NUMBER="65536"), True)

    def test_merge_record_must_be_this_repository(self):
        repo = "vpuhoff/powerchute-exporter"
        self.assertTrue(release.merged_into_master([pull(repo="Vpuhoff/Powerchute-exporter")], repo, SHA))
        self.assertFalse(release.merged_into_master([pull(ref="develop")], repo, SHA))
        self.assertFalse(release.merged_into_master([pull(merged=None)], repo, SHA))
        self.assertFalse(release.merged_into_master([pull(sha="b" * 40)], repo, SHA))
        self.assertFalse(release.merged_into_master([pull(repo="other/exporter")], repo, SHA))

    def test_merge_detection_waits_for_the_pull_request(self):
        calls = []

        def fake(*args, optional=False):
            calls.append(args)
            if len(calls) < 2:
                return [[]]
            return [[pull()]]

        with mock.patch("release.gh_json", fake), mock.patch("release.time.sleep") as sleep:
            self.assertTrue(release.detect_merge(master_env()))
        self.assertEqual(sleep.call_count, 1)

    def test_non_master_push_does_not_query_github(self):
        with mock.patch("release.gh_json", side_effect=AssertionError("api")):
            self.assertFalse(release.detect_merge(master_env(GITHUB_REF="refs/heads/feature")))


class AssetTests(unittest.TestCase):
    def setUp(self):
        self.tmp = TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        root = Path(self.tmp.name)
        self.assets = root / "assets"
        self.assets.mkdir()
        self.dashboard = root / "dashboard.json"
        self.dashboard.write_text('{"panels":[{"title":"ups"}]}', encoding="utf-8")
        write_packages(self.assets, VERSION)

    def test_prepare_writes_dashboard_and_complete_sums(self):
        release.prepare_assets(self.assets, VERSION, self.dashboard)
        names = {path.name for path in self.assets.iterdir()}
        self.assertIn("powerchute-exporter-grafana.json", names)
        listed = set()
        for line in (self.assets / "SHA256SUMS").read_text(encoding="utf-8").splitlines():
            digest, name = line.split("  ", 1)
            self.assertEqual(digest, release.sha256(self.assets / name))
            listed.add(name)
        self.assertEqual(listed, names - {"SHA256SUMS"})

    def test_powershell_manifest_is_normalized(self):
        manifest, names = next(iter(release.payloads(VERSION).items()))
        lines = [f"{release.sha256(self.assets / name)}  {name}" for name in names]
        payload = ("\r\n".join(lines) + "\r\n").encode("utf-8")
        (self.assets / manifest).write_bytes(b"\xef\xbb\xbf" + payload)
        release.prepare_assets(self.assets, VERSION, self.dashboard)
        text = (self.assets / manifest).read_bytes()
        self.assertFalse(text.startswith(b"\xef\xbb\xbf"))
        self.assertNotIn(b"\r", text)

    def test_bad_checksum_is_rejected(self):
        manifest = next(iter(release.payloads(VERSION)))
        path = self.assets / manifest
        text = path.read_text(encoding="utf-8")
        path.write_text(("0" if text[0] != "0" else "1") + text[1:], encoding="utf-8")
        with self.assertRaisesRegex(ValueError, "checksum"):
            release.prepare_assets(self.assets, VERSION, self.dashboard)

    def test_unexpected_or_missing_file_is_rejected(self):
        (self.assets / "extra.txt").write_bytes(b"x")
        with self.assertRaisesRegex(ValueError, "unexpected"):
            release.prepare_assets(self.assets, VERSION, self.dashboard)
        (self.assets / "extra.txt").unlink()
        next(path for path in self.assets.iterdir() if path.suffix != ".txt").unlink()
        with self.assertRaisesRegex(ValueError, "incomplete"):
            release.prepare_assets(self.assets, VERSION, self.dashboard)

    def test_dashboard_without_panels_is_rejected(self):
        self.dashboard.write_text("{}", encoding="utf-8")
        with self.assertRaisesRegex(ValueError, "panels"):
            release.prepare_assets(self.assets, VERSION, self.dashboard)


class PublishTests(unittest.TestCase):
    def setUp(self):
        self.tmp = TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        root = Path(self.tmp.name)
        self.assets = root / "assets"
        self.assets.mkdir()
        dashboard = root / "dashboard.json"
        dashboard.write_text('{"panels":[{"title":"ups"}]}', encoding="utf-8")
        write_packages(self.assets, VERSION)
        release.prepare_assets(self.assets, VERSION, dashboard)
        self.env = master_env(PUBLISH_RELEASE="true", VERSION=VERSION, RELEASE_TAG="v" + VERSION, COMMIT_SHA=SHA)
        self.fake = GitHubFake(SHA, {path.name for path in self.assets.iterdir()})

    def publish(self):
        with mock.patch("release.gh_json", self.fake.gh_json), mock.patch("release.gh_run", side_effect=AssertionError("gh release")):
            release.publish_assets(self.env, self.assets)

    def test_branch_target_resolves_and_publish_pins_the_sha(self):
        self.publish()
        self.assertTrue(self.fake.created)
        self.assertIn(f"target_commitish={SHA}", self.fake.create_args)
        self.assertIn("draft=true", self.fake.create_args)
        self.assertEqual(set(self.fake.uploaded_names), self.fake.names)
        self.assertGreater(self.fake.tag_lookups, 0)
        self.assertIn(f"target_commitish={SHA}", self.fake.patch_args)
        self.assertIn("draft=false", self.fake.patch_args)

    def test_existing_draft_is_resumed_when_tag_lookup_misses_it(self):
        self.fake.draft_ready = True
        self.publish()
        self.assertFalse(self.fake.created)
        self.assertEqual(set(self.fake.uploaded_names), self.fake.names)
        self.assertTrue(self.fake.patched)

    def test_moved_branch_does_not_publish(self):
        self.fake.resolved = "c" * 40
        with self.assertRaisesRegex(ValueError, "built commit"):
            self.publish()
        self.assertTrue(self.fake.created)
        self.assertEqual(self.fake.uploaded_names, [])
        self.assertFalse(self.fake.patched)

    def test_existing_tag_on_another_commit_is_not_moved(self):
        self.fake.tag_sha = "c" * 40
        with self.assertRaisesRegex(ValueError, "another commit"):
            self.publish()
        self.assertFalse(self.fake.created)

    def test_published_release_is_left_in_place(self):
        self.fake.tag_sha = SHA
        self.fake.published = True
        self.publish()
        self.assertFalse(self.fake.created)
        self.assertFalse(self.fake.patched)

    def test_manifest_must_list_every_asset(self):
        path = self.assets / "SHA256SUMS"
        lines = path.read_text(encoding="utf-8").splitlines()
        path.write_bytes(("\n".join(lines[:-1]) + "\n").encode())
        with mock.patch("release.gh_json", side_effect=AssertionError("api")):
            with self.assertRaisesRegex(ValueError, "changed"):
                release.publish_assets(self.env, self.assets)

    def test_unverified_push_cannot_publish(self):
        self.env["PUBLISH_RELEASE"] = "false"
        with self.assertRaisesRegex(ValueError, "verified merge"):
            release.publish_assets(self.env, self.assets)


class CommitAndErrorTests(unittest.TestCase):
    def test_full_sha_is_not_resolved_again(self):
        with mock.patch("release.gh_json", side_effect=AssertionError("called")):
            self.assertEqual(release.commit_sha("vpuhoff/powerchute-exporter", SHA), SHA)

    def test_branch_commitish_uses_the_commits_api(self):
        seen = []

        def fake(*args, optional=False):
            seen.append(args)
            return {"sha": SHA}

        with mock.patch("release.gh_json", fake):
            self.assertEqual(release.commit_sha("vpuhoff/powerchute-exporter", "master"), SHA)
        self.assertIn("commits/master", seen[0][1])

    def test_commitish_cannot_escape_the_api_path(self):
        with mock.patch("release.gh_json", side_effect=AssertionError("called")):
            with self.assertRaises(ValueError):
                release.commit_sha("vpuhoff/powerchute-exporter", "../master")

    def test_github_error_includes_stderr(self):
        failed = subprocess.CompletedProcess(["gh"], 1, "", "HTTP 422: tag exists")
        with mock.patch("release.subprocess.run", return_value=failed):
            with self.assertRaisesRegex(RuntimeError, "HTTP 422"):
                release.gh_run("release", "create", "v0.2.17")

    def test_optional_404_is_absence(self):
        missing = subprocess.CompletedProcess(["gh"], 1, "", "gh: HTTP 404: Not Found")
        with mock.patch("release.subprocess.run", return_value=missing):
            self.assertIsNone(release.gh_json("api", "repos/a/b/git/ref/tags/v0.2.17", optional=True))


if __name__ == "__main__":
    unittest.main()
