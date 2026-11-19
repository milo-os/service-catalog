// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/managedfields"
	clientgoapplyconfigurations "k8s.io/client-go/applyconfigurations"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/kube-openapi/pkg/validation/spec"
	"sigs.k8s.io/yaml"
)

// crdTypeConvertedResources are the CRDs whose server-side apply merge rules
// the fake client takes from the generated CRD schema. Without one it deduces
// a schema and treats every list as atomic, so two managers applying different
// conditions would replace each other's instead of merging per type.
var crdTypeConvertedResources = []string{
	"services.miloapis.com_serviceentitlements.yaml",
	"services.miloapis.com_serviceconsumers.yaml",
}

var (
	crdTypeConverterOnce sync.Once
	crdTypeConverter     managedfields.TypeConverter
	crdTypeConverterErr  error
)

// fakeTypeConverters returns the type converters for a fake client: the CRD
// schemas first, then the fake client's own defaults.
func fakeTypeConverters() []managedfields.TypeConverter {
	crdTypeConverterOnce.Do(func() {
		crdTypeConverter, crdTypeConverterErr = newCRDTypeConverter()
	})
	if crdTypeConverterErr != nil {
		panic(crdTypeConverterErr)
	}
	clientGoScheme := runtime.NewScheme()
	if err := scheme.AddToScheme(clientGoScheme); err != nil {
		panic(err)
	}
	return []managedfields.TypeConverter{
		crdTypeConverter,
		clientgoapplyconfigurations.NewTypeConverter(clientGoScheme),
		managedfields.NewDeducedTypeConverter(),
	}
}

func newCRDTypeConverter() (managedfields.TypeConverter, error) {
	definitions := map[string]*spec.Schema{}
	for _, file := range crdTypeConvertedResources {
		raw, err := os.ReadFile(filepath.Join("..", "..", "config", "base", "crd", "bases", file))
		if err != nil {
			return nil, err
		}
		var crd apiextensionsv1.CustomResourceDefinition
		if err := yaml.Unmarshal(raw, &crd); err != nil {
			return nil, fmt.Errorf("decode %s: %w", file, err)
		}
		for _, version := range crd.Spec.Versions {
			props, err := json.Marshal(version.Schema.OpenAPIV3Schema)
			if err != nil {
				return nil, err
			}
			var schema spec.Schema
			if err := json.Unmarshal(props, &schema); err != nil {
				return nil, fmt.Errorf("convert %s schema: %w", file, err)
			}
			// The API server merges a custom resource's metadata by the
			// ObjectMeta schema, not the CRD's bare "type: object", which
			// would make finalizers atomic.
			metadata := schema.Properties["metadata"]
			metadata.Properties = map[string]spec.Schema{"finalizers": {
				SchemaProps: spec.SchemaProps{
					Type:  []string{"array"},
					Items: &spec.SchemaOrArray{Schema: spec.StringProperty()},
				},
				VendorExtensible: spec.VendorExtensible{Extensions: spec.Extensions{"x-kubernetes-list-type": "set"}},
			}}
			metadata.AddExtension("x-kubernetes-preserve-unknown-fields", true)
			schema.Properties["metadata"] = metadata
			schema.AddExtension("x-kubernetes-group-version-kind", []any{map[string]any{
				"group":   crd.Spec.Group,
				"version": version.Name,
				"kind":    crd.Spec.Names.Kind,
			}})
			definitions[crd.Spec.Group+"."+version.Name+"."+crd.Spec.Names.Kind] = &schema
		}
	}
	return managedfields.NewTypeConverter(definitions, false)
}
