package electrum

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"testing"
)

// fakeServer answers a few Electrum methods with canned, synthetic data.
func fakeServer(t *testing.T) (string, int) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		r := bufio.NewReader(conn)
		for {
			line, err := r.ReadBytes('\n')
			if err != nil {
				return
			}
			var req request
			json.Unmarshal(line, &req)
			var result any
			switch req.Method {
			case "server.version":
				result = []string{"Fulcrum 1.11", "1.4"}
			case "blockchain.scripthash.get_history":
				// a notification first, to check it is skipped
				conn.Write([]byte(`{"jsonrpc":"2.0","method":"blockchain.headers.subscribe","params":[{"height":1}]}` + "\n"))
				result = []HistoryItem{{TxHash: "aa", Height: 100}}
			case "blockchain.scripthash.get_balance":
				result = Balance{Confirmed: 5000}
			}
			b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
			conn.Write(append(b, '\n'))
		}
	}()
	a := ln.Addr().(*net.TCPAddr)
	return a.IP.String(), a.Port
}

func TestClient(t *testing.T) {
	host, port := fakeServer(t)
	c, err := Dial(context.Background(), Config{Host: host, Port: port})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	h, err := c.History("00")
	if err != nil || len(h) != 1 || h[0].Height != 100 {
		t.Fatalf("history %v %v", h, err)
	}
	b, err := c.Balance("00")
	if err != nil || b.Confirmed != 5000 {
		t.Fatalf("balance %v %v", b, err)
	}
}
