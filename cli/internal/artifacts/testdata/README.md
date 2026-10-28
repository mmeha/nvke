# Artifact file test data

`valid.json` is a complete artifact file in the shape `nvkr release export`
emits, with the NVKE output names and synthetic digests. `cases/` holds one
file per validation rule in `CONTRACT.md`, each derived from `valid.json`
with one change, plus the valid variants: the minimal hand-written file with
only `outputs`, NICo absent, no source for `cwe`, and extra outputs and keys
that `nvkectl` drops.

`nvkr/` holds two unmodified exports produced by the real exporter from
NVKR's own test fixtures. `file_test.go` decodes them and checks that the
fields `nvkectl` reads come through. They carry no NVKE output, so the
validator must fail them on the missing required output and on nothing else.

`cases.yaml` lists every file with the result the validator must return:
`ok`, or a substring of the error message, and the target platform when it is
not `linux/amd64`. The validator and its table-driven test over `cases.yaml`
follow in a later change.
