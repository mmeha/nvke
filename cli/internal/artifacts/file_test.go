// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package artifacts

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func decodeFile(t *testing.T, path string) *File {
	t.Helper()
	r, err := os.Open(filepath.Join("testdata", path))
	require.NoError(t, err)
	defer r.Close()
	f, err := Decode(r)
	require.NoError(t, err, path)
	return f
}

// TestDecodeNVKRExport decodes two unmodified nvkr release export documents
// and checks that the fields nvkectl reads come through, and that the rest
// of the document does not get in the way.
func TestDecodeNVKRExport(t *testing.T) {
	t.Run("helm chart and apt repository", func(t *testing.T) {
		f := decodeFile(t, "nvkr/helm-export.json")
		assert.Equal(t, "sha256:3f92237e0617d36f56df83a0de80bcc76c3cee27ac2770b9ee119627400f58ac", f.RootDigest)
		require.Len(t, f.Outputs, 2)

		chart := f.Output("gpu-runtime")
		require.NotNil(t, chart)
		assert.Equal(t, "helm-oci", chart.Kind)
		assert.Equal(t, "1.0.0", chart.ChartVersion)
		assert.True(t, strings.HasSuffix(chart.Reference, "@sha256:3c5490c74f24352b9dc8da0bb032dea92b5c531ef4fa7a640ecbd08f7cbe8888"))
		assert.Nil(t, chart.Platforms())

		apt := f.Output("worker-apt")
		require.NotNil(t, apt)
		assert.Equal(t, "apt-repository", apt.Kind)

		// Evidence is present but carries no source for either output.
		require.Len(t, f.Evidence, 2)
		assert.Nil(t, f.Source("gpu-runtime"))
		assert.Nil(t, f.Output("cwe"))
	})

	t.Run("executables with per-output source", func(t *testing.T) {
		f := decodeFile(t, "nvkr/exec-export.json")
		require.Len(t, f.Outputs, 4)

		alpha := f.Output("alpha")
		require.NotNil(t, alpha)
		assert.Equal(t, "executable", alpha.Kind)
		assert.Equal(t, []string{"linux/amd64"}, alpha.Platforms())

		// The source lives per evidence entry. alpha and beta come from one
		// repository at different commits; omega has none.
		require.NotNil(t, f.Source("alpha"))
		assert.Equal(t, "https://example.test/one", f.Source("alpha").Repo)
		assert.Equal(t, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", f.Source("alpha").Commit)
		assert.Equal(t, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", f.Source("beta").Commit)
		assert.Nil(t, f.Source("omega"))
	})
}

// TestDecodeValid decodes the NVKE sample and checks the shapes the
// installer relies on: a multi-platform image, a single-platform image, a
// chart, and a source read through evidence.
func TestDecodeValid(t *testing.T) {
	f := decodeFile(t, "valid.json")
	require.Len(t, f.Outputs, 11)

	cwe := f.Output("cwe")
	require.NotNil(t, cwe)
	assert.Equal(t, "oci", cwe.Kind)
	assert.ElementsMatch(t, []string{"linux/amd64", "linux/arm64"}, cwe.Platforms())
	require.NotNil(t, f.Source("cwe"))
	assert.Equal(t, "https://github.com/dsx-ai-factory/nvke", f.Source("cwe").Repo)

	profiles := f.Output("controller-profiles")
	require.NotNil(t, profiles)
	assert.Equal(t, []string{"linux/amd64"}, profiles.Platforms())

	spire := f.Output("spire")
	require.NotNil(t, spire)
	assert.Equal(t, "helm-oci", spire.Kind)
	assert.Equal(t, "0.30.1", spire.ChartVersion)
	assert.Nil(t, f.Source("spire"))
}

// TestDecodeMinimal decodes a hand-written file that carries only the
// outputs list. Nothing else is required to decode.
func TestDecodeMinimal(t *testing.T) {
	f := decodeFile(t, "cases/minimal-hand-written.json")
	assert.Empty(t, f.RootDigest)
	assert.Empty(t, f.Evidence)
	assert.Nil(t, f.Source("cwe"))
	require.NotNil(t, f.Output("cwe"))
	assert.ElementsMatch(t, []string{"linux/amd64", "linux/arm64"}, f.Output("cwe").Platforms())
	assert.Equal(t, []string{"linux/amd64"}, f.Output("controller-profiles").Platforms())
	assert.Equal(t, "0.30.1", f.Output("spire").ChartVersion)
}

// TestDecodeCases decodes every case file. Only the file that is not JSON
// fails here; the other rules belong to the validator.
func TestDecodeCases(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "cases", "*.json"))
	require.NoError(t, err)
	require.NotEmpty(t, files)
	for _, path := range files {
		name := filepath.Base(path)
		t.Run(name, func(t *testing.T) {
			r, err := os.Open(path)
			require.NoError(t, err)
			defer r.Close()
			_, err = Decode(r)
			if name == "not-json.json" {
				require.ErrorContains(t, err, "not valid JSON")
				return
			}
			require.NoError(t, err)
		})
	}
}
