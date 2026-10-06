package wallet

import (
	"os"
	"strings"
	"testing"
)

// testdata/core_descriptors.txt was generated on a throwaway regtest node:
// each line is "descriptor|receive0,receive1,change0" as Bitcoin Core's
// deriveaddresses computed them. The keys belong to no real wallet.
func TestDescriptorsMatchBitcoinCore(t *testing.T) {
	b, err := os.ReadFile("testdata/core_descriptors.txt")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
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
