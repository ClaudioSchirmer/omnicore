package queries

import (
	"reflect"
	"sort"
	"testing"
)

func collectExportGoPaths(p *ExportPlan) []string {
	var out []string
	var walk func(n *ExportNode, prefix string)
	walk = func(n *ExportNode, prefix string) {
		for _, c := range n.Columns {
			out = append(out, joinExportPath(prefix, c.GoField))
		}
		for _, ch := range n.Children {
			walk(ch, joinExportPath(prefix, ch.GoSegment))
		}
	}
	if p != nil && p.Root != nil {
		walk(p.Root, "")
	}
	sort.Strings(out)
	return out
}

func TestExportPlan_PruneToProjection(t *testing.T) {
	all := []string{"Addresses.Geo.Lat", "Addresses.Street", "Addresses.ZipCode", "Email", "Name"}
	cases := []struct {
		name string
		proj map[string]int
		want []string
	}{
		{"nil → whole doc", nil, all},
		{"empty → whole doc", map[string]int{}, all},
		{"_id:0 only → whole doc", map[string]int{"_id": 0}, all},
		{"include leaf", map[string]int{"Name": 1}, []string{"Name"}},
		{"include leaf + nested leaf", map[string]int{"Name": 1, "Addresses.ZipCode": 1}, []string{"Addresses.ZipCode", "Name"}},
		{"include subtree keeps whole subtree", map[string]int{"Addresses": 1}, []string{"Addresses.Geo.Lat", "Addresses.Street", "Addresses.ZipCode"}},
		{"exclude leaf", map[string]int{"Email": 0}, []string{"Addresses.Geo.Lat", "Addresses.Street", "Addresses.ZipCode", "Name"}},
		{"exclude nested leaf", map[string]int{"Addresses.Street": 0}, []string{"Addresses.Geo.Lat", "Addresses.ZipCode", "Email", "Name"}},
		{"exclude subtree drops whole subtree", map[string]int{"Addresses": 0}, []string{"Email", "Name"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := collectExportGoPaths(sampleExportPlan().PruneToProjection(tc.proj))
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("PruneToProjection(%v) = %v, want %v", tc.proj, got, tc.want)
			}
		})
	}
}

func sampleExportPlan() *ExportPlan {
	return &ExportPlan{Root: &ExportNode{
		Columns: []ExportColumn{
			{GoField: "Name", WireLeaf: "name", LabelKey: "UserNameField"},
			{GoField: "Email", WireLeaf: "email", LabelKey: "UserEmailField"},
		},
		Children: []*ExportNode{{
			GoSegment: "Addresses", WireSegment: "addresses",
			Columns: []ExportColumn{
				{GoField: "Street", WireLeaf: "street"},
				{GoField: "ZipCode", WireLeaf: "zipCode", LabelKey: "AddressZipCodeField"},
			},
			Children: []*ExportNode{{
				GoSegment: "Geo", WireSegment: "geo",
				Columns: []ExportColumn{{GoField: "Lat", WireLeaf: "lat"}},
			}},
		}},
	}}
}

func TestExportPlan_WireToGoPaths(t *testing.T) {
	got := sampleExportPlan().WireToGoPaths()
	want := map[string]string{
		"name":              "Name",
		"email":             "Email",
		"addresses":         "Addresses",
		"addresses.street":  "Addresses.Street",
		"addresses.zipCode": "Addresses.ZipCode",
		"addresses.geo":     "Addresses.Geo",
		"addresses.geo.lat": "Addresses.Geo.Lat",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("WireToGoPaths mismatch:\n got %v\nwant %v", got, want)
	}
}

func TestExportPlan_Validate(t *testing.T) {
	p := sampleExportPlan()
	if bad, ok := p.Validate([]string{"name", "addresses.zipCode", "addresses"}); !ok {
		t.Fatalf("expected valid, got bad=%q", bad)
	}
	if bad, ok := p.Validate([]string{"name", "bogus"}); ok || bad != "bogus" {
		t.Fatalf("expected bad=bogus ok=false, got bad=%q ok=%v", bad, ok)
	}
}

func TestExportPlan_Projection(t *testing.T) {
	got := sampleExportPlan().Projection([]string{"name", "addresses.zipCode", "addresses"})
	want := map[string]int{"Name": 1, "Addresses.ZipCode": 1, "Addresses": 1}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Projection mismatch: got %v want %v", got, want)
	}
}

func TestExportPlan_PruneLeaf(t *testing.T) {
	root := sampleExportPlan().Prune([]string{"name", "addresses.zipCode"}).Root
	if len(root.Columns) != 1 || root.Columns[0].GoField != "Name" {
		t.Fatalf("expected only Name at root, got %+v", root.Columns)
	}
	if len(root.Children) != 1 {
		t.Fatalf("expected addresses child kept, got %d", len(root.Children))
	}
	addr := root.Children[0]
	if len(addr.Columns) != 1 || addr.Columns[0].GoField != "ZipCode" {
		t.Fatalf("expected only ZipCode under addresses, got %+v", addr.Columns)
	}
	if len(addr.Children) != 0 {
		t.Fatalf("expected geo grandchild pruned, got %d", len(addr.Children))
	}
}

func TestExportPlan_PruneSubtree(t *testing.T) {
	root := sampleExportPlan().Prune([]string{"addresses"}).Root
	if len(root.Columns) != 0 {
		t.Fatalf("expected no root columns, got %+v", root.Columns)
	}
	addr := root.Children[0]
	if len(addr.Columns) != 2 {
		t.Fatalf("expected whole addresses subtree (2 cols), got %d", len(addr.Columns))
	}
	if len(addr.Children) != 1 || len(addr.Children[0].Columns) != 1 {
		t.Fatalf("expected geo grandchild kept whole, got %+v", addr.Children)
	}
}

func TestExportPlan_PruneEmptyReturnsAll(t *testing.T) {
	p := sampleExportPlan()
	if p.Prune(nil) != p {
		t.Fatal("empty prune should return the same plan unchanged")
	}
}

func TestExportPlan_WireToGoPaths_NilPlanAndNilRoot(t *testing.T) {
	var nilPlan *ExportPlan
	if got := nilPlan.WireToGoPaths(); len(got) != 0 {
		t.Fatalf("nil plan should yield empty map, got %v", got)
	}
	rootless := &ExportPlan{}
	if got := rootless.WireToGoPaths(); len(got) != 0 {
		t.Fatalf("plan with nil Root should yield empty map, got %v", got)
	}
}

func TestJoinExportPath(t *testing.T) {
	if got := joinExportPath("", "name"); got != "name" {
		t.Fatalf("empty prefix should return the segment, got %q", got)
	}
	if got := joinExportPath("addresses", ""); got != "addresses" {
		t.Fatalf("empty segment should return the prefix, got %q", got)
	}
	if got := joinExportPath("addresses", "zipCode"); got != "addresses.zipCode" {
		t.Fatalf("expected dotted join, got %q", got)
	}
}

func TestSplitFields(t *testing.T) {
	got := SplitFields(" name , addresses.zipCode ,, ")
	want := []string{"name", "addresses.zipCode"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SplitFields got %v want %v", got, want)
	}
	if SplitFields("") != nil {
		t.Fatal("empty input should yield nil")
	}
}
