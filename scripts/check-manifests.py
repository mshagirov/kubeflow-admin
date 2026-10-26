#!/usr/bin/env python3
"""Offline rendering + security invariants. Requires PyYAML and kustomize."""
import os
from pathlib import Path
import subprocess
import yaml

ROOT = Path(__file__).resolve().parent.parent
# The read-only base role grants get/list over exactly these resources. Keep it
# separate from ALLOWED: a write overlay may need an extra verb/resource (for
# example RuntimeClass get while planning a GPU edit) without the read base
# silently gaining it.
READ_RESOURCES = {
    ("kubeflow.org", "profiles"),
    ("kubeflow.org", "notebooks"),
    # PVC deletion must detect namespace-local PodDefault references even when
    # optional PodDefault creation is disabled. This read-only grant is in the
    # base role; the mutation verbs stay in the separate poddefaults overlay.
    ("kubeflow.org", "poddefaults"),
    ("", "persistentvolumes"),
    ("", "persistentvolumeclaims"),
    *(("", r) for r in (
        "namespaces", "pods", "events", "resourcequotas", "limitranges", "nodes")),
    ("apps", "statefulsets"),
    ("storage.k8s.io", "storageclasses"),
    ("rbac.authorization.k8s.io", "rolebindings"),
}
# ALLOWED is the union of every verb/resource any shipped role may grant.
ALLOWED = {
    **{key: {"get", "list"} for key in READ_RESOURCES},
    ("kubeflow.org", "profiles"): {"get", "list", "create", "patch", "delete"},
    ("kubeflow.org", "notebooks"): {"get", "list", "patch"},
    # PodDefault mutation is a separate false-by-default capability used only
    # inside reviewed PVC creation/cleanup operations. Never add wildcards,
    # Secret/exec access or unrelated resources here.
    ("kubeflow.org", "poddefaults"): {"get", "list", "create", "patch", "delete"},
    ("", "persistentvolumes"): {"get", "list", "create", "patch", "delete"},
    ("", "persistentvolumeclaims"): {"get", "list", "create", "patch", "delete"},
    # GPU notebook planning must read the referenced RuntimeClass; get only.
    ("node.k8s.io", "runtimeclasses"): {"get"},
    # Explicit optional pod-restart workflow uses PDB-aware eviction only.
    ("", "pods/eviction"): {"create"},
    # Explicit optional node-cordon workflow sets spec.unschedulable via JSON patch.
    ("", "nodes"): {"get", "list", "patch"},
}
EXPECTED_ROLES = {
    "kubeflow-admin-read": {key: {"get", "list"} for key in READ_RESOURCES},
    "kubeflow-admin-writes": {
        ("kubeflow.org", "profiles"): {"create", "patch"},
        ("kubeflow.org", "notebooks"): {"patch"},
        ("", "persistentvolumeclaims"): {"create"},
        ("node.k8s.io", "runtimeclasses"): {"get"},
    },
    "kubeflow-admin-static-storage": {("", "persistentvolumes"): {"create", "patch"}},
    "kubeflow-admin-pvc-expansion": {("", "persistentvolumeclaims"): {"patch"}},
    "kubeflow-admin-pod-restarts": {("", "pods/eviction"): {"create"}},
    "kubeflow-admin-node-cordon": {("", "nodes"): {"patch"}},
    # Explicit optional Profile deletion workflow: the single delete verb on
    # the cluster-scoped Profile resource, and nothing else.
    "kubeflow-admin-profile-deletion": {("kubeflow.org", "profiles"): {"delete"}},
    "kubeflow-admin-storage-editing": {
        ("", "persistentvolumes"): {"patch"},
        ("", "persistentvolumeclaims"): {"patch"},
    },
    "kubeflow-admin-storage-deletion": {
        ("", "persistentvolumes"): {"delete"},
        ("", "persistentvolumeclaims"): {"delete"},
    },
    "kubeflow-admin-pv-reclaim": {("", "persistentvolumes"): {"patch"}},
    # Optional, separately reviewed PodDefault create/overwrite/cleanup used
    # only as a substep of reviewed PVC operations. Base read stays get/list.
    "kubeflow-admin-poddefaults": {
        ("kubeflow.org", "poddefaults"): {"get", "list", "create", "patch", "delete"},
    },
}
# Destructive grants remain separately opt-in and resource-specific; they
# must not enter base, writes or the all-features overlay implicitly.
DELETE_PATHS = {
    ("kubeflow.org", "profiles"): "profile-deletion",
    ("kubeflow.org", "poddefaults"): "poddefaults",
    ("", "persistentvolumes"): "storage-deletion",
    ("", "persistentvolumeclaims"): "storage-deletion",
}
DELETE_OPT_IN_PATH = "profile-deletion"
# Every overlay variant that must render, exactly once, for the release.
EXPECTED_VARIANTS = {
    "base",
    "all-features",
    "local-storage",
    "node-cordon",
    "pod-restarts",
    "poddefaults",
    "pvc-expansion",
    "pv-reclaim",
    "profile-deletion",
    "static-storage",
    "storage-editing",
    "storage-deletion",
    "writes",
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
                        if resource == "pods/eviction":
                            assert path.name == "pod-restarts", "eviction grant must stay separately opt-in"
                        if resource == "nodes" and "patch" in rule["verbs"]:
                            assert path.name == "node-cordon", "node cordon grant must stay separately opt-in"
                        if (group, resource) == ("kubeflow.org", "poddefaults"):
                            assert path.name == "poddefaults" or set(rule["verbs"]) <= {"get", "list"}, \
                                "PodDefault mutation must stay in the separate poddefaults overlay"
                        if resource.endswith("/finalizers"):
                            assert not ({"patch", "update", "delete"} & set(rule["verbs"])), rule
                        for verb in rule["verbs"]:
                            if verb == "delete":
                                assert (group, resource) in DELETE_PATHS, rule
                                assert path.name == DELETE_PATHS[(group, resource)], "deletion grants must stay separately opt-in"
                        actual.setdefault((group, resource), set()).update(rule["verbs"])
                        assert set(rule["verbs"]) <= ALLOWED.get((group, resource), set()), rule
                        if path.name in ("base", "local-storage"):
                            assert set(rule["verbs"]) <= {"get", "list"}, rule
            assert actual == EXPECTED_ROLES[d["metadata"]["name"]], (path, actual)
            if d["metadata"]["name"] == "kubeflow-admin-profile-deletion":
                assert path.name == DELETE_OPT_IN_PATH, "profile deletion grant must stay separately opt-in"
            for role, overlay in (("kubeflow-admin-storage-editing", "storage-editing"),
                                  ("kubeflow-admin-pv-reclaim", "pv-reclaim"),
                                  ("kubeflow-admin-storage-deletion", "storage-deletion")):
                if d["metadata"]["name"] == role:
                    assert path.name == overlay, "storage management grants must stay separately opt-in"
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
    directories = [ROOT / "deploy/base", *sorted((ROOT / "deploy/overlays").iterdir())]
    directories = [directory for directory in directories if directory.is_dir()]
    variants = {directory.name for directory in directories}
    assert variants == EXPECTED_VARIANTS, f"overlay variants changed: {sorted(variants)}"
    for directory in directories:
        check(directory)
    with (ROOT / "deploy/examples/db-ownership-check.yaml").open() as source:
        checks = [doc for doc in yaml.safe_load_all(source) if doc["kind"] == "Pod"]
    assert len(checks) == 2, "both disposable database ownership checks must be present"
    for pod in checks:
        assert pod["spec"].get("automountServiceAccountToken") is False, "offline database checks must not mount API credentials"
        for container in pod["spec"]["containers"]:
            for group in ("requests", "limits"):
                assert all(container.get("resources", {}).get(group, {}).get(key) for key in ("cpu", "memory")), "database checks must bound CPU and memory"
    print(f"OK {len(variants)} rendered variants ({', '.join(sorted(variants))})")
