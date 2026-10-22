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

    def test_storage_provisioning_action_contract(self):
        action = self.spec["components"]["schemas"]["Action"]
        kinds = set(action["properties"]["kind"]["enum"])
        self.assertTrue({"pv.create.static", "pvc.create.bound"} <= kinds)
        # Existing workflows must stay documented; legacy templates are not
        # reinterpreted by the new registration-template actions.
        self.assertTrue({"pvc.create.dynamic", "storage.create.static",
                         "pv.reserve-and-claim"} <= kinds)
        properties = action["properties"]
        for field in ("nfsPath", "beeGFSPath", "directoryUID", "directoryGID",
                      "podDefault", "confirmPodDefaultCleanup", "template",
                      "storageClass", "pvName", "targetUID",
                      "targetResourceVersion"):
            self.assertIn(field, properties)
        self.assertFalse(action["additionalProperties"])
        pod_default = self.spec["components"]["schemas"]["PodDefaultRequest"]
        self.assertEqual(set(pod_default["required"]),
                         {"name", "mountPath", "overwrite"})
        self.assertFalse(pod_default["additionalProperties"])
        # The upstream "poddefault-<name>" annotation segment is bounded at 63
        # characters, so the reviewed name is capped independently at 52 bytes.
        self.assertEqual(pod_default["properties"]["name"]["maxLength"], 52)
        # BeeGFS compatibility bound is on the joined final handle.
        self.assertEqual(properties["beeGFSPath"]["maxLength"], 128)

        # The approved registration-template projection is part of the existing
        # inventory response, not a new standalone route.
        inventory = self.spec["components"]["schemas"]["Inventory"]
        self.assertEqual(inventory["properties"]["storageTemplates"]["items"]["$ref"],
                         "#/components/schemas/StorageRegistrationTemplate")
        template = self.spec["components"]["schemas"]["StorageRegistrationTemplate"]
        self.assertFalse(template["additionalProperties"])
        for field in ("name", "kind", "storageClass", "pvCapacity",
                      "pvAccessModes", "pvcRequest", "pvcAccessModes",
                      "reclaimPolicy", "volumeMode", "nfsServer", "nfsRoots",
                      "nfsMountOptions", "requireDirectoryUIDGID",
                      "beegfsDriver", "beegfsPrefix", "podDefaultName",
                      "podDefaultMountPathPrefix"):
            self.assertIn(field, template["properties"])
        self.assertEqual(template["properties"]["podDefaultName"]["maxLength"], 52)

    def test_inline_mapping_descriptions_do_not_create_null_properties(self):
        # YAML inline mappings with an unquoted comma turn the remainder of a
        # description into an extra null-valued key. The OpenAPI validator then
        # rejects the schema even though YAML parsing succeeds.
        def assert_no_nulls(value, path=""):
            if isinstance(value, dict):
                for key, item in value.items():
                    self.assertIsNotNone(item, f"null value at {path}/{key}")
                    assert_no_nulls(item, f"{path}/{key}")
            elif isinstance(value, list):
                for index, item in enumerate(value):
                    assert_no_nulls(item, f"{path}/{index}")

        assert_no_nulls(self.spec)

    def test_no_standalone_poddefault_mutation_route(self):
        # PodDefault changes are only substeps of a reviewed PVC plan; the API
        # deliberately exposes no standalone page or mutation endpoint.
        paths = [path for path in self.spec.get("paths", {})
                 if "poddefault" in path.lower()]
        self.assertEqual(paths, [])

    def test_mutation_receipts_are_bounded_safe_identity_facts(self):
        schemas = self.spec["components"]["schemas"]
        receipts = schemas["Operation"]["properties"]["mutationReceipts"]
        self.assertEqual(receipts["maxItems"], 128)
        self.assertEqual(receipts["items"]["$ref"],
                         "#/components/schemas/MutationReceipt")
        receipt = schemas["MutationReceipt"]
        self.assertFalse(receipt["additionalProperties"])
        self.assertEqual(set(receipt["properties"]),
                         {"kind", "namespace", "name", "uid",
                          "resourceVersion", "outcome"})
        self.assertEqual(set(receipt["required"]),
                         {"kind", "name", "uid", "resourceVersion", "outcome"})
        self.assertEqual(set(receipt["properties"]["outcome"]["enum"]),
                         {"created", "reserved", "overwritten", "deleted",
                          "available", "bound"})


if __name__ == "__main__":
    unittest.main()
