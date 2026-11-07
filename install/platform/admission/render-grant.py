#!/usr/bin/env python3
"""Render one user's admission params and bindings from their grant.

    render-grant.py <chart-dir> <namespace> <torrent-node-port> <service-type>

Prints the root-owned ConfigMap that holds the user's grant as policy params,
and the ValidatingAdmissionPolicyBindings that attach both platform policies to
exactly that namespace. It goes through `kubectl apply`, run as root at
`box adduser` -- and again at `box install`, `box update` and `box bootstrap`,
so a chart release that blesses a new repository reaches existing seedboxes.

The blessed repository list is read from the chart's variants.yaml -- the same
file the templates resolve images from -- so a repository is blessed in one
place and nowhere else; there is no second list to edit when an app is added.
This script is the seam `box` replaces with the same logic in Go when it reads
a published chart; until then it is how the platform layer and the e2e build
the params.
"""

import os
import sys

import yaml

USAGE = "usage: render-grant.py <chart-dir> <namespace> <torrent-node-port> <service-type>"

BINDING = """---
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicyBinding
metadata:
  name: {name}
  labels:
    app.kubernetes.io/part-of: s5n
spec:
  policyName: {policy}
  validationActions: ["Deny"]
  paramRef:
    name: {params}
    namespace: s5n-system
    parameterNotFoundAction: Deny
  matchResources:
    # The binding is cluster-scoped, so the user cannot edit it; it selects
    # their namespace by the name label the API server puts on every namespace.
    namespaceSelector:
      matchExpressions:
        - key: kubernetes.io/metadata.name
          operator: In
          values: ["{namespace}"]
"""


def main():
    if len(sys.argv) != 5:
        print(USAGE, file=sys.stderr)
        sys.exit(2)
    chart_dir, namespace, node_port, service_type = sys.argv[1:]

    with open(os.path.join(chart_dir, "variants.yaml")) as f:
        variants = yaml.safe_load(f)

    repositories = sorted(
        {
            variant["repository"]
            for app in variants.values()
            for variant in app.values()
            if isinstance(variant, dict) and variant.get("repository")
        }
    )
    if not repositories:
        sys.exit("variants.yaml names no repositories")
    if service_type not in ("NodePort", "LoadBalancer", "ClusterIP"):
        sys.exit("unsupported service type %r" % service_type)
    port = int(node_port)

    params = "s5n-grant-%s" % namespace
    print(
        f"""apiVersion: v1
kind: ConfigMap
metadata:
  name: {params}
  namespace: s5n-system
  labels:
    app.kubernetes.io/part-of: s5n
    s5n.io/grant: {namespace}
data:
  # Generated from {os.path.join(chart_dir, "variants.yaml")}; edit the chart, not this.
  blessedRepositories: "{",".join(repositories)}"
  serviceType: {service_type}
  torrentNodePort: "{port}"
"""
    )
    for policy, suffix in (("s5n-blessed-images", ""), ("s5n-granted-service", "-service")):
        print(BINDING.format(name=params + suffix, policy=policy, params=params, namespace=namespace), end="")


if __name__ == "__main__":
    main()
