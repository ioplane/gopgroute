//go:build linux

package app

import (
	"bytes"
	"encoding/json"
	"testing"
)

func FuzzConfig(f *testing.F) {
	for _, seed := range [][]byte{[]byte(example), []byte("null"), []byte(`{"asn":1,"asn":2}`), {0xff}} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input []byte) {
		cfg, err := Decode(bytes.NewReader(input))
		if err != nil {
			return
		}
		data, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Decode(bytes.NewReader(data)); err != nil {
			t.Fatalf("accepted config did not round-trip: %v", err)
		}
	})
}
