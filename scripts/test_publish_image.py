import os
from pathlib import Path
import shutil
import subprocess
import tarfile
import tempfile
import unittest


SCRIPT = Path(__file__).with_name("publish-image.sh")
DOCKERFILE = SCRIPT.parent.parent / "Dockerfile"
REVISION = "0123456789abcdef0123456789abcdef01234567"

FAKE_GIT = """#!/bin/sh
case "$*" in
  *"rev-parse --is-inside-work-tree"*) printf '%s\\n' "$FAKE_WORKTREE" ;;
  *"status --porcelain=v1 --untracked-files=normal"*) printf '%s' "$FAKE_STATUS"; exit "$FAKE_STATUS_RC" ;;
  *"archive --format=tar --output="*)
    archive_out=
    for arg in "$@"; do
      case "$arg" in --output=*) archive_out=${arg#--output=} ;; esac
    done
    [ -n "$archive_out" ] || { echo 'fake git: missing archive output' >&2; exit 1; }
    [ "${FAKE_ARCHIVE_RC:-0}" -eq 0 ] || exit "$FAKE_ARCHIVE_RC"
    if [ "${FAKE_ARCHIVE_EMPTY:-0}" -eq 0 ]; then
      printf 'fake tar archive\\n' > "$archive_out"
    else
      : > "$archive_out"
    fi
    ;;
  *"rev-parse HEAD"*) printf '%s\\n' "$FAKE_REVISION"; exit "$FAKE_REVISION_RC" ;;
  *) printf 'unexpected git invocation: %s\\n' "$*" >&2; exit 1 ;;
esac
"""


class PublishImageTests(unittest.TestCase):
    def test_dockerfile_has_reproducible_oci_metadata(self):
        text = DOCKERFILE.read_text()
        self.assertIn(
            "# syntax=docker/dockerfile:1.7@sha256:a57df69d0ea827fb7266491f2813635de6f17269be881f696fbfdf2d83dda33e",
            text,
        )
        for needle in [
            "ARG VERSION=dev",
            "ARG REVISION=unknown",
            'org.opencontainers.image.version="$VERSION"',
            'org.opencontainers.image.revision="$REVISION"',
            'org.opencontainers.image.source="https://github.com/mshagirov/kubeflow-admin"',
            'org.opencontainers.image.licenses="MIT"',
        ]:
            self.assertIn(needle, text)
        self.assertNotIn("org.opencontainers.image.created", text)

    def invoke(self, image, *, status="", worktree="true", revision=REVISION,
               status_rc=0, revision_rc=0, archive_rc=0, archive_empty=0):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            capture = root / "args"
            docker = root / "docker"
            docker.write_text('#!/bin/sh\nprintf "%s\\n" "$@" > "$CAPTURE"\n')
            docker.chmod(0o755)
            git = root / "git"
            git.write_text(FAKE_GIT)
            git.chmod(0o755)
            env = dict(
                os.environ,
                PATH=directory + os.pathsep + os.environ["PATH"],
                CAPTURE=str(capture),
                FAKE_STATUS=status,
                FAKE_STATUS_RC=str(status_rc),
                FAKE_WORKTREE=worktree,
                FAKE_REVISION=revision,
                FAKE_REVISION_RC=str(revision_rc),
                FAKE_ARCHIVE_RC=str(archive_rc),
                FAKE_ARCHIVE_EMPTY=str(archive_empty),
            )
            result = subprocess.run(
                ["sh", str(SCRIPT), image, "linux/amd64,linux/arm64"],
                env=env, capture_output=True, text=True,
            )
            args = capture.read_text().splitlines() if capture.exists() else None
            return result, args

    def test_release_and_latest_share_one_build_with_provenance_args(self):
        for repository in [
            "muratshagirov/kubeflow-admin",
            "registry.example:5000/team/app",
            "localhost:5000/app",
        ]:
            with self.subTest(repository=repository):
                result, args = self.invoke(repository + ":0.4.2")
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(args, [
                    "buildx", "build",
                    "--platform", "linux/amd64,linux/arm64",
                    "--provenance=true", "--sbom=true",
                    "--build-arg", "VERSION=0.4.2",
                    "--build-arg", "REVISION=" + REVISION,
                    "--push",
                    "-t", repository + ":0.4.2",
                    "-t", repository + ":latest",
                    "-",
                ])

    def test_archive_failure_or_empty_archive_refuses_before_docker(self):
        for kwargs, message in [
            ({"archive_rc": 1}, "source archive"),
            ({"archive_empty": 1}, "archive is empty"),
        ]:
            with self.subTest(**kwargs):
                result, args = self.invoke("team/app:0.4.2", **kwargs)
                self.assertEqual(result.returncode, 2)
                self.assertIsNone(args)
                self.assertIn(message, result.stderr)

    def test_dirty_tree_refuses_publication_before_docker(self):
        result, args = self.invoke(
            "team/app:0.4.2",
            status=" M internal/web/web.go\n?? docs/storage-provisioning-plan.md\n",
        )
        self.assertEqual(result.returncode, 1)
        self.assertIsNone(args)
        self.assertIn("dirty tree", result.stderr)
        self.assertIn("internal/web/web.go", result.stderr)

    def test_git_guard_failures_refuse_before_docker(self):
        cases = [
            ({"worktree": "false"}, "Git worktree"),
            ({"status_rc": 1}, "Could not inspect"),
            ({"revision": "not-a-sha"}, "Git revision"),
            ({"revision_rc": 1}, "Git revision"),
        ]
        for kwargs, message in cases:
            with self.subTest(**kwargs):
                result, args = self.invoke("team/app:0.4.2", **kwargs)
                self.assertEqual(result.returncode, 2)
                self.assertIsNone(args)
                self.assertIn(message, result.stderr)

    def test_invalid_or_non_release_reference_never_calls_docker(self):
        for image in [
            "team/app",
            "registry.example:5000/team/app",
            "team/app:",
            "team/app:latest",
            "team/app:LATEST",
            "team/app:lAtEsT",
            "team/app:dev",
            "team/app:unknown",
            "team/app:local",
            "team/app@sha256:abc",
            "team/app:0.4.2@sha256:abc",
            "team/app:bad tag",
            "team/app:0.4.2/extra",
            "Team/App:0.4.2",
            ":0.4.2",
        ]:
            with self.subTest(image=image):
                result, args = self.invoke(image)
                self.assertEqual(result.returncode, 2)
                self.assertIsNone(args)

    def test_archive_context_is_exact_tracked_head_and_excludes_ignored_files(self):
        with tempfile.TemporaryDirectory() as directory, \
                tempfile.TemporaryDirectory() as tools:
            root = Path(directory)
            scripts = root / "scripts"
            scripts.mkdir()
            shutil.copy(SCRIPT, scripts / "publish-image.sh")
            (root / ".gitignore").write_text(
                "/deploy/site/\n/bin/\n/internal/ignored-*\n"
            )
            (root / "README.md").write_text("tracked\n")
            (root / "Dockerfile").write_text("FROM scratch\n")
            (root / "internal").mkdir()
            (root / "internal/tracked.go").write_text("package internal\n")
            subprocess.run(["git", "init", "-q", str(root)], check=True, capture_output=True)
            subprocess.run(["git", "-C", str(root), "config", "user.email", "release-test@example.invalid"], check=True)
            subprocess.run(["git", "-C", str(root), "config", "user.name", "Release Test"], check=True)
            subprocess.run(
                ["git", "-C", str(root), "add", ".gitignore", "README.md",
                 "Dockerfile", "internal/tracked.go", "scripts/publish-image.sh"],
                check=True,
            )
            subprocess.run(
                ["git", "-C", str(root), "-c", "commit.gpgsign=false",
                 "-c", "core.hooksPath=/dev/null", "commit", "-q", "-m", "fixture"],
                check=True, capture_output=True,
            )
            # These are ignored, including one under internal/. They must never
            # enter the Docker context even though the clean-tree check passes.
            (root / "internal/ignored-secret.go").write_text("package internal\nconst secret = \"must-not-ship\"\n")
            (root / "deploy/site").mkdir(parents=True)
            (root / "deploy/site/config.yaml").write_text("site: local\n")
            (root / "bin").mkdir()
            (root / "bin/kubeflow-admin").write_text("ignored\n")

            capture = Path(tools) / "args"
            context = Path(tools) / "context.tar"
            docker = Path(tools) / "docker"
            docker.write_text(
                '#!/bin/sh\nprintf "%s\\n" "$@" > "$CAPTURE"\ncat > "$CAPTURE_CONTEXT"\n'
            )
            docker.chmod(0o755)
            env = dict(
                os.environ,
                PATH=tools + os.pathsep + os.environ["PATH"],
                CAPTURE=str(capture),
                CAPTURE_CONTEXT=str(context),
            )
            result = subprocess.run(
                ["sh", str(scripts / "publish-image.sh"), "team/app:0.4.2", "linux/amd64,linux/arm64"],
                env=env, capture_output=True, text=True,
            )
            self.assertEqual(result.returncode, 0, result.stderr)
            args = capture.read_text().splitlines()
            revision = subprocess.check_output(
                ["git", "-C", str(root), "rev-parse", "HEAD"], text=True,
            ).strip()
            self.assertIn("VERSION=0.4.2", args)
            self.assertIn("REVISION=" + revision, args)
            self.assertIn("team/app:latest", args)
            self.assertEqual(args[-1], "-")

            with tarfile.open(context, "r:") as archive:
                names = set(archive.getnames())
            self.assertIn("Dockerfile", names)
            self.assertIn("internal/tracked.go", names)
            self.assertNotIn("internal/ignored-secret.go", names)
            self.assertNotIn("deploy/site/config.yaml", names)
            self.assertNotIn("bin/kubeflow-admin", names)


if __name__ == "__main__":
    unittest.main()
