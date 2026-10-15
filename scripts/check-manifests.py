#!/usr/bin/env python3
"""Offline rendering + security invariants. Requires PyYAML and kustomize."""
import os
from pathlib import Path
import subprocess
import yaml

ROOT = Path(__file__).resolve().parent.parent
ALLOWED = {
    ("kubeflow.org", "profiles"): {"get", "list", "create", "patch"},
    ("kubeflow.org", "notebooks"): {"get", "list", "patch"},
    ("", "persistentvolumes"): {"get", "list", "create", "patch"},
    ("", "persistentvolumeclaims"): {"get", "list", "create", "patch"},
    **{("", r): {"get", "list"} for r in (
        "namespaces", "pods", "events", "resourcequotas", "limitranges", "nodes")},
    ("apps", "statefulsets"): {"get", "list"},
    ("storage.k8s.io", "storageclasses"): {"get", "list"},
    ("rbac.authorization.k8s.io", "rolebindings"): {"get", "list"},
}
EXPECTED_ROLES = {
    "kubeflow-admin-read": {key: {"get", "list"} for key in ALLOWED},
    "kubeflow-admin-writes": {
        ("kubeflow.org", "profiles"): {"create", "patch"},
        ("kubeflow.org", "notebooks"): {"patch"},
        ("", "persistentvolumeclaims"): {"create"},
    },
    "kubeflow-admin-static-storage": {("", "persistentvolumes"): {"create", "patch"}},
    "kubeflow-admin-pvc-expansion": {("", "persistentvolumeclaims"): {"patch"}},
}

def check(path):
    rendered = subprocess.check_output(
        [os.environ.get("KUSTOMIZE", "kustomize"), "build", str(path)], text=True)
    docs = list(yaml.safe_load_all(rendered))
    objects = {(d["kind"], d["metadata"].get("namespace", ""), d["metadata"]["name"]): d for d in docs}
    assert len(objects) == len(docs), f"duplicate resources: {path}"
    for d in docs:
        if d["kind"] in ("Role", "ClusterRole"):
            actual = {}
            for rule in d.get("rules", []):
                assert not rule.get("nonResourceURLs"), rule
                for group in rule["apiGroups"]:
                    for resource in rule["resources"]:
                        actual.setdefault((group, resource), set()).update(rule["verbs"])
                        assert set(rule["verbs"]) <= ALLOWED.get((group, resource), set()), rule
                        if path.name in ("base", "local-storage"):
                            assert set(rule["verbs"]) <= {"get", "list"}, rule
            assert actual == EXPECTED_ROLES[d["metadata"]["name"]], (path, actual)
        if d["kind"] == "Deployment":
            assert d["spec"]["replicas"] == 1
            assert d["spec"]["strategy"]["type"] == "Recreate"
            pod = d["spec"]["template"]["spec"]
            assert pod["securityContext"]["runAsNonRoot"]
            assert pod["securityContext"]["seccompProfile"]["type"] == "RuntimeDefault"
            for c in pod["containers"]:
                sc = c["securityContext"]
                assert sc["readOnlyRootFilesystem"] and not sc["allowPrivilegeEscalation"]
                assert sc["capabilities"]["drop"] == ["ALL"]
                assert all(p in c for p in ("startupProbe", "readinessProbe", "livenessProbe"))
        if d["kind"] == "ConfigMap":
            config = yaml.safe_load(d["data"]["config.yaml"])
            assert not any(config["features"].values()), "shipped config must be read-only"
            journal = config["database"].get("journalMode", "")
            assert journal in ("", "wal", "delete"), journal
        if d["kind"] == "PersistentVolumeClaim" and path.name == "base":
            class_name = d["spec"].get("storageClassName")
            assert class_name is None or str(class_name).startswith("REPLACE_"), class_name
    print(f"OK {path.relative_to(ROOT)} ({len(docs)} resources)")

if __name__ == "__main__":
    for directory in [ROOT / "deploy/base", *sorted((ROOT / "deploy/overlays").iterdir())]:
        if directory.is_dir():
            check(directory)
