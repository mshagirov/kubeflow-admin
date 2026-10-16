"""Regression tests for the offline API contract checker itself."""
import copy
import importlib.util
from pathlib import Path
import unittest

import yaml

SPEC = importlib.util.spec_from_file_location(
    "api_parity", Path(__file__).with_name("check-api-parity.py"))
parity = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(parity)


class APIParityTests(unittest.TestCase):
    def setUp(self):
        self.spec = yaml.safe_load(parity.SPEC.read_text())
        self.routes = parity.implemented_routes(parity.WEB.read_text())

    def test_current_routes_and_error_shape(self):
        parity.check_routes(self.routes, self.spec)
        parity.check_error_shape(parity.ERRORS.read_text(), self.spec)

    def test_undocumented_api_route_is_rejected(self):
        with self.assertRaises(AssertionError):
            parity.check_routes(self.routes | {("get", "/api/v1/undocumented")}, self.spec)

    def test_missing_control_route_documentation_is_rejected(self):
        del self.spec["paths"]["/auth/signed-out"]
        with self.assertRaises(AssertionError):
            parity.check_routes(self.routes, self.spec)

    def test_aliases_use_json_pointer_escaping(self):
        self.assertEqual(parity.resolve(self.spec, "#/paths/~1auth~1login"),
                         self.spec["paths"]["/auth/login"])

    def test_wrong_error_field_types_are_rejected(self):
        for field in ("code", "message", "retryable"):
            with self.subTest(field=field):
                spec = copy.deepcopy(self.spec)
                spec["components"]["schemas"]["APIError"]["properties"][field]["type"] = "integer"
                with self.assertRaises(AssertionError):
                    parity.check_error_shape(parity.ERRORS.read_text(), spec)


if __name__ == "__main__":
    unittest.main()
