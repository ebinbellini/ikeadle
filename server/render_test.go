package server

import "testing"

func TestTemplatesLoad(t *testing.T) {
	if err := LoadTemplates(); err != nil {
		t.Fatalf("LoadTemplates: %v", err)
	}
}
