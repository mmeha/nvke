# Artifact file test cases

`valid.json` is a complete artifact file in the shape `nvkr release export`
emits, with the NVKE output names and synthetic digests. `cases/` holds one
file per rule in `CONTRACT.md`, each derived from `valid.json` with one
change. `nvkr/` holds two unmodified exports produced by the real exporter
from NVKR's own test fixtures. They carry no NVKE output, so they must fail
on the missing required output and on nothing else.

`cases.yaml` lists every file with the result the loader must return: `ok`,
or a substring of the error message, and the target platform when it is not
`linux/amd64`. The table-driven test in the follow-up PR reads `cases.yaml`,
loads each file through the loader, and compares the result with `expect`.
