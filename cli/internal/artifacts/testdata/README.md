# Artifact file test cases

`valid.yaml` is a complete artifact file. `cases.yaml` lists every file in this
directory with the result the loader must return: `ok`, or a substring of the
error message. The table-driven test in the follow-up PR reads `cases.yaml`,
loads each file through the loader, and compares the result with `expect`.
Paths inside the files resolve against the file's own directory, so
`valid.yaml` finds the OCI layout under `layouts/`.
