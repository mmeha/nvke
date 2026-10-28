# Artifact file contract

The artifact file lists every image and chart that `nvkectl` installs on the
seed and on the site. It is the only source of images and charts. The stack
config names no images. `nvkectl` knows which outputs it expects and refuses a
file that lacks one.

The schema is `nvke.dev/artifacts/v1alpha1`. Unknown top-level keys and
unknown output names are errors.

## Fields

| Field | Required | Meaning |
| --- | --- | --- |
| `schemaVersion` | yes | Must be `nvke.dev/artifacts/v1alpha1`. |
| `source.repo` | yes | The NVKE checkout the artifacts were built from. |
| `source.commit` | yes | The commit of that checkout. |
| `capiVersion` | yes | The Cluster API version used for both the seed and the site. |
| `outputs.<name>` | yes | A list of entries for one output. See below. |

Each entry under `outputs.<name>` has these keys:

| Key | Required | Meaning |
| --- | --- | --- |
| `ref` | one of `ref`, `path` | An OCI reference. It must carry a digest (`@sha256:<64 hex>`). |
| `path` | one of `ref`, `path` | A local OCI layout directory or a chart `.tgz`. Relative paths resolve against the file's directory. |
| `arch` | images only | `amd64` or `arm64`. Chart entries omit it. |
| `chartVersion` | charts only | The chart version. |

An output is a list of entries, not a single object. The seed and the site may
run on different architectures, so one image name may need one entry per
architecture. The list keeps the name as the lookup key and makes a repeated
`(name, arch)` pair an error. A chart is architecture independent and has one
entry.

## Outputs

| Name | Kind | Required | Role |
| --- | --- | --- | --- |
| `cwe` | image | yes | Controller image. |
| `controller-profiles` | image | yes | Profiles image. |
| `capnico-controller` | image | yes | CAPNICo controller image. |
| `controller-chart` | chart | yes | Controller chart. |
| `nktp` | chart | yes | Trust plane chart. |
| `postgresql` | chart | yes | Database chart. |
| `spire-crds` | chart | yes | SPIRE CRDs chart. |
| `spire` | chart | yes | SPIRE chart. |
| `temporal` | chart | yes | Temporal chart. |
| `nico` | chart | no | NICo chart. Its presence selects NICo on the seed. |
| `nico-prereqs` | chart | no | NICo prerequisites chart. Present together with `nico`. |

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

## Sources of the file

NVKE builds write this file. An NVKR release description (`nvkr release export`,
schema `nvkr.dev/release-description/v1`) satisfies this contract by design.
No converter exists.

`nvkectl` writes the sha256 digest of the file into the install record. The
seed and the site then provably used the same file.

## Validation

`nvkectl` validates the file before it installs anything. The checks run in
this order and stop at the first failure:

1. Schema and required fields. Unknown keys are errors.
2. Required outputs are present.
3. Each `ref` carries a digest. Each `path` exists.
4. Each chart version is in the supported range.
5. Each reference resolves. The registry is reachable or the local path is readable.
6. `helm template` of each chart with the embedded values, then a server-side dry run against the target cluster.
7. Cluster preconditions: a default StorageClass, cert-manager and Cluster API CRDs, image access, nodes able to attest, a VIP that resolves.

Every failure names the entry, for example `outputs.cwe[0]: ref has no digest`.
The entry path is `outputs.<name>[<index>]`, where `<index>` is the position
in the list. Every failure points the user to `nvkr` or to this document.

Steps 1 to 4 need no network and no cluster. The test cases under `testdata/`
cover them.

## What happens after validation

`nvkectl` installs the charts the file names in the order it holds. Each chart
gets the shared values from the embedded stack directory (see PR #1996) plus an
overlay rendered from the user's inputs: the site spec and the stack config.
The overlay is to be implemented.
