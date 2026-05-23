package rpc

import "testing"

func TestHttpToWebsocket(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"http://structsd:26657", "ws://structsd:26657/websocket", false},
		{"http://structsd:26657/", "ws://structsd:26657/websocket", false},
		{"https://public.testnet.structs.network:26657", "wss://public.testnet.structs.network:26657/websocket", false},
		{"https://public.testnet.structs.network:26657/", "wss://public.testnet.structs.network:26657/websocket", false},
		{"ws://localhost:26657/websocket", "ws://localhost:26657/websocket", false},
		{"wss://localhost:26657/websocket", "wss://localhost:26657/websocket", false},
		{"ftp://nope", "", true},
		{"://garbage", "", true},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got, err := httpToWebsocket(c.in)
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.want {
				t.Fatalf("got %q want %q", got, c.want)
			}
		})
	}
}

func TestParseNewBlockHeight(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want int64
		ok   bool
	}{
		{
			name: "valid NewBlock",
			raw: `{"jsonrpc":"2.0","id":0,"result":{"query":"tm.event='NewBlock'",
				"data":{"type":"tendermint/event/NewBlock",
				"value":{"block":{"header":{"height":"42","chain_id":"test"}}}}}}`,
			want: 42, ok: true,
		},
		{
			name: "subscription ack (empty result)",
			raw:  `{"jsonrpc":"2.0","id":1,"result":{}}`,
			ok:   false,
		},
		{
			name: "wrong event type",
			raw: `{"jsonrpc":"2.0","id":0,"result":{
				"data":{"type":"tendermint/event/Tx","value":{}}}}`,
			ok: false,
		},
		{
			name: "garbage",
			raw:  `not json`,
			ok:   false,
		},
		{
			name: "height=0",
			raw: `{"result":{"data":{"type":"tendermint/event/NewBlock",
				"value":{"block":{"header":{"height":"0"}}}}}}`,
			ok: false,
		},
		{
			name: "non-numeric height",
			raw: `{"result":{"data":{"type":"tendermint/event/NewBlock",
				"value":{"block":{"header":{"height":"abc"}}}}}}`,
			ok: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := parseNewBlockHeight([]byte(c.raw))
			if ok != c.ok {
				t.Fatalf("ok: got %v want %v", ok, c.ok)
			}
			if got != c.want {
				t.Fatalf("height: got %d want %d", got, c.want)
			}
		})
	}
}

func TestDedupeURLs(t *testing.T) {
	in := []string{
		"http://a:26657",
		"http://a:26657/",
		"  ",
		"http://b:26657",
		"http://a:26657",
	}
	got := dedupeURLs(in)
	if len(got) != 2 || got[0] != "http://a:26657" || got[1] != "http://b:26657" {
		t.Fatalf("unexpected dedupe result: %v", got)
	}
}
