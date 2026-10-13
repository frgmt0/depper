package exitcode

import (
	"errors"
	"fmt"
	"testing"
)

func TestCode(t *testing.T) {
	base := errors.New("boom")
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"nil", nil, OK},
		{"plain", base, Failure},
		{"unhealthy", New(Unhealthy, base), Unhealthy},
		{"wrapped drift", fmt.Errorf("context: %w", New(Drift, base)), Drift},
		{"silent", Silent(Unhealthy), Unhealthy},
	}
	for _, tc := range cases {
		if got := Code(tc.err); got != tc.want {
			t.Errorf("%s: Code = %d, want %d", tc.name, got, tc.want)
		}
	}
	if Silent(Drift).Error() != "" {
		t.Error("silent error must have an empty message")
	}
	if !errors.Is(New(Drift, base), base) {
		t.Error("New must wrap the original error")
	}
}
