#!/usr/bin/env python3
"""Offline parity between the implemented HTTP API and api/openapi.yaml.

Requires PyYAML only; no cluster or network access. It fails when a registered
/api/ route is undocumented, a documented /api/ route is not registered, a
documented control route is missing, or the documented Error envelope stops
matching the Go apiError struct.
"""
import re
from pathlib import Path
import yaml

ROOT = Path(__file__).resolve().parent.parent
WEB = ROOT / "internal/web/web.go"
ERRORS = ROOT / "internal/web/errors.go"
SPEC = ROOT / "api/openapi.yaml"
HTTP_METHODS = {"get", "post", "put", "patch", "delete", "options", "head", "trace"}
# Non-API control routes, including protected logout routes. Aliases must be
# documented too; this set says nothing about a route's authentication policy.
CONTROL_ROUTES = {
    ("get", "/login"),
    ("get", "/callback"),
    ("get", "/auth/login"),
    ("get", "/auth/callback"),
    ("get", "/auth/signed-out"),
    ("get", "/signed-out"),
    ("get", "/healthz"),
    ("get", "/readyz"),
    ("post", "/logout"),
    ("post", "/auth/logout"),
}


def implemented_routes(text):
    routes = set()
    for method, path in re.findall(r'HandleFunc\("([A-Z]+)\s+(/[^"]*)"', text):
        # Skip concatenated loop prefixes (".../"+name), handled below.
        if not path.endswith("/"):
            routes.add((method.lower(), path))
    # Loop with a literal prefix: private.HandleFunc("GET /api/v1/"+name).
    prefixed = re.search(
        r'for _, name := range \[\]string\{([^}]*)\}\s*\{\s*'
        r'private\.HandleFunc\("([A-Z]+)\s+(/[^"]*)"\+name',
        text,
        re.S,
    )
    if prefixed:
        method, prefix = prefixed.group(2).lower(), prefixed.group(3)
        for name in re.findall(r'"([^"]+)"', prefixed.group(1)):
            routes.add((method, prefix + name))
    # Loop of full paths: root.HandleFunc("GET "+path).
    full = re.search(
        r'for _, path := range \[\]string\{([^}]*)\}\s*\{\s*'
        r'root\.HandleFunc\("([A-Z]+) "\+path',
        text,
        re.S,
    )
    if full:
        method = full.group(2).lower()
        for name in re.findall(r'"([^"]+)"', full.group(1)):
            routes.add((method, name))
    return routes


def documented_routes(spec):
    routes = set()
    for path, item in spec.get("paths", {}).items():
        if not path.startswith("/api/"):
            continue
        for method in item:
            if method in HTTP_METHODS:
                routes.add((method, path))
    return routes


def resolve(spec, ref):
    node = spec
    for part in ref.lstrip("#/").split("/"):
        node = node[part.replace("~1", "/").replace("~0", "~")]
    return node


def check_routes(routes, spec):
    implemented = {r for r in routes if r[1].startswith("/api/")}
    documented = documented_routes(spec)
    missing = sorted(implemented - documented)
    extra = sorted(documented - implemented)
    assert not missing, f"implemented but undocumented API routes: {missing}"
    assert not extra, f"documented but unregistered API routes: {extra}"
    absent = sorted(CONTROL_ROUTES - routes)
    assert not absent, f"control routes not registered: {absent}"
    for method, path in sorted(CONTROL_ROUTES):
        item = spec.get("paths", {}).get(path)
        assert item is not None, f"undocumented control route: {method} {path}"
        seen = set()
        while "$ref" in item:
            ref = item["$ref"]
            assert ref not in seen, f"cyclic route reference: {path}"
            seen.add(ref)
            item = resolve(spec, ref)
        assert method in item, f"undocumented control method: {method} {path}"
    return len(implemented)


def check_error_shape(errs, spec):
    block = re.search(r"type apiError struct \{(.*?)\}", errs, re.S)
    assert block, "apiError struct not found in internal/web/errors.go"
    fields = set(re.findall(r'json:"([^"]+)"', block.group(1)))
    assert fields == {"code", "message", "retryable"}, fields
    assert re.search(r'Error\s+apiError\s+`json:"error"`', errs), \
        "failure() must wrap apiError as the `error` field"
    schema = spec["components"]["responses"]["Error"]["content"]["application/json"]["schema"]
    assert schema.get("type") == "object", schema
    assert set(schema.get("required", [])) == {"error"}, schema
    assert set(schema.get("properties", {})) == {"error"}, schema
    target = resolve(spec, schema["properties"]["error"]["$ref"])
    assert target.get("type") == "object", target
    assert set(target.get("required", [])) == fields, target
    assert set(target.get("properties", {})) == fields, target
    assert target["properties"]["retryable"].get("type") == "boolean", target
    for field in ("code", "message"):
        assert target["properties"][field].get("type") == "string", target


if __name__ == "__main__":
    routes = implemented_routes(WEB.read_text())
    with SPEC.open() as f:
        spec = yaml.safe_load(f)
    count = check_routes(routes, spec)
    check_error_shape(ERRORS.read_text(), spec)
    print(f"OK api/openapi.yaml ({count} API routes, control routes, structured error shape)")
