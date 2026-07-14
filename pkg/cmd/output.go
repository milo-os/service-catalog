package cmd

import (
	"encoding/json"
	"fmt"
	"io"

	"k8s.io/cli-runtime/pkg/genericclioptions"
	"sigs.k8s.io/yaml"

	"go.miloapis.com/service-catalog/pkg/activation"
)

// printJSON serialises obj to indented JSON and writes it to w.
func printJSON(w io.Writer, obj any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(obj); err != nil {
		return fmt.Errorf("encoding JSON: %w", err)
	}
	return nil
}

// printYAML serialises obj to YAML and writes it to w. sigs.k8s.io/yaml
// round-trips through the struct's JSON tags, so it stays in sync with
// printJSON's field naming for free.
func printYAML(w io.Writer, obj any) error {
	b, err := yaml.Marshal(obj)
	if err != nil {
		return fmt.Errorf("encoding YAML: %w", err)
	}
	_, err = w.Write(b)
	return err
}

// activationIO adapts a genericclioptions.IOStreams to activation.IOStreams.
// The two types carry the same three streams under different field names
// (ErrOut vs Err) because activation predates this package and was ported
// in as-is rather than adopting cli-runtime's naming.
func activationIO(s genericclioptions.IOStreams) activation.IOStreams {
	return activation.IOStreams{In: s.In, Out: s.Out, Err: s.ErrOut}
}
