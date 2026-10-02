package mongostore

import "testing"

// Pure unit tests — no database, so these run in CI (#171).
func TestToInt(t *testing.T) {
	for _, tc := range []struct {
		in   any
		want int
		ok   bool
	}{
		{3, 3, true},
		{int32(3), 3, true}, // what the driver returns for int32 fields
		{int64(3), 3, true},
		{3.0, 3, true},
		{3.9, 3, true}, // truncates; reject fractionals upstream
		{"3", 0, false},
		{nil, 0, false},
		{true, 0, false},
	} {
		if got, ok := ToInt(tc.in); got != tc.want || ok != tc.ok {
			t.Errorf("ToInt(%v) = %d,%v want %d,%v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestToBool(t *testing.T) {
	if b, ok := ToBool(true); !ok || !b {
		t.Error("ToBool(true)")
	}
	if _, ok := ToBool("true"); ok {
		t.Error("ToBool(\"true\") must fail: strings are caller bugs")
	}
	if _, ok := ToBool(1); ok {
		t.Error("ToBool(1) must fail")
	}
}

func TestClampLimit(t *testing.T) {
	if got := ClampLimit(0, 200, 500); got != 200 {
		t.Errorf("default: %d", got)
	}
	if got := ClampLimit(-5, 200, 500); got != 200 {
		t.Errorf("negative: %d", got)
	}
	if got := ClampLimit(1<<30, 200, 500); got != 500 {
		t.Errorf("ceiling: %d", got)
	}
	if got := ClampLimit(50, 200, 500); got != 50 {
		t.Errorf("passthrough: %d", got)
	}
}

func TestOpenRequiresURI(t *testing.T) {
	if _, _, err := Open("  ", "db"); err == nil {
		t.Error("blank URI must fail")
	}
}
