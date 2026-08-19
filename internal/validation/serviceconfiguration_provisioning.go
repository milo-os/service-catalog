// SPDX-License-Identifier: AGPL-3.0-only

package validation

import (
	"errors"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
	"go.miloapis.com/service-catalog/internal/provisioning"
)

// validateProvisioning checks the intra-document rules for a spec.provisioning
// declaration: that every embedded object decodes into a write the platform
// will make, and that no two objects in a declaration contend for one name.
//
// This is the first of two enforcement points. It gives the provider a
// synchronous error instead of a refusal buried in a consumer's status later.
// It is not the binding one: the controller repeats every check before it
// writes, because this webhook can be absent from the cluster and a
// configuration admitted under an earlier schema stays in etcd. Enforcing only
// here, or leaving it to RBAC, would describe a control that is not in force:
// the operator writes into consumer control planes as system:masters.
func validateProvisioning(sc *servicesv1alpha1.ServiceConfiguration) field.ErrorList {
	var allErrs field.ErrorList

	if sc.Spec.Provisioning == nil {
		return allErrs
	}

	fldPath := field.NewPath("spec", "provisioning", "resources")
	seen := make(map[string]struct{}, len(sc.Spec.Provisioning.Resources))

	for i, res := range sc.Spec.Provisioning.Resources {
		itemPath := fldPath.Index(i)

		if _, dup := seen[res.Name]; dup {
			allErrs = append(allErrs, field.Duplicate(itemPath.Child("name"), res.Name))
		}
		seen[res.Name] = struct{}{}

		type identity struct {
			gvk  schema.GroupVersionKind
			name string
		}
		installed := make(map[identity]struct{}, len(res.Objects))

		for j, raw := range res.Objects {
			objPath := itemPath.Child("objects").Index(j)

			obj, err := provisioning.Decode(raw.RawExtension)
			if err != nil {
				var invalid *provisioning.ErrObjectInvalid
				path := objPath
				if errors.As(err, &invalid) {
					for _, segment := range strings.Split(invalid.Field, ".") {
						path = path.Child(segment)
					}
				}
				allErrs = append(allErrs, field.Invalid(path, string(raw.Raw), err.Error()))
				continue
			}

			// Two objects under one name in one declaration cannot both be
			// installed, and the second would silently win.
			id := identity{gvk: obj.GVK, name: obj.Name}
			if _, dup := installed[id]; dup {
				allErrs = append(allErrs, field.Duplicate(objPath,
					fmt.Sprintf("%s %s", obj.GVK.Kind, obj.Name)))
			}
			installed[id] = struct{}{}
		}
	}

	return allErrs
}
