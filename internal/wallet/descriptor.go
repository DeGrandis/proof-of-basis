package wallet

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/hdkeychain"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
)

// Deriver produces the address at (chain, index): chain 0 is receive,
// chain 1 is change. Both extended keys and descriptors implement it.
type Deriver interface {
	Derive(chain, index uint32) (Address, error)
	Chains() uint32
	Network() *chaincfg.Params
}

func (a *Account) Chains() uint32            { return 2 }
func (a *Account) Network() *chaincfg.Params { return a.Params }

// Parse accepts either an extended public key or an output descriptor.
func Parse(s string, override ScriptType, regtest bool) (Deriver, error) {
	s = strings.TrimSpace(s)
	if strings.Contains(s, "(") {
		return ParseDescriptor(s, regtest)
	}
	return ParseExtendedKey(s, override, regtest)
}

// Supported descriptor shapes (BIP 380-386):
//
//	pkh(K) wpkh(K) sh(wpkh(K)) tr(K)
//	wsh(multi|sortedmulti(k,K,...)) sh(wsh(multi|sortedmulti(...))) sh(multi|sortedmulti(...))
//
// where K is [fingerprint/origin]xpub/path with a /* wildcard, and the
// chain step is either <0;1> or a literal 0 (change is then assumed to be 1).
type Descriptor struct {
	Text   string
	kind   string // pkh wpkh sh-wpkh tr wsh-multi sh-wsh-multi sh-multi
	k      int
	sorted bool
	keys   []descKey
	params *chaincfg.Params
	chains uint32
}

type descKey struct {
	ext    *hdkeychain.ExtendedKey
	prefix []uint32 // fixed steps after the xpub
	multi  []uint32 // chain step choices; nil means none
}

func (d *Descriptor) Chains() uint32            { return d.chains }
func (d *Descriptor) Network() *chaincfg.Params { return d.params }

func ParseDescriptor(s string, regtest bool) (*Descriptor, error) {
	s = strings.Join(strings.Fields(s), "")
	body := s
	if i := strings.LastIndexByte(s, '#'); i >= 0 {
		body = s[:i]
		want, err := DescriptorChecksum(body)
		if err != nil {
			return nil, err
		}
		if s[i+1:] != want {
			return nil, fmt.Errorf("descriptor checksum mismatch (expected #%s): check for a copy/paste error", want)
		}
	}
	d := &Descriptor{Text: body}
	inner, err := d.unwrap(body)
	if err != nil {
		return nil, err
	}
	var keyStrs []string
	switch d.kind {
	case "pkh", "wpkh", "sh-wpkh", "tr":
		if strings.Contains(inner, ",") {
			return nil, errors.New("taproot script trees and multi-key single-sig are not supported; use a key-path-only tr(KEY) descriptor")
		}
		keyStrs = []string{inner}
	default:
		parts := strings.Split(inner, ",")
		if len(parts) < 2 {
			return nil, errors.New("multi() needs a threshold and at least one key")
		}
		d.k, err = strconv.Atoi(parts[0])
		if err != nil || d.k < 1 || d.k > len(parts)-1 {
			return nil, fmt.Errorf("bad multisig threshold %q", parts[0])
		}
		keyStrs = parts[1:]
		limit := 20
		if d.kind == "sh-multi" {
			limit = 15
		}
		if len(keyStrs) > limit {
			return nil, fmt.Errorf("too many keys for %s (max %d)", d.kind, limit)
		}
	}
	for _, ks := range keyStrs {
		k, err := parseDescKey(ks, regtest)
		if err != nil {
			return nil, err
		}
		if d.params == nil {
			d.params = k.params
		} else if d.params != k.params {
			return nil, errors.New("descriptor mixes mainnet and test keys")
		}
		d.keys = append(d.keys, k.descKey)
	}
	d.chains = 0
	for i, k := range d.keys {
		n := uint32(1)
		if k.multi != nil {
			n = uint32(len(k.multi))
		}
		if i == 0 {
			d.chains = n
		} else if n != d.chains {
			return nil, errors.New("all keys must have the same number of <a;b> path choices")
		}
	}
	return d, nil
}

// MultisigDescriptor builds a ranged multisig descriptor from cosigner keys
// (xpub, Zpub, Ypub..., optionally with [fingerprint/path] origin and their
// own /<0;1>/* suffix). script is p2wsh, p2sh-p2wsh or p2sh; empty picks
// from the first key's prefix (Ypub/Upub nested, otherwise native segwit).
func MultisigDescriptor(threshold int, keys []string, script string, sorted bool) (string, error) {
	var ks []string
	for _, k := range keys {
		k = strings.Join(strings.Fields(k), "")
		if k == "" {
			continue
		}
		bare := k
		if i := strings.IndexByte(bare, ']'); i >= 0 {
			bare = bare[i+1:]
		}
		if !strings.Contains(bare, "/") {
			k += "/<0;1>/*"
		}
		ks = append(ks, k)
	}
	if len(ks) < 2 {
		return "", errors.New("a multisig wallet needs at least two cosigner keys")
	}
	if threshold < 1 || threshold > len(ks) {
		return "", fmt.Errorf("signatures required must be between 1 and %d", len(ks))
	}
	if script == "" {
		script = "p2wsh"
		first := ks[0]
		if i := strings.IndexByte(first, ']'); i >= 0 {
			first = first[i+1:]
		}
		if strings.HasPrefix(first, "Ypub") || strings.HasPrefix(first, "Upub") {
			script = "p2sh-p2wsh"
		}
	}
	fn := "multi"
	if sorted {
		fn = "sortedmulti"
	}
	inner := fmt.Sprintf("%s(%d,%s)", fn, threshold, strings.Join(ks, ","))
	var body string
	switch script {
	case "p2wsh":
		body = "wsh(" + inner + ")"
	case "p2sh-p2wsh":
		body = "sh(wsh(" + inner + "))"
	case "p2sh":
		body = "sh(" + inner + ")"
	default:
		return "", fmt.Errorf("unknown multisig script type %q", script)
	}
	sum, err := DescriptorChecksum(body)
	if err != nil {
		return "", err
	}
	return body + "#" + sum, nil
}

func (d *Descriptor) unwrap(s string) (string, error) {
	strip := func(s, fn string) (string, bool) {
		if strings.HasPrefix(s, fn+"(") && strings.HasSuffix(s, ")") {
			return s[len(fn)+1 : len(s)-1], true
		}
		return s, false
	}
	multi := func(s string) (string, bool) {
		if in, ok := strip(s, "sortedmulti"); ok {
			d.sorted = true
			return in, true
		}
		return strip(s, "multi")
	}
	if in, ok := strip(s, "sh"); ok {
		if in2, ok := strip(in, "wpkh"); ok {
			d.kind = "sh-wpkh"
			return in2, nil
		}
		if in2, ok := strip(in, "wsh"); ok {
			if in3, ok := multi(in2); ok {
				d.kind = "sh-wsh-multi"
				return in3, nil
			}
			return "", errors.New("only multi/sortedmulti is supported inside sh(wsh(...))")
		}
		if in2, ok := multi(in); ok {
			d.kind = "sh-multi"
			return in2, nil
		}
		return "", errors.New("unsupported sh(...) descriptor")
	}
	if in, ok := strip(s, "wsh"); ok {
		if in2, ok := multi(in); ok {
			d.kind = "wsh-multi"
			return in2, nil
		}
		return "", errors.New("only multi/sortedmulti is supported inside wsh(...); miniscript policies are not yet supported")
	}
	for _, fn := range []string{"wpkh", "pkh", "tr"} {
		if in, ok := strip(s, fn); ok {
			d.kind = fn
			return in, nil
		}
	}
	return "", fmt.Errorf("unsupported descriptor %q", trunc(s))
}

func trunc(s string) string {
	if len(s) > 24 {
		return s[:24] + "…"
	}
	return s
}

type parsedKey struct {
	descKey
	params *chaincfg.Params
}

func parseDescKey(s string, regtest bool) (parsedKey, error) {
	if strings.HasPrefix(s, "[") {
		i := strings.IndexByte(s, ']')
		if i < 0 {
			return parsedKey{}, errors.New("unclosed key origin [")
		}
		s = s[i+1:] // origin info is informational only
	}
	parts := strings.Split(s, "/")
	acct, err := parseExtendedKey(parts[0], P2PKH, regtest, true)
	if err != nil {
		if strings.HasPrefix(parts[0], "xprv") || strings.HasPrefix(parts[0], "tprv") {
			return parsedKey{}, errors.New("descriptor contains a private key; export the public (watch-only) descriptor instead")
		}
		return parsedKey{}, err
	}
	k := parsedKey{descKey: descKey{ext: acct.key}, params: acct.Params}
	steps := parts[1:]
	if len(steps) == 0 || steps[len(steps)-1] != "*" {
		return parsedKey{}, errors.New("each key needs a /* wildcard (ranged descriptor)")
	}
	steps = steps[:len(steps)-1]
	for i, st := range steps {
		if strings.HasSuffix(st, "'") || strings.HasSuffix(st, "h") || strings.HasSuffix(st, "H") {
			return parsedKey{}, errors.New("hardened steps after an xpub cannot be derived; use the account-level xpub")
		}
		if strings.HasPrefix(st, "<") && strings.HasSuffix(st, ">") {
			if k.multi != nil {
				return parsedKey{}, errors.New("only one <a;b> step is allowed")
			}
			for _, c := range strings.Split(st[1:len(st)-1], ";") {
				n, err := strconv.ParseUint(c, 10, 31)
				if err != nil {
					return parsedKey{}, fmt.Errorf("bad path step %q", st)
				}
				k.multi = append(k.multi, uint32(n))
			}
			continue
		}
		n, err := strconv.ParseUint(st, 10, 31)
		if err != nil {
			return parsedKey{}, fmt.Errorf("bad path step %q", st)
		}
		// A literal receive step 0 just before /* implies change is 1.
		if i == len(steps)-1 && n == 0 && k.multi == nil {
			k.multi = []uint32{0, 1}
			continue
		}
		k.prefix = append(k.prefix, uint32(n))
	}
	return k, nil
}

func (k descKey) pub(chain, index uint32) (*btcec.PublicKey, error) {
	cur := k.ext
	steps := append([]uint32(nil), k.prefix...)
	if k.multi != nil {
		if int(chain) >= len(k.multi) {
			return nil, fmt.Errorf("descriptor has no chain %d", chain)
		}
		steps = append(steps, k.multi[chain])
	} else if chain != 0 {
		return nil, fmt.Errorf("descriptor has no chain %d", chain)
	}
	steps = append(steps, index)
	var err error
	for _, st := range steps {
		if cur, err = cur.Derive(st); err != nil {
			return nil, err
		}
	}
	return cur.ECPubKey()
}

func (d *Descriptor) Derive(chain, index uint32) (Address, error) {
	pubs := make([]*btcec.PublicKey, len(d.keys))
	for i, k := range d.keys {
		p, err := k.pub(chain, index)
		if err != nil {
			return Address{}, err
		}
		pubs[i] = p
	}
	var addr btcutil.Address
	var err error
	single := func(st ScriptType) error {
		a := &Account{Script: st, Params: d.params}
		addr, err = a.addressFor(pubs[0])
		return err
	}
	switch d.kind {
	case "pkh":
		err = single(P2PKH)
	case "wpkh":
		err = single(P2WPKH)
	case "sh-wpkh":
		err = single(P2SHP2WPKH)
	case "tr":
		err = single(P2TR)
	default:
		if d.sorted {
			sort.Slice(pubs, func(i, j int) bool {
				return bytes.Compare(pubs[i].SerializeCompressed(), pubs[j].SerializeCompressed()) < 0
			})
		}
		apks := make([]*btcutil.AddressPubKey, len(pubs))
		for i, p := range pubs {
			if apks[i], err = btcutil.NewAddressPubKey(p.SerializeCompressed(), d.params); err != nil {
				return Address{}, err
			}
		}
		script, err := txscript.MultiSigScript(apks, d.k)
		if err != nil {
			return Address{}, err
		}
		wsh := sha256.Sum256(script)
		switch d.kind {
		case "wsh-multi":
			addr, err = btcutil.NewAddressWitnessScriptHash(wsh[:], d.params)
		case "sh-wsh-multi":
			addr, err = btcutil.NewAddressScriptHash(append([]byte{0x00, 0x20}, wsh[:]...), d.params)
		case "sh-multi":
			addr, err = btcutil.NewAddressScriptHash(script, d.params)
		}
		if err != nil {
			return Address{}, err
		}
	}
	if err != nil {
		return Address{}, err
	}
	spk, err := txscript.PayToAddrScript(addr)
	if err != nil {
		return Address{}, err
	}
	return Address{chain, index, addr.EncodeAddress(), spk, ScriptHash(spk)}, nil
}

// Kind is a short human label for the descriptor's script.
func (d *Descriptor) Kind() string {
	if strings.HasSuffix(d.kind, "multi") {
		return fmt.Sprintf("%s %d-of-%d", d.kind, d.k, len(d.keys))
	}
	return d.kind
}

// DescriptorChecksum computes the BIP 380 checksum of a descriptor.
func DescriptorChecksum(desc string) (string, error) {
	const inputCharset = "0123456789()[],'/*abcdefgh@:$%{}IJKLMNOPQRSTUVWXYZ&+-.;<=>?!^_|~ijklmnopqrstuvwxyzABCDEFGH`#\"\\ "
	const checksumCharset = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"
	gen := [5]uint64{0xf5dee51989, 0xa9fdca3312, 0x1bab10e32d, 0x3706b1677a, 0x644d626ffd}
	c := uint64(1)
	poly := func(v uint64) {
		top := c >> 35
		c = (c&0x7ffffffff)<<5 ^ v
		for i := 0; i < 5; i++ {
			if (top>>i)&1 == 1 {
				c ^= gen[i]
			}
		}
	}
	var groups []uint64
	for _, ch := range desc {
		pos := strings.IndexRune(inputCharset, ch)
		if pos < 0 {
			return "", fmt.Errorf("invalid character %q in descriptor", ch)
		}
		poly(uint64(pos) & 31)
		groups = append(groups, uint64(pos)>>5)
		if len(groups) == 3 {
			poly(groups[0]*9 + groups[1]*3 + groups[2])
			groups = groups[:0]
		}
	}
	switch len(groups) {
	case 1:
		poly(groups[0])
	case 2:
		poly(groups[0]*3 + groups[1])
	}
	for i := 0; i < 8; i++ {
		poly(0)
	}
	c ^= 1
	out := make([]byte, 8)
	for i := 0; i < 8; i++ {
		out[i] = checksumCharset[(c>>(5*(7-i)))&31]
	}
	return string(out), nil
}
