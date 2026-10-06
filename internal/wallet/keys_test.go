package wallet

import "testing"

// Public test vectors from BIP44/49/84/86 for the mnemonic
// "abandon abandon ... about". No real wallet data.
func TestDeriveVectors(t *testing.T) {
	cases := []struct {
		name, key string
		override  ScriptType
		chain     uint32
		want      string
	}{
		{"bip44", "xpub6BosfCnifzxcFwrSzQiqu2DBVTshkCXacvNsWGYJVVhhawA7d4R5WSWGFNbi8Aw6ZRc1brxMyWMzG3DSSSSoekkudhUd9yLb6qx39T9nMdj", "", 0, "1LqBGSKuX5yYUonjxT5qGfpUsXKYYWeabA"},
		{"bip49", "ypub6Ww3ibxVfGzLrAH1PNcjyAWenMTbbAosGNB6VvmSEgytSER9azLDWCxoJwW7Ke7icmizBMXrzBx9979FfaHxHcrArf3zbeJJJUZPf663zsP", "", 0, "37VucYSaXLCAsxYyAPfbSi9eh4iEcbShgf"},
		{"bip84", "zpub6rFR7y4Q2AijBEqTUquhVz398htDFrtymD9xYYfG1m4wAcvPhXNfE3EfH1r1ADqtfSdVCToUG868RvUUkgDKf31mGDtKsAYz2oz2AGutZYs", "", 0, "bc1qcr8te4kr609gcawutmrza0j4xv80jy8z306fyu"},
		{"bip84-change", "zpub6rFR7y4Q2AijBEqTUquhVz398htDFrtymD9xYYfG1m4wAcvPhXNfE3EfH1r1ADqtfSdVCToUG868RvUUkgDKf31mGDtKsAYz2oz2AGutZYs", "", 1, "bc1q8c6fshw2dlwun7ekn9qwf37cu2rn755upcp6el"},
		{"bip86", "xpub6BgBgsespWvERF3LHQu6CnqdvfEvtMcQjYrcRzx53QJjSxarj2afYWcLteoGVky7D3UKDP9QyrLprQ3VCECoY49yfdDEHGCtMMj92pReUsQ", P2TR, 0, "bc1p5cyxnuxmeuwuvkwfem96lqzszd02n6xdcjrs20cac6yqjjwudpxqkedrcr"},
	}
	for _, c := range cases {
		a, err := ParseExtendedKey(c.key, c.override, false)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		got, err := a.Derive(c.chain, 0)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got.Address != c.want {
			t.Errorf("%s: got %s want %s", c.name, got.Address, c.want)
		}
		if len(got.ScriptHash) != 64 {
			t.Errorf("%s: bad scripthash %q", c.name, got.ScriptHash)
		}
	}
}

func TestRejectsGarbage(t *testing.T) {
	for _, s := range []string{"", "xpubnope", "1LqBGSKuX5yYUonjxT5qGfpUsXKYYWeabA"} {
		if _, err := ParseExtendedKey(s, "", false); err == nil {
			t.Errorf("expected error for %q", s)
		}
	}
}
