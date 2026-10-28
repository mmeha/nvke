// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package artifacts reads the artifact file: the JSON document that names
// every image and chart nvkectl installs. CONTRACT.md in this directory is
// the contract. The types here are the whole of what nvkectl decodes; every
// other key in the document is dropped at decode time.
package artifacts

import (
	"encoding/json"
	"fmt"
	"io"
)

// File is the subset of the artifact file that nvkectl reads.
type File struct {
	// RootDigest identifies the release the file describes. It is recorded
	// in the install record when present.
	RootDigest string `json:"rootDigest"`
	// Outputs lists the images and charts, one entry per name.
	Outputs []Output `json:"outputs"`
	// Evidence carries the source of an output, when the producer knows it.
	Evidence []Evidence `json:"evidence"`
}

// Output is one image or chart.
type Output struct {
	Name string `json:"name"`
	// Kind is oci for an image and helm-oci for a chart.
	Kind string `json:"kind"`
	// Reference is the digest-pinned OCI reference the install pulls from.
	Reference string `json:"reference"`
	// OS and Arch name the platform of a single-platform image.
	OS   string `json:"os"`
	Arch string `json:"arch"`
	// ImageIndex lists the platforms of a multi-platform image.
	ImageIndex *ImageIndex `json:"imageIndex"`
	// ChartVersion is set on a chart.
	ChartVersion string `json:"chartVersion"`
}

// ImageIndex is the platform membership of a multi-platform image.
type ImageIndex struct {
	Children []ImageIndexChild `json:"children"`
}

// ImageIndexChild is one platform of a multi-platform image.
type ImageIndexChild struct {
	// Platform is os/arch, for example linux/amd64.
	Platform string `json:"platform"`
}

// Evidence binds a source to the outputs it describes.
type Evidence struct {
	Binding EvidenceBinding `json:"binding"`
	Source  *Source         `json:"source"`
}

// EvidenceBinding names the outputs an evidence entry describes.
type EvidenceBinding struct {
	Outputs []string `json:"outputs"`
}

// Source is the checkout an output was built from.
type Source struct {
	Repo   string `json:"repo"`
	Commit string `json:"commit"`
}

// Decode reads an artifact file from r. It accepts any JSON object and keeps
// the fields of File; unknown keys are dropped. Decode does not validate the
// content.
func Decode(r io.Reader) (*File, error) {
	var f File
	if err := json.NewDecoder(r).Decode(&f); err != nil {
		return nil, fmt.Errorf("artifact file is not valid JSON: %w", err)
	}
	return &f, nil
}

// Output returns the output with the given name, or nil.
func (f *File) Output(name string) *Output {
	for i := range f.Outputs {
		if f.Outputs[i].Name == name {
			return &f.Outputs[i]
		}
	}
	return nil
}

// Source returns the source bound to the named output, or nil when no
// evidence entry carries one.
func (f *File) Source(name string) *Source {
	for _, e := range f.Evidence {
		if e.Source == nil {
			continue
		}
		for _, o := range e.Binding.Outputs {
			if o == name {
				return e.Source
			}
		}
	}
	return nil
}

// Platforms returns the os/arch platforms an image output serves: the
// single platform from OS and Arch, or every child of the image index. A
// chart has none.
func (o *Output) Platforms() []string {
	if o.ImageIndex != nil {
		platforms := make([]string, 0, len(o.ImageIndex.Children))
		for _, c := range o.ImageIndex.Children {
			platforms = append(platforms, c.Platform)
		}
		return platforms
	}
	if o.OS != "" && o.Arch != "" {
		return []string{o.OS + "/" + o.Arch}
	}
	return nil
}
