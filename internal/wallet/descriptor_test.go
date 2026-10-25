package wallet

import (
	"fmt"

	"github.com/btcsuite/btcd/btcutil/base58"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"os"
	"strings"
	"testing"
)

// testdata/core_descriptors.txt was generated on a throwaway regtest node:
// each line is "descriptor|receive0,receive1,change0" as Bitcoin Core's
// deriveaddresses computed them. The keys belong to no real wallet.
// Testdata files may have CRLF endings on Windows checkouts; lines are
// split after normalising them.
func TestDescriptorsMatchBitcoinCore(t *testing.T) {
	b, err := os.ReadFile("testdata/core_descriptors.txt")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(strings.ReplaceAll(string(b), "\r\n", "\n")), "\n") {
		desc, want, _ := strings.Cut(line, "|")
		w := strings.Split(want, ",")
		d, err := Parse(desc, "", true)
		if err != nil {
			t.Errorf("%s: %v", trunc(desc), err)
			continue
		}
		got := []string{}
		for _, ci := range [][2]uint32{{0, 0}, {0, 1}, {1, 0}} {
			a, err := d.Derive(ci[0], ci[1])
			if err != nil {
				t.Fatalf("%s: %v", trunc(desc), err)
			}
			got = append(got, a.Address)
		}
		if strings.Join(got, ",") != strings.Join(w, ",") {
			t.Errorf("%s\n got %v\nwant %v", desc[:16], got, w)
		}
	}
}

func TestDescriptorChecksum(t *testing.T) {
	// Example from BIP 380.
	got, _ := DescriptorChecksum("raw(deadbeef)")
	if got != "89f8spxm" {
		t.Errorf("checksum %s", got)
	}
	if _, err := ParseDescriptor("wpkh(tpubDDWidpKv7s9bR8jx7qMzRQjEbz1phHWcCN2tKSkyxY8MCqpx14uDBcehjrD6ks8eGLpxp6DoEauFeigy5dPnLBZEmErjdqssA7yLfsgDnn7/0/*)#aaaaaaaa", true); err == nil {
		t.Error("bad checksum accepted")
	}
}

func TestDescriptorErrors(t *testing.T) {
	k := "tpubDDWidpKv7s9bR8jx7qMzRQjEbz1phHWcCN2tKSkyxY8MCqpx14uDBcehjrD6ks8eGLpxp6DoEauFeigy5dPnLBZEmErjdqssA7yLfsgDnn7"
	for _, d := range []string{
		"wpkh(" + k + ")",             // no wildcard
		"wpkh(" + k + "/0'/*)",        // hardened after xpub
		"wsh(multi(3," + k + "/0/*))", // threshold too high
		"wsh(and_v(v:pk(" + k + "/0/*),older(10)))",
		"tr(" + k + "/0/*,pk(" + k + "/1/*))",
	} {
		if _, err := ParseDescriptor(d, true); err == nil {
			t.Errorf("accepted %s", trunc(d))
		}
	}
}

// reversion re-encodes an extended key with other SLIP-132 version bytes.
func reversion(t *testing.T, key string, ver [4]byte) string {
	t.Helper()
	raw := base58.Decode(key)
	payload := append([]byte(nil), raw[:78]...)
	copy(payload, ver[:])
	return base58.Encode(append(payload, chainhash.DoubleHashB(payload)[:4]...))
}

// A multisig wallet built from cosigner keys derives the same addresses as
// the equivalent descriptor in Bitcoin Core's vectors, whether the keys are
// written as tpubs or as Vpubs (the testnet form of Zpub).
func TestMultisigFromCosignerKeys(t *testing.T) {
	b, _ := os.ReadFile("testdata/core_descriptors.txt")
	var desc, want string
	for _, line := range strings.Split(strings.TrimSpace(strings.ReplaceAll(string(b), "\r\n", "\n")), "\n") {
		if strings.HasPrefix(line, "wsh(sortedmulti(") {
			desc, want, _ = strings.Cut(line, "|")
			break
		}
	}
	if desc == "" {
		t.Skip("no wsh(sortedmulti) vector")
	}
	inner := desc[len("wsh(sortedmulti("):strings.Index(desc, "))")]
	parts := strings.Split(inner, ",")
	k := parts[0]
	var keys, vkeys []string
	for _, p := range parts[1:] {
		keys = append(keys, p)
		origin, rest := "", p
		if i := strings.IndexByte(p, ']'); i >= 0 {
			origin, rest = p[:i+1], p[i+1:]
		}
		key, path, _ := strings.Cut(rest, "/")
		vkeys = append(vkeys, origin+reversion(t, key, [4]byte{0x02, 0x57, 0x54, 0x83})+"/"+path)
	}
	th := 0
	fmt.Sscan(k, &th)
	for _, set := range [][]string{keys, vkeys} {
		ms, err := MultisigDescriptor(th, set, "p2wsh", true)
		if err != nil {
			t.Fatal(err)
		}
		d, err := Parse(ms, "", true)
		if err != nil {
			t.Fatal(err)
		}
		a0, _ := d.Derive(0, 0)
		if !strings.HasPrefix(want, a0.Address) {
			t.Errorf("got %s, want first of %s", a0.Address, want)
		}
	}
	lone, _, _ := strings.Cut(vkeys[0][strings.IndexByte(vkeys[0], ']')+1:], "/")
	if _, err := Parse(lone, "", true); err != ErrMultisig {
		t.Errorf("a lone cosigner key should explain multisig, got %v", err)
	}
	if _, err := MultisigDescriptor(3, keys[:2], "", true); err == nil {
		t.Error("3-of-2 accepted")
	}
}
