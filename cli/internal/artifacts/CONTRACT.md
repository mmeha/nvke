# Artifact file contract

The artifact file is an NVKR release description: the output of
`nvkr release export`, unmodified. `nvkectl` reads it as is. No converter
exists and no NVKE-specific schema exists.

The file lists every image and chart that `nvkectl` installs on the seed and
on the site. It is the only source of images and charts. The stack config
names no images. `nvkectl` knows which outputs it expects, by name, and
refuses a file that lacks one.

NVKR owns the schema. `nvkectl` reads a subset of the document and ignores
the rest. NVKR adds fields and outputs that NVKE does not use, such as the
`nvkectl` binaries, apt repositories and disk images. An unknown key or an
unknown output name is not an error. An unknown `schemaVersion` is.

## Encoding

JSON. The exporter has one format.

```sh
nvkr release export --layout <dir> --reference <registry>/<repo>@sha256:<digest> > artifacts.json
```

## What nvkectl reads

| Field | Meaning |
| --- | --- |
| `schemaVersion` | Must be `nvkr.dev/release-description/v1`. |
| `rootDigest` | The release's identity. Recorded in the install record. |
| `outputs[]` | A list sorted by `name`, one entry per output. `nvkectl` looks an output up by `name`. |
| `evidence[]` | One SBOM entry per output. The source of an output lives here. |

Per output:

| Key | Meaning |
| --- | --- |
| `name` | The lookup key. See the table of outputs. |
| `kind` | `oci` for an image, `helm-oci` for a chart. Any other kind on an NVKE output is an error. |
| `reference` | Where to pull from. NVKR always pins it with `@sha256:<64 hex>`. The install uses nothing else. |
| `os`, `arch` | Present on a single-platform image. |
| `imageIndex` | Present on a multi-platform image. `requiredPlatforms` and `children[].platform` say which platforms it serves. |
| `chartVersion` | Present on a chart. |

Per evidence entry:

| Key | Meaning |
| --- | --- |
| `binding.outputs[]` | The outputs this entry describes. |
| `source.repo`, `source.commit` | The checkout the subject was built from. Present on NVKE-built subjects. |

There is no release-wide source. Each source describes its own subject, and
`nvkectl` reads it per output. The install record carries the source of every
output that has one. The `cwe` output must have one. Third-party charts have
none.

The Cluster API version is not in the file. The `nvkectl` build pins it, one
number for Cluster API core, the kubeadm providers and `clusterctl`.

## Outputs

| Name | Kind | Required | Role |
| --- | --- | --- | --- |
| `cwe` | `oci` | yes | Controller image. |
| `controller-profiles` | `oci` | yes | Profiles image. |
| `capnico-controller` | `oci` | yes | CAPNICo controller image. |
| `controller-chart` | `helm-oci` | yes | Controller chart. |
| `nktp` | `helm-oci` | yes | Telemetry pipeline chart. |
| `postgresql` | `helm-oci` | yes | Database chart. |
| `spire-crds` | `helm-oci` | yes | SPIRE CRDs chart. |
| `spire` | `helm-oci` | yes | SPIRE chart. |
| `temporal` | `helm-oci` | yes | Temporal chart. |
| `nico` | `helm-oci` | no | NICo chart. Its presence selects NICo on the seed. |
| `nico-prereqs` | `helm-oci` | no | NICo prerequisites chart. Required when `nico` is present. |

## Platforms

The seed and the site may run on different architectures. One image name is
one output. A multi-platform image carries an `imageIndex` with one child per
platform. An image output serves a platform when `os/arch` names it or when
`imageIndex.children[]` has a child for it. `nvkectl` checks every image
output against the platform of the cluster it is about to install on.

## Offline installs

The file carries registry references only. An install without registry
access stages the referenced manifests into a local store first, digests
unchanged. That is an `nvkectl` step, separate from this file, and it is not
part of this contract.

## Supported chart versions

Each chart version must fall inside the range the `nvkectl` build supports.
The ranges below are placeholders. The build sets the real ranges.

| Chart | Range |
| --- | --- |
| `controller-chart` | `>=0.1.0 <1.0.0` |
| `nktp` | `>=0.1.0 <1.0.0` |
| `postgresql` | `>=18.5.0 <19.0.0` |
| `spire-crds` | `>=0.6.0 <0.7.0` |
| `spire` | `>=0.30.0 <0.31.0` |
| `temporal` | `>=0.74.0 <0.75.0` |
| `nico` | `>=0.1.0 <1.0.0` |
| `nico-prereqs` | `>=0.1.0 <1.0.0` |

## Install record

`nvkectl` writes the sha256 digest of the file and the file's `rootDigest`
into the install record. The seed and the site then provably used the same
release.

## Validation

`nvkectl` validates the file before it installs anything. The checks run in
this order and stop at the first failure:

1. The file is JSON and `schemaVersion` is the one above.
2. Every output name appears once. Every required output is present.
   `nico-prereqs` is present when `nico` is.
3. Every NVKE output has the kind its role needs. Its `reference` is present
   and carries a digest.
4. Every image output serves the target platform.
5. Every chart version is in the supported range.
6. `cwe` has a source in `evidence[]`.
7. Every reference resolves. The registry is reachable.
8. `helm template` of each chart with the embedded values, then a server-side
   dry run against the target cluster.
9. Cluster preconditions: a default StorageClass, cert-manager and Cluster API
   CRDs, image access, nodes able to attest, a VIP that resolves.

A failure on one output names it as `outputs[name=<name>]`, for example
`outputs[name=cwe]: reference has no digest`. A failure on the document as a
whole names the key. Every failure points the user to `nvkr` or to this
document.

Steps 1 to 6 need no network and no cluster. The test cases under `testdata/`
cover them, including two unmodified exports from NVKR's own test fixtures.
Those two carry no NVKE output, so the only error they may produce is the
missing required output. Any other error means this contract has drifted from
the exporter.

## What happens after validation

`nvkectl` installs the charts the file names in the order it holds. Each chart
gets the shared values from the embedded stack directory (see PR #1996) plus an
overlay rendered from the user's inputs: the site spec and the stack config.
The overlay is to be implemented.
