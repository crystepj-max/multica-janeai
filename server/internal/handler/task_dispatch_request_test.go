package handler

import (
	"strings"
	"testing"
)

func TestDecodeIssueDispatchExpectedRevision(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		want    int64
		wantErr bool
	}{
		{name: "positive revision", body: `{"expected_revision":7}`, want: 7},
		{name: "empty body", body: "", wantErr: true},
		{name: "missing revision", body: `{}`, wantErr: true},
		{name: "zero revision", body: `{"expected_revision":0}`, wantErr: true},
		{name: "negative revision", body: `{"expected_revision":-1}`, wantErr: true},
		{name: "wrong type", body: `{"expected_revision":"7"}`, wantErr: true},
		{name: "unknown field", body: `{"expected_revision":7,"force":true}`, wantErr: true},
		{name: "trailing document", body: `{"expected_revision":7} {}`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := decodeIssueDispatchExpectedRevision(strings.NewReader(tt.body))
			if (err != nil) != tt.wantErr {
				t.Fatalf("decodeIssueDispatchExpectedRevision() error = %v, wantErr %t", err, tt.wantErr)
			}
			if err == nil && got != tt.want {
				t.Fatalf("revision = %d, want %d", got, tt.want)
			}
		})
	}
}
