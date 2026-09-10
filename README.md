# Label Policy Supervisor Service

A vSphere Supervisor Service, that lets a namespace user apply labels to existing objects they don't otherwise have RBAC to `patch` directly. A privileged controller does the patching on their behalf, scoped to an explicit allowlist of target resource types.

This project also contains some basic concepts around, structuring, packaging and deploying vSphere Supervisor services with the [carvel.dev](https://carvel.dev) toolset.

## Credit

Project is based on [William Arroyo's](https://github.com/warroyo) [Argocd attach service](https://github.com/warroyo/argocd-attach-service)

## Prerequisites

- VCF supervisor cluster
- Container registry
- Docker/Podman
- Carvel.dev tooling installed
- govc (optional)

## Development

### Carvel.dev


- Website - https://carvel.dev
- Github - https://github.com/carvel-dev/carvel

Tools:

- [kctrl (kapp-controller)](https://carvel.dev/kapp-controller/docs/v0.57.x/)
- [ytt (yaml templating)](https://carvel.dev/ytt/)
- [imgpkg (packaging)](https://carvel.dev/imgpkg/)
- [kbld (image resolution)](https://carvel.dev/kbld/)

### Project structure

- `controller/` — the Go controller backing custom [Kubernetes Operator](https://kubernetes.io/docs/concepts/extend-kubernetes/operator/)  
  - `main.go` is the reconciler entrypoint 
  - `manifests/crd.yml` is the authored CRD for our application
  - `Dockerfile` builds the controller image, and `go.mod`/`go.sum` pin its dependencies.

- `config/` — the ytt-templated package that gets installed on the supervisor cluster: 
  - `config-release.yml` wires the package metadata for `make release` and crucially contains the target container image of the controller
  - `schema.yml` defines the data values (including `allowed_resources`, the RBAC/allowlist source of truth) and their defaults
  - `deploy.yml` renders the `Deployment`/`ServiceAccount`/RBAC from those values

- `package-build.yml` and `package-resources.yml` — the `kctrl` (Carvel) package definitions that turn `config/` into an installable Supervisor Service package: 
  - `package-build.yml` drives `make release` (ytt + kbld + imgpkg bundle build), 
  - `package-resources.yml` is the generated `Package`/`PackageMetadata`/`PackageInstall` resources used to install it on a Supervisor cluster

- `Makefile` - basic makefile with 3 stages - build-controller, release-controller & release

### Local environment setup

Building and releasing requires having Docker/Podman on your system. You'll also need the [Carvel tooling](#carveldev) (`kctrl`, `ytt`, `imgpkg`, `kbld`) on your `PATH`. When working with a registry with a self-signed or internally-issued certificate, import that CA into your system's trust store first — otherwise Docker/Podman and `imgpkg` will fail to push/pull with a TLS verification error.

### Building and packaging

Steps:

  - `export VERSION=1.0.0` - tag your build with a version
  - `make build-controller` - building the controller container image
  - `make release-controller` - pushing controller image to registry
  - `make release` - create supervisor service package referencing image from above steps, push it to repo and create a manifest for it

Last step generates a `label-policy.yml` Carvel Package manifest. `kctrl package release` bundles `config/` and writes the resulting `PackageMetadata` (`metadata.yml`) and versioned `Package` (`package.yml`) under `carvel-artifacts/packages/label-policy.field.vmware.com/`; the `make release` target concatenates those two documents into a single `label-policy.yml`.

### Installing Supervisor Service in vSphere

There are two distinct approaches for deploying this package to Supervisor:

### vSphere UI

Through vSphere UI under Supervisor Management you can upload your package.

### govc

With [govc](https://github.com/vmware/govmomi/tree/main/govc) you can automate deployment of you Supervisor Service.

Installing a Supervisor Service

```bash
export GOVC_URL="https://vce-01.corp.local"
export GOVC_USERNAME="administrator@vsphere.local"
export GOVC_PASSWORD=
export GOVC_TLS_CA_CERTS=vc-root-ca.pem
govc namespace.service.create -accept-eula=false -spec-type=carvel -trusted=false  label-policy.yml
```

Cleaning up with govc

```bash
govc namespace.service.version.deactivate label-policy.field.vmware.com 1.0.0
govc namespace.service.deactivate label-policy.field.vmware.com
govc namespace.service.version.rm label-policy.field.vmware.com 1.0.0
govc namespace.service.rm label-policy.field.vmware.com
```

### Activating Supervisor Service

Once Supervisor service is installed you can activate it on selected Supervisor using vSphere UI.


## Working with Supervisor Services

A few general guidelines that apply beyond this specific service, and things to watch out for:

- **Least-privilege RBAC.** The `ServiceAccount`/`ClusterRole` your package installs runs with real cluster-admin-adjacent power inside every namespace where it's activated — scope verbs and resources to exactly what the controller needs (see `allowed_resources` in [`config/schema.yml`](config/schema.yml)), and prefer generating RBAC from a single source of truth rather than hand-maintaining it alongside the controller flags.
- **Namespace scoping.** Supervisor Services are activated per-namespace; don't assume cluster-scoped access or the ability to reach across namespaces unless you've explicitly requested and been granted that in the package's RBAC.
- **Pin images by digest.** Use `kbld` (or equivalent) to resolve the controller image to a digest at package-build time, not a mutable tag — otherwise `kapp-controller` can silently redeploy a different image than what was tested.
- **Package versions are immutable.** Once a `Package` version is released/activated, treat it as immutable — ship fixes as a new version rather than mutating an existing one, since consumers may already be pinned to it via `versionSelection`.
- **CRD changes need a migration story.** Adding a new field is safe; renaming/removing one or tightening `required` on an existing version can break existing custom resources on upgrade — bump the CRD version and support both, or provide a conversion webhook, rather than breaking existing objects in place.
- **Idempotent, resync-tolerant reconciliation.** kapp-controller/your controller may reconcile on a timer as well as on watch events — reconcile logic must be safe to run repeatedly against the same state, not just on the first apply.
- **Test against a real Supervisor cluster early.** Admission/validation behavior (namespace webhooks, PSA, the VI admin's allowlist) differs from a vanilla kind/minikube cluster — a package that installs cleanly there can still fail activation or RBAC checks on VCF.
- **Don't put secrets in data values.** Package data values (`config/schema.yml` defaults, `PackageInstall` values) usually end up visible to whoever can read the `Package`/`PackageInstall` resources — pull secrets from a `Secret`/external source instead of baking them into schema defaults.



## How it works

The service watches a `LabelPolicy` custom resource. Each `LabelPolicy` names a target (by GVR + either a `name` or a `selector.matchLabels`) and a set of labels. The controller resolves the matching object(s) in the same namespace and applies the labels via Kubernetes server-side apply, owned under the field manager `labelpolicy-controller`.

```yaml
apiVersion: field.vmware.com/v1
kind: LabelPolicy
metadata:
  name: cost-center-tag
spec:
  target:
    group: vmoperator.vmware.com
    version: v1alpha5
    resource: virtualmachines
    kind: VirtualMachine
    selector:
      matchLabels:
        app: frontend
    # OR: name: my-single-vm
  labels:
    cost-center: "1234"
    backup-tier: gold
```

Deleting the `LabelPolicy` re-applies with an empty label set under the same field manager, which retracts exactly the keys this controller owns — it does not delete the target object.

## Allowed targets

Because `spec.target` can name any GVR, the controller only acts on types explicitly allowed by the package operator. `config/schema.yml`'s `allowed_resources` list is the single source of truth for this — it's used to generate both:

- the `ClusterRole` the package installs (only `get`/`list`/`watch`/`patch` on exactly those resources), and
- the `--allowed-target=group/version/resource/kind` flags passed to the controller, which it checks independently of RBAC before touching anything.

Default allowlist: `vmoperator.vmware.com/v1alpha3/virtualmachines` (VirtualMachine) and `cluster.x-k8s.io/v1beta1/clusters` (Cluster API `Cluster`). A VI admin extends this at install time via the package's data values, the same way `label-policy`'s `blocked_namespaces` is set.

## Known v1 limitations

- Targets are resolved once per `LabelPolicy` add/update/resync tick (`--resync-period`, default 60s) — it does not watch the target objects themselves, so a new object created after the `LabelPolicy` won't get labeled until the next resync.
- Apply uses `Force: true`, so a conflicting label key already set by another field manager (e.g. plain `kubectl label`, on an object that predates any SSA use) will be overwritten rather than rejected.
- No status subresource / conditions yet — check controller logs for apply failures (e.g. a target outside the allowlist).

## CRD source of truth

`controller/manifests/crd.yml` is the authored file (kept next to the Go types it mirrors); `config/crd.yml` is generated from it by `make config/crd.yml` (a prerequisite of `make release`). ytt refuses to follow a symlink there, so this is a real copy, not a symlink — don't hand-edit `config/crd.yml` directly, it'll be overwritten.
