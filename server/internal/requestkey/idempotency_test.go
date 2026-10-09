package requestkey

import (
	"strings"
	"testing"
)

func TestParseIdempotencyKey(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    string
		wantErr bool
	}{
		{name: "missing", wantErr: true},
		{name: "blank", value: " \t", wantErr: true},
		{name: "trimmed", value: "  batch-1/task-2  ", want: "batch-1/task-2"},
		{name: "maximum bytes", value: strings.Repeat("k", 255), want: strings.Repeat("k", 255)},
		{name: "over maximum bytes", value: strings.Repeat("k", 256), wantErr: true},
		{name: "multibyte maximum bytes", value: strings.Repeat("界", 85), want: strings.Repeat("界", 85)},
		{name: "multibyte over maximum bytes", value: strings.Repeat("界", 86), wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseIdempotencyKey(tt.value)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseIdempotencyKey() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil && got != tt.want {
				t.Fatalf("ParseIdempotencyKey() = %q, want %q", got, tt.want)
			}
		})
	}
}
