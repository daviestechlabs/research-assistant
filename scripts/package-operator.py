#!/usr/bin/env python3
"""Generate the operator's mounted chart and structural CRD from the application chart."""

import argparse
import json
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
APP = ROOT / "charts/research-desk"
OPERATOR = ROOT / "charts/research-desk-operator"


def structural(node):
    if isinstance(node, list):
        return [structural(item) for item in node]
    if not isinstance(node, dict):
        return node
    if node.get("uniqueItems") is True:
        node = {key: value for key, value in node.items() if key != "uniqueItems"}
        node["x-kubernetes-list-type"] = "set"
    return {
        key: structural(value)
        for key, value in node.items()
        if key not in ("$schema", "if", "then")
        and not (key == "allOf" and any("if" in branch for branch in value))
        and not (key == "additionalProperties" and value is False)
    }


def generated_crd():
    schema = structural(json.loads((APP / "values.schema.json").read_text()))
    values = json.loads((APP / "values.yaml").read_text())
    # CR fields receive the chart's safe defaults. Authentication and claim ownership are explicit.
    for key, value in values.items():
        if key not in ("auth", "storage"):
            schema["properties"][key]["default"] = value
    storage = schema["properties"]["storage"]
    storage["required"] = ["existingClaim"]
    for key in ("catalogFile", "memoryMiB"):
        storage["properties"][key]["default"] = values["storage"][key]
    schema["required"] = ["auth", "storage"]
    schema["x-kubernetes-validations"] = [
        {"rule": '!has(self.indexing) || !self.indexing.enabled || size(self.indexing.configMapName) > 0', "message": "Enabled indexing requires an external collection configuration."},
        {
            "rule": '!self.gateway.enabled || (self.auth.publicOrigin == "https://" + self.gateway.host && size(self.gateway.parents) > 0)',
            "message": "Enabled Gateway host must match the authenticated public origin and have an HTTPS parent.",
        },
        {
            "rule": '!self.gateway.enabled || (self.gateway.authorizationEndpoint.startsWith("https://") && self.gateway.tokenEndpoint.startsWith("https://") && self.gateway.endSessionEndpoint.startsWith("https://"))',
            "message": "Enabled Gateway requires HTTPS OIDC endpoints.",
        },
    ]
    return {
        "apiVersion": "apiextensions.k8s.io/v1",
        "kind": "CustomResourceDefinition",
        "metadata": {"name": "researchdesks.research.daviestechlabs.io"},
        "spec": {
            "group": "research.daviestechlabs.io",
            "scope": "Namespaced",
            "names": {
                "kind": "ResearchDesk",
                "listKind": "ResearchDeskList",
                "plural": "researchdesks",
                "singular": "researchdesk",
                "shortNames": ["rdesk"],
            },
            "versions": [
                {
                    "name": "v1alpha1",
                    "served": True,
                    "storage": True,
                    "subresources": {"status": {}},
                    "additionalPrinterColumns": [
                        {
                            "name": "Deployed",
                            "type": "string",
                            "jsonPath": '.status.conditions[?(@.type=="Deployed")].status',
                        },
                        {
                            "name": "Model",
                            "type": "string",
                            "jsonPath": ".spec.inference.model",
                        },
                        {
                            "name": "Age",
                            "type": "date",
                            "jsonPath": ".metadata.creationTimestamp",
                        },
                    ],
                    "schema": {
                        "openAPIV3Schema": {
                            "type": "object",
                            "required": ["spec"],
                            "properties": {
                                "apiVersion": {"type": "string"},
                                "kind": {"type": "string"},
                                "metadata": {"type": "object"},
                                "spec": schema,
                                "status": {
                                    "type": "object",
                                    "x-kubernetes-preserve-unknown-fields": True,
                                },
                            },
                        }
                    },
                }
            ],
        },
    }


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--check",
        action="store_true",
        help="Reject stale generated files without writing",
    )
    args = parser.parse_args()
    expected = {
        OPERATOR / "files/research-desk" / path.relative_to(APP): path.read_bytes()
        for path in APP.rglob("*")
        if path.is_file()
    }
    expected[OPERATOR / "crds/researchdesks.yaml"] = (
        json.dumps(generated_crd(), indent=2) + "\n"
    ).encode()
    actual = set(
        path for path in (OPERATOR / "files/research-desk").rglob("*") if path.is_file()
    )
    if args.check:
        stale = [
            str(path.relative_to(ROOT))
            for path, data in expected.items()
            if not path.exists() or path.read_bytes() != data
        ]
        stale += [str(path.relative_to(ROOT)) for path in actual - set(expected)]
        if stale:
            raise SystemExit("Stale operator package: " + ", ".join(stale))
        print("Operator chart bundle and CRD match the source chart.")
        return
    for path in actual - set(expected):
        path.unlink()
    for path, data in expected.items():
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(data)
    print("Generated the operator chart bundle and ResearchDesk CRD.")


if __name__ == "__main__":
    main()
