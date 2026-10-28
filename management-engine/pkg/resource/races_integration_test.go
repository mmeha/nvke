//go:build integration_test

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package resource

import (
	"context"
	"net/http"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestConcurrentWrites(t *testing.T) {
	f := setup(t, true)
	normal := f.client(t)
	for _, tc := range []struct {
		name, method, change string
		changes, attempts    int32
		want                 int
	}{
		{"update-status", http.MethodPatch, "status", 1, 2, 0},
		{"update-exhausted", http.MethodPatch, "status", 4, 4, 409},
		{"update-spec", http.MethodPatch, "spec", 1, 1, 412},
		{"delete-status", http.MethodDelete, "status", 1, 2, 0},
		{"delete-spec", http.MethodDelete, "spec", 1, 1, 412},
		{"delete-recreated", http.MethodDelete, "recreate", 1, 1, 412},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := &RequestOptions{}
			_, err := normal.Create(WithOptions(f.as("org-a"), opts), input(tc.name, "before"))
			require.NoError(t, err)
			_, err = normal.Create(f.as("org-b"), input(tc.name, "other-namespace"))
			require.NoError(t, err)
			var writes, resolutions atomic.Int32
			scoped := *f
			scoped.cfg.Namespace = func(context.Context) (string, error) {
				if resolutions.Add(1) == 1 {
					return "org-a", nil
				}
				return "org-b", nil
			}
			client := scoped.client(t, func(base http.RoundTripper) http.RoundTripper {
				return roundTripFunc(func(req *http.Request) (*http.Response, error) {
					if req.Method == tc.method && writes.Add(1) <= tc.changes {
						obj, err := f.resource().Get(f.ctx, tc.name, metav1.GetOptions{})
						if err != nil {
							return nil, err
						}
						switch tc.change {
						case "status":
							obj.Object["status"] = map[string]any{"counter": int64(writes.Load())}
							_, err = f.resource().UpdateStatus(f.ctx, obj, metav1.UpdateOptions{})
						case "spec":
							version := &RequestOptions{}
							_, err = normal.Get(WithOptions(f.as("org-a"), version), tc.name)
							if err == nil {
								_, err = normal.Update(
									WithOptions(f.as("org-a"), &RequestOptions{IfMatch: version.ETag}),
									tc.name, input(tc.name, "concurrent"),
								)
							}
						case "recreate":
							err = f.resource().Delete(f.ctx, tc.name, metav1.DeleteOptions{})
							if err == nil {
								_, err = normal.Create(f.as("org-a"), input(tc.name, "recreated"))
							}
						}
						if err != nil {
							return nil, err
						}
					}
					return base.RoundTrip(req)
				})
			})
			require.Zero(t, resolutions.Load(), "startup must not resolve request scope")
			ctx := WithOptions(f.ctx, &RequestOptions{IfMatch: opts.ETag})
			if tc.method == http.MethodPatch {
				_, err = client.Update(ctx, tc.name, input(tc.name, "after"))
			} else {
				err = client.Delete(ctx, tc.name, DeleteOptions{})
			}
			if tc.want == 0 {
				require.NoError(t, err)
			} else {
				statusCode(t, err, tc.want)
			}
			require.Equal(t, tc.attempts, writes.Load())
			require.Equal(t, int32(1), resolutions.Load(), "retries must retain the original namespace")
			got, err := normal.Get(f.as("org-a"), tc.name)
			switch {
			case tc.method == http.MethodDelete && tc.want == 0:
				statusCode(t, err, 404)
			default:
				require.NoError(t, err)
				want := "before"
				if tc.want == 0 {
					want = "after"
				}
				if tc.change == "spec" {
					want = "concurrent"
				}
				if tc.change == "recreate" {
					want = "recreated"
				}
				require.Equal(t, want, got.Spec.Value)
			}
			other, err := normal.Get(f.as("org-b"), tc.name)
			require.NoError(t, err)
			require.Equal(t, "other-namespace", other.Spec.Value)
		})
	}
}

func TestOwnershipConflict(t *testing.T) {
	f := setup(t, true)
	normal := f.client(t)
	opts := &RequestOptions{}
	ctx := f.as("org-a")
	_, err := normal.Create(WithOptions(ctx, opts), input("owned", "before"))
	require.NoError(t, err)
	stale := opts.ETag
	obj := f.stored(t, "owned")
	obj.Object["spec"].(map[string]any)["value"] = "native"
	_, err = f.resource().Update(ctx, obj, metav1.UpdateOptions{FieldManager: "native"})
	require.NoError(t, err)
	opts = &RequestOptions{}
	_, err = normal.Get(WithOptions(ctx, opts), "owned")
	require.NoError(t, err)
	var writes atomic.Int32
	client := f.client(t, func(base http.RoundTripper) http.RoundTripper {
		return roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method == http.MethodPatch {
				writes.Add(1)
			}
			return base.RoundTrip(req)
		})
	})
	for _, tc := range []struct {
		token  string
		status int
	}{{opts.ETag, 409}, {stale, 412}, {"", 428}} {
		_, err = client.Update(WithOptions(ctx, &RequestOptions{IfMatch: tc.token}), "owned", input("owned", "attempt"))
		statusCode(t, err, tc.status)
		require.Equal(t, int32(1), writes.Load(), "ownership conflicts are not retried; stale/missing tokens never write")
	}
	require.Equal(t, "native", f.stored(t, "owned").Object["spec"].(map[string]any)["value"])
}

func TestRequestDeadlines(t *testing.T) {
	f := setup(t, true)
	normal := f.client(t)
	for _, httpRequest := range []bool{false, true} {
		t.Run(strconv.FormatBool(httpRequest), func(t *testing.T) {
			name := "deadline-" + strconv.FormatBool(httpRequest)
			opts := &RequestOptions{}
			ctx := f.as("org-a")
			_, err := normal.Create(WithOptions(ctx, opts), input(name, "old"))
			require.NoError(t, err)
			var writes atomic.Int32
			client := f.client(t, func(base http.RoundTripper) http.RoundTripper {
				return roundTripFunc(func(req *http.Request) (*http.Response, error) {
					if req.Method == http.MethodPatch {
						writes.Add(1)
						<-req.Context().Done()
						return nil, req.Context().Err()
					}
					return base.RoundTrip(req)
				})
			})
			start := time.Now()
			if httpRequest {
				mux := http.NewServeMux()
				Mount(mux, "/widgets", client)
				handler := WithLimits(mux, ServerOptions{RequestTimeout: 50 * time.Millisecond})
				response := requestHTTP(handler, ctx, "PUT", "/widgets/"+name,
					`{"metadata":{"name":"`+name+`"},"spec":{"value":"new"}}`, strconv.Quote(opts.ETag))
				require.Equal(t, 503, response.Code, response.Body.String())
			} else {
				bounded, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
				defer cancel()
				_, err = client.Update(WithOptions(bounded, &RequestOptions{IfMatch: opts.ETag}), name, input(name, "new"))
				require.Error(t, err)
			}
			require.Less(t, time.Since(start), time.Second)
			require.Equal(t, int32(1), writes.Load())
			require.Equal(t, "old", f.stored(t, name).Object["spec"].(map[string]any)["value"])
		})
	}
}
