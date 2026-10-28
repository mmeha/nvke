# Artifact file contract

The artifact file lists every image and chart that `nvkectl` installs on the
seed and on the site. It is the only source of images and charts. The stack
config names no images. `nvkectl` knows which outputs it expects, by name,
and refuses a file that lacks one.

The contract is the Go type `File` in `file.go`. The file is a JSON object.
`nvkectl` decodes the fields of `File` and drops everything else. Where the
document comes from does not matter. `nvkr release export` produces one that
satisfies the contract unmodified. A build that does not use NVKR writes the
same fields by hand; `testdata/cases/minimal-hand-written.json` is the
smallest such file.

## What nvkectl decodes

| Field | Required | Meaning |
| --- | --- | --- |
| `outputs[]` | yes | A list of images and charts. `nvkectl` looks an output up by `name`. |
| `rootDigest` | no | The release's identity. Recorded in the install record when present. |
| `evidence[]` | no | Where the source of an output lives. Read for the install record only. |

Per output:

| Key | Required | Meaning |
| --- | --- | --- |
| `name` | yes | The lookup key. See the table of outputs. |
| `kind` | yes | `oci` for an image, `helm-oci` for a chart. |
| `reference` | yes | The OCI reference the install pulls from. It must carry `@sha256:<64 hex>`. |
| `os`, `arch` | image | The platform of a single-platform image. |
| `imageIndex.children[].platform` | image | The platforms of a multi-platform image. One of `os`/`arch` or `imageIndex` is present. |
| `chartVersion` | chart | The chart version. |

Per evidence entry:

| Key | Meaning |
| --- | --- |
| `binding.outputs[]` | The outputs this entry describes. |
| `source.repo`, `source.commit` | The checkout the subject was built from. |

Every other key in the document, at any level, is dropped at decode time.
`nvkr release export` emits many more: the inspection summary, the release
reference, the publisher's qualification, per-output digests, build digests,
components and artifact types, and SBOM digests. `nvkectl` reads none of
them, so it does not decode them. A key NVKR adds later is dropped the same
way. A `schemaVersion`, when present, is not checked.

There is no release-wide source. A source describes its own subject, and
`nvkectl` reads it per output. The install record carries the source of every
output that has one. A file without `evidence` installs; its record then has
no source.

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

Outputs with other names are dropped. NVKR's exports carry the `nvkectl`
binaries, apt repositories and disk images next to these; `nvkectl` does not
read them.

### Where the names come from

The names are the output names of NVKR's NVKE definition,
`inputs/definitions/nvke/definition.yaml` in the `nvkr` repository, checked at
commit `9686e3cd`. Eight of the nine required outputs are there under these
names, with the kinds this table wants. A change to a name happens in that
definition first and here second.

What the definition does not have today:

- `controller-profiles`. The definition has `nico-reference-profiles-fixture`,
  which it describes as reference test data, not the runnable profiles. The
  controller chart refuses to render without a profiles image, so this stays
  required. An export of today's definition fails validation on it until
  NVKR adds the output.
- `nico` and `nico-prereqs`. These are third-party charts NVKE does not
  build, like `postgresql` and `spire`. They are optional here. A seed that
  runs NICo needs their pins in the file. Until NVKR passes them through, the
  file is extended by hand with the two entries.

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

`nvkectl` writes the sha256 digest of the file into the install record, with
`rootDigest` and the per-output sources when the file has them. The seed and
the site then provably used the same file.

## Validation

`nvkectl` validates the file before it installs anything. The checks run in
this order and stop at the first failure:

1. The file is JSON.
2. Every output name appears once. Every required output is present.
   `nico-prereqs` is present when `nico` is.
3. Every NVKE output has the kind its role needs. Its `reference` is present
   and carries a digest.
4. Every image output serves the target platform.
5. Every chart version is in the supported range.
6. Every reference resolves. The registry is reachable.
7. `helm template` of each chart with the embedded values, then a server-side
   dry run against the target cluster.
8. Cluster preconditions: a default StorageClass, cert-manager and Cluster API
   CRDs, image access, nodes able to attest, a VIP that resolves.

A failure on one output names it as `outputs[name=<name>]`, for example
`outputs[name=cwe]: reference has no digest`. A failure on the document as a
whole names the key. Every failure points the user to this document.

Steps 1 to 5 need no network and no cluster. `testdata/cases.yaml` lists the
expected result for every test file. `file_test.go` covers decoding, including
two unmodified exports from NVKR's own test fixtures under `testdata/nvkr/`.
Those two carry no NVKE output, so the only validation error they may produce
is the missing required output. Any other error means the contract has
drifted from the exporter.

## What happens after validation

`nvkectl` installs the charts the file names in the order it holds. Each chart
gets the shared values from the embedded stack directory (see PR #1996) plus an
overlay rendered from the user's inputs: the site spec and the stack config.
The overlay is to be implemented.
