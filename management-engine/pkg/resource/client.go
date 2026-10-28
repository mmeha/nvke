// SPDX-License-Identifier: Apache-2.0

package resource

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"

	"github.com/dsx-ai-factory/nvke/management-engine/pkg/projection"
)

const (
	requestTimeout  = 30 * time.Second
	maxWriteRetries = 3
)

// resourceServer enforces projected intent, trusted namespace scope and spec concurrency.
type resourceServer[E ResourceHandler[I], I ResourceHandler[E]] struct {
	cfg      ClientConfig
	info     ResourceInfo
	resource dynamic.NamespaceableResourceInterface
}

// New creates reusable datastore clients and checks the live resource's basic capabilities.
// Construction contacts the datastore; request-specific scope is resolved later by each operation.
func New[E ResourceHandler[I], I ResourceHandler[E]](cfg ClientConfig, info ResourceInfo) (ResourceServer[E], error) {
	if cfg.Datastore == nil ||
		cfg.Tokens == nil ||
		(info.Scope == Namespaced && cfg.Namespace == nil) ||
		cfg.Surface == "" ||
		cfg.DatastoreID == "" {
		return nil, fmt.Errorf("datastore, tokens, surface, datastore ID and " +
			"a namespace resolver for namespaced resources are required")
	}
	if info.GVK.Empty() || info.Resource == "" {
		return nil, fmt.Errorf("resource metadata is incomplete")
	}
	if info.Scope != Namespaced && info.Scope != ClusterScoped {
		return nil, fmt.Errorf("resource scope is invalid")
	}
	if info.FieldManager == "" {
		info.FieldManager = "nvke-resource"
	}
	cfg.Datastore = rest.CopyConfig(cfg.Datastore)
	cfg.Datastore.Timeout = requestTimeout
	httpClient, err := rest.HTTPClientFor(cfg.Datastore)
	if err != nil {
		return nil, fmt.Errorf("creating datastore HTTP client: %w", err)
	}
	dyn, err := dynamic.NewForConfigAndClient(cfg.Datastore, httpClient)
	if err != nil {
		return nil, fmt.Errorf("creating datastore client: %w", err)
	}
	info.Verbs = operations(info.Verbs)
	if err = checkResource(cfg.Datastore, httpClient, info); err != nil {
		return nil, err
	}
	return &resourceServer[E, I]{
		cfg: cfg, info: info,
		resource: dyn.Resource(info.GVK.GroupVersion().WithResource(info.Resource)),
	}, nil
}

// request checks the registered operation and resolves trusted scope from the request context.
// The returned datastore handle uses the service's credentials.
// Update and Delete retain this handle and namespace across retries.
func (c *resourceServer[E, I]) request(ctx context.Context, op Operation) (dynamic.ResourceInterface, string, error) {
	if !slices.Contains(c.info.Verbs, op) {
		return nil, "", problem(405, "method_not_allowed", "The operation is not supported.")
	}
	if c.info.Scope == ClusterScoped {
		return c.resource, "", nil
	}
	namespace, err := c.cfg.Namespace(ctx)
	if err != nil {
		return nil, "", Unauthorized()
	}
	if len(validation.IsDNS1123Label(namespace)) != 0 {
		return nil, "", Forbidden()
	}
	return c.resource.Namespace(namespace), namespace, nil
}

// binding prevents opaque tokens from being reused for another surface, store, scope or resource.
func (c *resourceServer[E, I]) binding(namespace, name string) tokenBinding {
	return tokenBinding{
		Surface:  c.cfg.Surface,
		Store:    c.cfg.DatastoreID,
		Scope:    namespace,
		Resource: c.info.GVK.String() + "/" + c.info.Resource,
		Name:     name,
	}
}

// project decodes the internal type and converts it into the approved public shape.
func project[E ResourceHandler[I], I ResourceHandler[E]](obj *unstructured.Unstructured) (E, error) {
	var zero E
	internal, err := projection.JSON[I](obj.Object)
	if err != nil {
		return zero, problem(500, "projection_failed", "The resource cannot be represented.")
	}
	value, err := internal.Convert()
	if err != nil {
		return zero, problem(500, "projection_failed", "The resource cannot be represented.")
	}
	return value, nil
}

// result projects the stored object and returns its UID/generation token through per-call options.
func (c *resourceServer[E, I]) result(
	ctx context.Context,
	obj *unstructured.Unstructured,
	namespace string,
) (E, error) {
	value, err := project[E, I](obj)
	if err != nil {
		return value, err
	}
	token, err := c.cfg.Tokens.seal("version", c.binding(namespace, obj.GetName()), tokenValue{
		UID: string(obj.GetUID()), Generation: obj.GetGeneration(),
	})
	if err != nil {
		var zero E
		return zero, problem(500, "token_failed", "The resource token cannot be created.")
	}
	requestOptions(ctx).ETag = token
	return value, nil
}

// intent restricts writes to public identity and supplied spec fields before datastore admission.
// Original JSON preserves omissions and explicit false, zero, empty arrays and optional null values
// that would otherwise be changed by marshaling a typed internal resource.
func (c *resourceServer[E, I]) intent(ctx context.Context, op Operation, in E) (*unstructured.Unstructured, error) {
	supplied := requestOptions(ctx).body
	if supplied == nil {
		data, err := json.Marshal(in)
		if err != nil {
			return nil, Invalid()
		}
		supplied, err = decodeStrict(bytes.NewReader(data), new(E))
		if err != nil {
			return nil, Invalid()
		}
	}
	for key := range supplied {
		if key != "metadata" && key != "spec" {
			return nil, Invalid(FieldError{Field: "/" + key, Code: readOnlyCode, Message: "The field is read-only."})
		}
	}
	metadata, _ := supplied["metadata"].(map[string]any)
	for key := range metadata {
		if key != "name" && key != "uid" {
			return nil, Invalid(FieldError{Field: "/metadata/" + key, Code: readOnlyCode, Message: "The field is read-only."})
		}
	}
	if spec, found := supplied["spec"]; found {
		if _, ok := spec.(map[string]any); !ok {
			return nil, Invalid(FieldError{Field: "/spec", Code: invalidCode, Message: "Spec must be an object."})
		}
	}
	identity := &unstructured.Unstructured{Object: supplied}
	if err := validName(identity.GetName()); err != nil {
		return nil, err
	}
	if op == OpCreate && identity.GetUID() != "" {
		return nil, Invalid(FieldError{Field: uidPointer, Code: readOnlyCode, Message: "UID is read-only."})
	}
	if err := in.Validate(); err != nil {
		return nil, err
	}
	converted, err := in.Convert()
	if err != nil {
		return nil, err
	}
	if err = converted.Validate(); err != nil {
		return nil, err
	}
	data, err := projection.JSON[map[string]any](converted)
	if err != nil {
		return nil, err
	}
	internal := &unstructured.Unstructured{Object: data}
	if internal.GetName() != identity.GetName() || internal.GetUID() != identity.GetUID() {
		return nil, fmt.Errorf("resource conversion changed identity")
	}
	// Select only writable fields, even when the internal type contains private metadata or status.
	obj := &unstructured.Unstructured{Object: map[string]any{}}
	obj.SetName(identity.GetName())
	obj.SetUID(identity.GetUID())
	if spec, found := supplied["spec"]; found {
		obj.Object["spec"] = spec
	}
	obj.SetGroupVersionKind(c.info.GVK)
	return obj, nil
}

// apply submits the manager's complete intent without forcing ownership conflicts.
// Server-side apply releases omitted fields; fields owned only by this manager are removed or reset to defaults.
func (c *resourceServer[E, I]) apply(
	ctx context.Context,
	r dynamic.ResourceInterface,
	obj *unstructured.Unstructured,
) (*unstructured.Unstructured, error) {
	data, err := json.Marshal(obj.Object)
	if err != nil {
		return nil, fmt.Errorf("encoding intent: %w", err)
	}
	force := false
	return r.Patch(ctx, obj.GetName(), types.ApplyPatchType, data, metav1.PatchOptions{
		FieldManager: c.info.FieldManager, Force: &force, FieldValidation: "Strict",
	})
}

// Create atomically creates only; duplicate and racing attempts cannot replace a resource.
func (c *resourceServer[E, I]) Create(ctx context.Context, in E) (E, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	var zero E
	obj, err := c.intent(ctx, OpCreate, in)
	if err != nil {
		return zero, err
	}
	r, namespace, err := c.request(ctx, OpCreate)
	if err != nil {
		return zero, err
	}
	obj.SetNamespace(namespace)
	// The apply precondition makes creation atomic without a separate existence check.
	obj.SetResourceVersion("0")
	created, err := c.apply(ctx, r, obj)
	if duplicateApply(err) {
		return zero, problem(409, "already_exists", "The resource already exists.")
	}
	if err != nil {
		return zero, translate[E](err)
	}
	return c.result(ctx, created, namespace)
}

// Get projects one resource and returns its opaque spec-concurrency token.
func (c *resourceServer[E, I]) Get(ctx context.Context, name string) (E, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	var zero E
	if err := validName(name); err != nil {
		return zero, err
	}
	r, namespace, err := c.request(ctx, OpGet)
	if err != nil {
		return zero, err
	}
	obj, err := r.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return zero, translate[E](err)
	}
	return c.result(ctx, obj, namespace)
}

// List returns one datastore snapshot page with a default size of 100 and a maximum of 500.
// Continuation tokens are bound to request scope and page size and expire after ten minutes.
func (c *resourceServer[E, I]) List(ctx context.Context, opts ListOptions) (List[E], error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	page := List[E]{Items: []E{}}
	if opts.PageSize == 0 {
		opts.PageSize = 100
	}
	if opts.PageSize < 1 || opts.PageSize > 500 {
		return page, Invalid(FieldError{
			Field: "page_size", Code: invalidCode, Message: "Page size must be between 1 and 500.",
		})
	}
	r, namespace, err := c.request(ctx, OpList)
	if err != nil {
		return page, err
	}
	binding := c.binding(namespace, "")
	binding.PageSize = opts.PageSize
	listOpts := metav1.ListOptions{Limit: int64(opts.PageSize)}
	if opts.PageKey != "" {
		value, openErr := c.cfg.Tokens.open("page", binding, opts.PageKey)
		if openErr != nil {
			return page, openErr
		}
		listOpts.Continue = value.Continue
	}
	list, err := r.List(ctx, listOpts)
	if err != nil {
		return page, translate[E](err)
	}
	for i := range list.Items {
		value, projectErr := project[E, I](&list.Items[i])
		if projectErr != nil {
			return page, projectErr
		}
		page.Items = append(page.Items, value)
	}
	if list.GetContinue() != "" {
		page.PageKey, err = c.cfg.Tokens.seal("page", binding, tokenValue{
			Continue: list.GetContinue(), Expires: time.Now().Add(10 * time.Minute).Unix(),
		})
		if err != nil {
			return page, problem(500, "token_failed", "The page token cannot be created.")
		}
		page.HasMore = true
	}
	return page, nil
}

// validName rejects invalid Kubernetes object names before a datastore request is made.
func validName(name string) error {
	if len(validation.IsDNS1123Subdomain(name)) != 0 {
		return Invalid(FieldError{Field: namePointer, Code: invalidCode, Message: "A valid resource name is required."})
	}
	return nil
}

// expected verifies a required version token's integrity and binding to this resource and scope.
func (c *resourceServer[E, I]) expected(namespace, name, version string) (tokenValue, error) {
	if version == "" {
		return tokenValue{}, problem(428, "precondition_required", "A spec-concurrency precondition is required.")
	}
	return c.cfg.Tokens.open("version", c.binding(namespace, name), version)
}

// precondition rejects recreation or changed desired state while allowing status-only updates.
// ResourceVersion is deliberately excluded: each write attempt uses the latest value to close races after its read.
func precondition(obj *unstructured.Unstructured, expected tokenValue) error {
	if string(obj.GetUID()) != expected.UID || obj.GetGeneration() != expected.Generation {
		return problem(412, "precondition_failed", "The resource identity or desired state has changed.")
	}
	return nil
}

// Update applies this manager's complete writable intent under UID and generation preconditions.
// Omitted fields owned only by this manager are removed or reset to defaults.
// Adopting other writers' spec fields is outside the PoC.
// Conflicts get at most three retries; each read must still match the original UID and generation.
func (c *resourceServer[E, I]) Update(ctx context.Context, name string, in E) (E, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	var zero E
	if err := validName(name); err != nil {
		return zero, err
	}
	r, namespace, err := c.request(ctx, OpUpdate)
	if err != nil {
		return zero, err
	}
	expected, err := c.expected(namespace, name, requestOptions(ctx).IfMatch)
	if err != nil {
		return zero, err
	}
	for attempt := 0; attempt <= maxWriteRetries; attempt++ {
		current, err := r.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return zero, translate[E](err)
		}
		if err = precondition(current, expected); err != nil {
			return zero, err
		}
		obj, err := c.intent(ctx, OpUpdate, in)
		if err != nil {
			return zero, err
		}
		if obj.GetName() != name {
			return zero, Invalid(FieldError{
				Field: namePointer, Code: "identity_mismatch", Message: "The body name must match the request path.",
			})
		}
		if obj.GetUID() != "" && obj.GetUID() != current.GetUID() {
			return zero, Invalid(FieldError{
				Field: uidPointer, Code: "identity_mismatch", Message: "UID does not match the resource.",
			})
		}
		obj.SetNamespace(namespace)
		obj.SetUID(current.GetUID())
		obj.SetResourceVersion(current.GetResourceVersion())
		updated, err := c.apply(ctx, r, obj)
		if resourceVersionConflict(err) {
			continue
		}
		if err != nil {
			return zero, translate[E](err)
		}
		return c.result(ctx, updated, namespace)
	}
	return zero, problem(409, "conflict", "The resource changed too frequently; retry with a fresh read.")
}

// Delete requests deletion with the token's UID and generation and fresh resourceVersion preconditions on each retry.
// Success acknowledges the delete request; it does not wait for finalizers or dependent resources to disappear.
func (c *resourceServer[E, I]) Delete(ctx context.Context, name string, opts DeleteOptions) error {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	if err := validName(name); err != nil {
		return err
	}
	policy := metav1.DeletePropagationBackground
	switch opts.PropagationPolicy {
	case "", "background":
	case "foreground":
		policy = metav1.DeletePropagationForeground
	default:
		return Invalid(FieldError{
			Field: "propagation_policy", Code: invalidCode, Message: "Use background or foreground propagation.",
		})
	}
	r, namespace, err := c.request(ctx, OpDelete)
	if err != nil {
		return err
	}
	expected, err := c.expected(namespace, name, requestOptions(ctx).IfMatch)
	if err != nil {
		return err
	}
	for attempt := 0; attempt <= maxWriteRetries; attempt++ {
		current, err := r.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return translate[E](err)
		}
		if err = precondition(current, expected); err != nil {
			return err
		}
		uid, rv := current.GetUID(), current.GetResourceVersion()
		err = r.Delete(ctx, name, metav1.DeleteOptions{
			PropagationPolicy: &policy,
			Preconditions:     &metav1.Preconditions{UID: &uid, ResourceVersion: &rv},
		})
		if resourceVersionConflict(err) {
			continue
		}
		if err != nil {
			return translate[E](err)
		}
		return nil
	}
	return problem(409, "conflict", "The resource changed too frequently; retry with a fresh read.")
}
