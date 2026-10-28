// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/discovery"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/manager/signals"

	"github.com/dsx-ai-factory/nvke/management-engine/internal/public"
	"github.com/dsx-ai-factory/nvke/management-engine/pkg/resource"
)

func main() {
	if err := run(); err != nil {
		slog.Error("REST server stopped", "error", err)
		os.Exit(1)
	}
}

// run mounts the selected public surface and serves it until shutdown.
func run() error {
	surface := flag.String("surface", "", "REST surface: tenant or operator")
	address := flag.String("listen", ":8080", "HTTP listen address")
	contextName := flag.String("context", "", "Datastore kubeconfig context")
	namespace := flag.String("namespace", "tenant-test", "Trusted tenant namespace")
	datastoreID := flag.String("datastore-id", "", "Stable datastore identity for opaque tokens")
	keyFile := flag.String("token-key-file", "", "File containing a persistent 32-byte token key")
	flag.Parse()

	surfaces := map[string][]resource.Registration{"tenant": public.Tenant(), "operator": public.Operator()}
	registrations, ok := surfaces[*surface]
	if !ok || *datastoreID == "" {
		return fmt.Errorf("valid --surface and --datastore-id are required")
	}
	if *surface == "tenant" && len(validation.IsDNS1123Label(*namespace)) != 0 {
		return fmt.Errorf("--namespace must be a valid Kubernetes namespace")
	}
	key, err := os.ReadFile(*keyFile)
	if err != nil {
		return fmt.Errorf("read token key: %w", err)
	}
	tokens, err := resource.NewTokens(key)
	if err != nil {
		return fmt.Errorf("configure tokens: %w", err)
	}
	datastore, err := config.GetConfigWithContext(*contextName)
	if err != nil {
		return fmt.Errorf("load datastore configuration: %w", err)
	}
	datastore.Timeout = 5 * time.Second
	disco, err := discovery.NewDiscoveryClientForConfig(datastore)
	if err != nil {
		return fmt.Errorf("configure readiness discovery: %w", err)
	}
	// Tilt supplies one trusted namespace; caller authentication is outside this dev host.
	cfg := resource.ClientConfig{
		Datastore: datastore, Surface: *surface, DatastoreID: *datastoreID, Tokens: tokens,
		Namespace: func(context.Context) (string, error) { return *namespace, nil },
	}
	mux := http.NewServeMux()
	for _, registration := range registrations {
		if err := registration.Mount(mux, cfg); err != nil {
			return fmt.Errorf("mount %s surface: %w", *surface, err)
		}
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if _, err := disco.ServerResourcesForGroupVersionWithContext(r.Context(), "internal.nvke.nvidia.com/v1"); err != nil {
			http.Error(w, "datastore unavailable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	server := &http.Server{
		Addr: *address, Handler: resource.WithLimits(mux, resource.ServerOptions{}),
		ReadHeaderTimeout: 5 * time.Second, WriteTimeout: 35 * time.Second, IdleTimeout: time.Minute,
	}
	ctx := signals.SetupSignalHandler()
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.ListenAndServe() }()
	slog.Info("serving REST API", "surface", *surface, "address", *address)
	select {
	case err := <-serveErr:
		return fmt.Errorf("serve HTTP: %w", err)
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shut down HTTP: %w", err)
		}
		return nil
	}
}
