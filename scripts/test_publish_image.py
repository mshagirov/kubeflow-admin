import os
from pathlib import Path
import subprocess
import tempfile
import unittest


SCRIPT = Path(__file__).with_name("publish-image.sh")


class PublishImageTests(unittest.TestCase):
    def invoke(self, image):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            capture = root / "args"
            docker = root / "docker"
            docker.write_text('#!/bin/sh\nprintf "%s\\n" "$@" > "$CAPTURE"\n')
            docker.chmod(0o755)
            env = dict(os.environ, PATH=directory + os.pathsep + os.environ["PATH"], CAPTURE=str(capture))
            result = subprocess.run(["sh", str(SCRIPT), image, "linux/amd64,linux/arm64"], env=env, capture_output=True, text=True)
            return result, capture.read_text().splitlines() if capture.exists() else None

    def test_release_and_latest_share_one_build(self):
        for repository in ["muratshagirov/kubeflow-admin", "registry.example:5000/team/app"]:
            with self.subTest(repository=repository):
                result, args = self.invoke(repository + ":0.4.2")
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(args, ["buildx", "build", "--platform", "linux/amd64,linux/arm64", "--provenance=true", "--sbom=true", "--push", "-t", repository + ":0.4.2", "-t", repository + ":latest", "."])

    def test_invalid_or_latest_only_reference_never_calls_docker(self):
        for image in ["team/app", "registry.example:5000/team/app", "team/app:", "team/app:latest", "team/app@sha256:abc", "team/app:bad tag", ":0.4.2"]:
            with self.subTest(image=image):
                result, args = self.invoke(image)
                self.assertEqual(result.returncode, 2)
                self.assertIsNone(args)


if __name__ == "__main__":
    unittest.main()
