package validate

import "testing"

func TestCheckDay(t *testing.T) {
	if _, err := CheckDay("2026-09-20"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"20-09-2026", "2026-02-30", "", "2026-13-01"} {
		if _, err := CheckDay(bad); err == nil {
			t.Fatalf("expected error for %q", bad)
		}
	}
}

func TestCheckMacros(t *testing.T) {
	good := map[string]any{"kcal": 100.0, "protein": 5.0, "carbs": 10.0, "fat": 2.0, "fiber": 1.0}
	if err := CheckMacros(good, "item"); err != nil {
		t.Fatal(err)
	}
	if err := CheckMacros(map[string]any{"kcal": 1}, "item"); err == nil {
		t.Fatal("expected missing error")
	}
	if err := CheckMacros(map[string]any{
		"kcal": "200", "protein": 5.0, "carbs": 10.0, "fat": 2.0, "fiber": 1.0}, "item"); err != nil {
		t.Fatalf("string number rejected: %v", err)
	}
}

func TestToFloat(t *testing.T) {
	for in, want := range map[any]float64{"200": 200, 200: 200, 1.5: 1.5} {
		f, err := ToFloat(in)
		if err != nil || f != want {
			t.Fatalf("%v: got %v %v", in, f, err)
		}
	}
	if _, err := ToFloat("abc"); err == nil {
		t.Fatal("expected error")
	}
}

func TestSumTotals(t *testing.T) {
	tot := SumTotals([]map[string]any{
		{"kcal": 100.0, "protein": 5.0, "carbs": 10.0, "fat": 2.0, "fiber": 1.0},
		{"kcal": 50.0, "protein": 1.0, "carbs": 5.0, "fat": 0.0, "fiber": 0.0},
	})
	if tot["kcal"] != 150 || tot["protein"] != 6 {
		t.Fatalf("got %v", tot)
	}
}
