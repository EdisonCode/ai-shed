package risk

import (
	"slices"
	"testing"

	"github.com/edisoncode/ai-shed/internal/config"
)

var billing = config.Verify{Areas: []config.Area{{Name: "billing", Paths: []string{"internal/billing/", "migrations"}}}, HighLines: 300}

func TestTiers(t *testing.T) {
	for _, tc := range []struct {
		name    string
		diff    Diff
		tier    string
		reasons []string
	}{
		{"small with tests", Diff{Files: []string{"internal/export/csv.go", "internal/export/csv_test.go"}, Lines: 40}, config.TierLow, nil},
		{"docs only", Diff{Files: []string{"README.md", "docs/setup.md"}, Lines: 500}, config.TierLow, nil},
		{"in a high area", Diff{Files: []string{"internal/billing/refund.go", "internal/billing/refund_test.go"}, Lines: 20}, config.TierHigh, []string{"touches billing"}},
		{"a migration", Diff{Files: []string{"migrations/0042_add_column.sql"}, Lines: 5}, config.TierHigh, []string{"touches billing", "code changed and no test did"}},
		{"large", Diff{Files: []string{"internal/export/csv.go", "internal/export/csv_test.go"}, Lines: 301}, config.TierHigh, []string{"301 lines changed"}},
		{"code with no test", Diff{Files: []string{"internal/export/csv.go"}, Lines: 10}, config.TierHigh, []string{"code changed and no test did"}},
		{"a sibling of an area", Diff{Files: []string{"internal/billingx/a.go", "internal/billingx/a_test.go"}, Lines: 10}, config.TierLow, nil},
		{"a spec file counts as a test", Diff{Files: []string{"web/src/cart.ts", "web/src/cart.spec.ts"}, Lines: 10}, config.TierLow, nil},
	} {
		tier, reasons := Tier(billing, tc.diff)
		if tier != tc.tier || !slices.Equal(reasons, tc.reasons) {
			t.Errorf("%s: tier %s %q, want %s %q", tc.name, tier, reasons, tc.tier, tc.reasons)
		}
	}
}

func TestParseDiffReadsTheFilesAndCountsTheLines(t *testing.T) {
	d, err := parseDiff([]byte(`{"additions":30,"deletions":12,"files":[{"path":"a.go"},{"path":"a_test.go"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if d.Lines != 42 || !slices.Equal(d.Files, []string{"a.go", "a_test.go"}) {
		t.Fatalf("diff = %+v", d)
	}
}
