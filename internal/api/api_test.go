package api

import (
	"bytes"
	"testing"
)

func FuzzRelayDecode(f *testing.F) {
	f.Add([]byte(`{"host":"smtp.example.com","port":587,"tls":"starttls","username":"","from_address":"alerts@example.com"}`))
	f.Add([]byte(`{"password":null}`))
	f.Add([]byte(`{"password":0}`))
	f.Fuzz(func(t *testing.T, input []byte) {
		if len(input) > 64<<10 {
			t.Skip()
		}
		relay, password, err := decodeRelayInput(bytes.NewReader(input))
		if err != nil {
			return
		}
		if password != nil {
			relay.Password = *password
		}
		_ = relay.Validate()
	})
}
