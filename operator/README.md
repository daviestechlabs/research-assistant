# Research Desk Operator

Research Desk Operator reconciles a namespaced `ResearchDesk` resource into the application Helm chart.
It uses the upstream Operator SDK Helm operator.
The API is `research.daviestechlabs.io/v1alpha1`.
The resource name also becomes its Helm release name.
Use different names for the operator release and each desk.

## Prerequisites

- Kubernetes with Helm support.
- An existing writable PVC containing an imported metadata catalog.
- A built application image identified by its registry digest.
- A scoped LiteLLM service key and an allowed research model alias.
- An OIDC provider for private access.
- Envoy Gateway CRDs when enabling the supplied browser authentication route.

Create credential Secrets separately.
The application chart references them and does not include credential values.

## Install

Copy the sample values and set your Kubernetes API Service and endpoint CIDRs.
The example CIDRs represent a sample cluster.
The operator needs DNS and API access while other traffic remains blocked.
For Cilium, set `networkPolicy.provider` to `cilium`.
That mode adds a Cilium API-server entity rule for node endpoint access.
It requires the CiliumNetworkPolicy CRD.

```sh
python3 scripts/package-operator.py --check
helm lint charts/research-desk-operator -f operator/samples/operator-values.yaml
kubectl create namespace research
kubectl apply --dry-run=server -f charts/research-desk-operator/crds/researchdesks.yaml
kubectl apply -f charts/research-desk-operator/crds/researchdesks.yaml
helm upgrade --install research-operator charts/research-desk-operator \
  --namespace research --values operator/samples/operator-values.yaml \
  --wait --timeout 3m
```

The operator watches its own namespace.
Its Role contains no cluster-wide permissions or PVC permissions.
Helm storage requires access to namespace Secrets.
Install it in a namespace dedicated to research desks.

## Create a desk

Edit `operator/samples/researchdesk.yaml` with your actual image, identity settings, model key reference, and PVC name.
The sample remains suspended and uses a placeholder image digest.
Keep it suspended while checking the rendered resources.

```sh
helm template research-example charts/research-desk \
  --namespace research --values operator/samples/desk-values.yaml
kubectl apply --dry-run=server -f operator/samples/researchdesk.yaml
kubectl apply -f operator/samples/researchdesk.yaml
kubectl get researchdesk,pods,services --namespace research
```

Set `spec.suspend` to `false` after configuring the image, catalog, credentials, and access path.
Check the `Deployed` condition and Pod readiness.
The default network rules permit the supplied Envoy Gateway and model service layout.
Adapt its namespaces and labels to your cluster before activation.
Gateway mode validates its HTTPS origin and OIDC endpoints.

## Reconciliation and removal

The operator repairs missing chart resources and applies supported spec changes.
It reconciles dependent resources at a one-minute interval.
Helm creates an uninstall finalizer on each desk.
Deleting a desk removes its chart resources while retaining the external PVC.
Removing the operator before its desks can leave finalizers pending.
Delete the desks first when retiring an installation.

The upstream SDK uses strategic merge for drift repair.
Some changes, such as duplicate named Service ports, can fail that merge.
Inspect the condition and operator logs before attempting a repair.
Use reviewed Helm migration procedures for existing releases.
The `legacyDeployment` setting supports the original `research-assistant` selector during migration.
It is not required for a new installation.

## Upgrade

Helm does not upgrade files in its `crds` directory automatically.
Review and apply the generated CRD before upgrading the operator release.
The generated operator bundle must match `charts/research-desk`.

```sh
python3 scripts/package-operator.py --check
kubectl apply --dry-run=server -f charts/research-desk-operator/crds/researchdesks.yaml
kubectl apply -f charts/research-desk-operator/crds/researchdesks.yaml
helm upgrade research-operator charts/research-desk-operator \
  --namespace research --values operator/samples/operator-values.yaml --wait
```

Set `spec.indexing.imageDigest` to pin a worker during an application update.
The digest must identify an immutable image in the application repository.
An empty value uses the application image.
