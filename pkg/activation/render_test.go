package activation

import (
	"bytes"
	"strings"
	"testing"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

func TestRenderList(t *testing.T) {
	entries := []CatalogEntry{
		{
			Service: ServiceInfo{
				ObjectName:    "ai-assistant",
				CanonicalName: "ai-assistant.datumapis.com",
				DisplayName:   "AI Assistant",
			},
			State: StateNotRequested,
		},
		{
			Service: ServiceInfo{
				ObjectName:    "compute",
				CanonicalName: "compute.datumapis.com",
				DisplayName:   "Compute",
			},
			State: StateActive,
		},
	}

	t.Run("every row carries the name enable accepts", func(t *testing.T) {
		var buf bytes.Buffer
		RenderList(&buf, entries)
		lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")

		if !strings.Contains(lines[0], "SERVICE NAME") {
			t.Fatalf("header %q is missing the SERVICE NAME column", lines[0])
		}
		for _, entry := range entries {
			row := rowFor(t, lines, entry.Service.DisplayName)
			if !strings.Contains(row, entry.Service.CanonicalName) {
				t.Errorf("row %q does not carry canonical name %q", row, entry.Service.CanonicalName)
			}
			// The canonical name must resolve through the same matcher
			// `services enable` uses, or the column is a dead end.
			if _, err := FindService(publishedList(entries), entry.Service.CanonicalName); err != nil {
				t.Errorf("FindService(%q) = %v, want it to resolve", entry.Service.CanonicalName, err)
			}
		}
	})

	t.Run("hint names the column to copy from", func(t *testing.T) {
		var buf bytes.Buffer
		RenderList(&buf, entries)
		if !strings.Contains(buf.String(), "datumctl services enable <SERVICE NAME>") {
			t.Fatalf("output has no enable hint:\n%s", buf.String())
		}
	})

	t.Run("empty catalog prints no hint", func(t *testing.T) {
		var buf bytes.Buffer
		RenderList(&buf, nil)
		if strings.Contains(buf.String(), "services enable") {
			t.Fatalf("empty catalog should not suggest enabling anything:\n%s", buf.String())
		}
	})
}

// publishedList rebuilds the catalog the entries were rendered from, so a test
// can check that what the table prints is what the enable verb resolves.
func publishedList(entries []CatalogEntry) *servicesv1alpha1.ServiceList {
	list := &servicesv1alpha1.ServiceList{}
	for _, entry := range entries {
		list.Items = append(list.Items, *service(
			entry.Service.ObjectName,
			entry.Service.CanonicalName,
			entry.Service.DisplayName,
			servicesv1alpha1.PhasePublished,
		))
	}
	return list
}

// rowFor returns the single table row whose first cell is display, failing the
// test when the row is absent.
func rowFor(t *testing.T, lines []string, display string) string {
	t.Helper()
	for _, line := range lines[1:] {
		if strings.HasPrefix(line, display) {
			return line
		}
	}
	t.Fatalf("no row for %q in:\n%s", display, strings.Join(lines, "\n"))
	return ""
}
