package addon

import (
	"context"
	"encoding/json"
	"fmt"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	sigsyaml "sigs.k8s.io/yaml"
)

// ApplyManifest applies a single YAML manifest via server-side apply.
//
// On a NoKindMatchError, this forces the REST mapper to re-run discovery
// (via ClusterHandle.ResetRESTMapper) and retries once — see that field's
// own doc comment for why this must be done explicitly here rather than
// trusted to DeferredDiscoveryRESTMapper's own built-in retry, which
// doesn't actually fire in kluster's case. This is what makes ApplyManifest
// work for CRDs installed by a prior addon earlier in the same `kluster up`
// run, not just ones that existed before this process started.
func ApplyManifest(ctx context.Context, h ClusterHandle, yamlData string) error {
	jsonData, err := sigsyaml.YAMLToJSON([]byte(yamlData))
	if err != nil {
		return fmt.Errorf("parse manifest YAML: %w", err)
	}

	obj := &unstructured.Unstructured{}
	if err := obj.UnmarshalJSON(jsonData); err != nil {
		return fmt.Errorf("decode manifest: %w", err)
	}

	gvk := obj.GroupVersionKind()
	mapping, err := h.RESTMapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if apimeta.IsNoMatchError(err) && h.ResetRESTMapper != nil {
		h.ResetRESTMapper()
		mapping, err = h.RESTMapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	}
	if err != nil {
		return fmt.Errorf("REST mapping for %v: %w", gvk, err)
	}

	var dr dynamic.ResourceInterface
	if mapping.Scope.Name() == apimeta.RESTScopeNameNamespace {
		dr = h.DynClient.Resource(mapping.Resource).Namespace(obj.GetNamespace())
	} else {
		dr = h.DynClient.Resource(mapping.Resource)
	}

	_, err = dr.Patch(ctx, obj.GetName(), types.ApplyPatchType, jsonData, metav1.PatchOptions{
		FieldManager: "kluster",
		Force:        boolPtr(true),
	})
	return err
}

func boolPtr(b bool) *bool { return &b }

// waitForCondition polls obj.status.conditions until type==condType and status=="True".
func waitForCondition(obj *unstructured.Unstructured, condType string) bool {
	conditions, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	for _, c := range conditions {
		m, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		if m["type"] == condType && m["status"] == "True" {
			return true
		}
	}
	return false
}

// marshalForApply is a convenience wrapper used by apply helpers that build
// objects programmatically rather than from YAML strings.
func marshalForApply(obj *unstructured.Unstructured) ([]byte, error) {
	data, err := json.Marshal(obj.Object)
	if err != nil {
		return nil, fmt.Errorf("marshal object: %w", err)
	}
	return data, nil
}
